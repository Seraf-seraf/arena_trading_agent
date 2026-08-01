// Package navigation строит безопасные пути по известным состояниям UI.
package navigation

import (
	"fmt"
	"strings"
	"time"

	"github.com/arena-trading-agent/arena-trading-agent/internal/domain"
	"github.com/arena-trading-agent/arena-trading-agent/internal/protocol"
)

// VerificationRule задаёт ожидаемый результат перехода. BBox зарезервирован
// для будущего ROI-verifier и до его сквозной реализации отклоняется.
type VerificationRule struct {
	State         domain.ScreenState
	MinConfidence float64
	Timeout       time.Duration
	BBox          *domain.Rectangle
}

// Transition описывает проверяемое ребро графа экранов.
type Transition struct {
	From     domain.ScreenState
	To       domain.ScreenState
	Action   protocol.Action
	Class    protocol.ActionClass
	Verify   VerificationRule
	MaxRetry int
}

// Path is a declarative, ordered UI route. It contains no runtime state: the
// executor validates every edge against a fresh observation before sending an
// action to Windows.
type Path []Transition

// Navigator хранит известный граф интерфейса.
type Navigator struct {
	edges map[domain.ScreenState][]Transition
}

// New создаёт навигатор и отклоняет небезопасные переходы.
func New(transitions []Transition) (*Navigator, error) {
	const methodCtx = "navigation.New"

	n := &Navigator{edges: make(map[domain.ScreenState][]Transition)}
	for _, transition := range transitions {
		transition.Class = normalizedClass(transition.Class)
		if transition.From == domain.StateUnknown || transition.To == domain.StateUnknown {
			return nil, fmt.Errorf("%s: переходы через UNKNOWN должны выполняться политикой восстановления", methodCtx)
		}
		if transition.Verify.State != transition.To || transition.MaxRetry < 0 {
			return nil, fmt.Errorf("%s: некорректная проверка перехода %s → %s", methodCtx, transition.From, transition.To)
		}
		switch transition.Class {
		case protocol.ActionNavigation, protocol.ActionPurchase, protocol.ActionBarter,
			protocol.ActionListing, protocol.ActionReprice:
		default:
			return nil, fmt.Errorf("%s: переход %s → %s содержит неизвестный класс %q", methodCtx, transition.From, transition.To, transition.Class)
		}
		if transition.Class != protocol.ActionNavigation && transition.MaxRetry != 0 {
			return nil, fmt.Errorf(
				"%s: денежный переход %s → %s не может автоматически повторяться",
				methodCtx,
				transition.From,
				transition.To,
			)
		}
		if transition.Class != protocol.ActionNavigation {
			if err := validateMonetaryAction(transition.Action); err != nil {
				return nil, fmt.Errorf(
					"%s: денежный переход %s → %s небезопасен: %w",
					methodCtx,
					transition.From,
					transition.To,
					err,
				)
			}
		}
		if transition.Verify.Timeout < 0 {
			return nil, fmt.Errorf(
				"%s: переход %s → %s содержит отрицательный срок проверки",
				methodCtx,
				transition.From,
				transition.To,
			)
		}
		if transition.Verify.BBox != nil {
			return nil, fmt.Errorf(
				"%s: переход %s → %s содержит неподдерживаемую область проверки; verification bbox должна быть пустой",
				methodCtx,
				transition.From,
				transition.To,
			)
		}
		n.edges[transition.From] = append(n.edges[transition.From], cloneTransition(transition))
	}
	return n, nil
}

func validateMonetaryAction(action protocol.Action) error {
	const methodCtx = "navigation.validateMonetaryAction"

	if strings.ToUpper(strings.TrimSpace(action.Kind)) != "CLICK" ||
		action.Point == nil ||
		!validPoint(*action.Point) ||
		len(action.Steps) != 0 {
		return fmt.Errorf(
			"%s: разрешён ровно один CLICK по нормализованной точке",
			methodCtx,
		)
	}
	button := strings.ToUpper(strings.TrimSpace(action.Value))
	if button != "LEFT" && button != "PRIMARY" {
		return fmt.Errorf(
			"%s: денежный CLICK требует кнопку LEFT или PRIMARY",
			methodCtx,
		)
	}
	return nil
}

// Path возвращает кратчайшую известную последовательность безопасных
// навигационных переходов. Денежные рёбра никогда не используются как
// сокращение обычного маршрута.
func (n *Navigator) Path(from, to domain.ScreenState) (Path, error) {
	const methodCtx = "navigation.Navigator.Path"

	path, err := n.PathForClass(from, to, protocol.ActionNavigation)
	if err != nil {
		return nil, fmt.Errorf("%s: не удалось построить навигационный путь: %w", methodCtx, err)
	}
	return path, nil
}

// PathForClass возвращает кратчайший путь, состоящий только из переходов
// заданного класса. Денежный класс должен запрашиваться явно владельцем
// торговой транзакции; Router и сканеры используют только ActionNavigation.
func (n *Navigator) PathForClass(
	from,
	to domain.ScreenState,
	class protocol.ActionClass,
) (Path, error) {
	const methodCtx = "navigation.Navigator.PathForClass"

	class = normalizedClass(class)
	switch class {
	case protocol.ActionNavigation, protocol.ActionPurchase, protocol.ActionBarter,
		protocol.ActionListing, protocol.ActionReprice:
	default:
		return nil, fmt.Errorf("%s: неизвестный класс действий %q", methodCtx, class)
	}
	if from == to {
		return Path{}, nil
	}
	type node struct {
		state domain.ScreenState
		path  Path
	}
	queue := []node{{state: from}}
	seen := map[domain.ScreenState]bool{from: true}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		for _, edge := range n.edges[current.state] {
			if normalizedClass(edge.Class) != class {
				continue
			}
			if seen[edge.To] {
				continue
			}
			path := append(clonePath(current.path), cloneTransition(edge))
			if edge.To == to {
				return path, nil
			}
			seen[edge.To] = true
			queue = append(queue, node{state: edge.To, path: path})
		}
	}
	return nil, fmt.Errorf(
		"%s: не найден путь класса %s из %s в %s",
		methodCtx,
		class,
		from,
		to,
	)
}

// PathForCommit builds a route with zero or more navigation edges followed by
// exactly one final edge of the requested monetary class. This prevents a
// scanner from using money-changing shortcuts and prevents TradeRunner from
// discovering the expected class only after input has already happened.
func (n *Navigator) PathForCommit(
	from,
	to domain.ScreenState,
	class protocol.ActionClass,
) (Path, error) {
	const methodCtx = "navigation.Navigator.PathForCommit"

	class = normalizedClass(class)
	switch class {
	case protocol.ActionPurchase, protocol.ActionBarter,
		protocol.ActionListing, protocol.ActionReprice:
	default:
		return nil, fmt.Errorf("%s: класс фиксации сделки %q не является денежным", methodCtx, class)
	}
	type node struct {
		state domain.ScreenState
		path  Path
	}
	queue := []node{{state: from}}
	seen := map[domain.ScreenState]bool{from: true}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		for _, edge := range n.edges[current.state] {
			edgeClass := normalizedClass(edge.Class)
			if edgeClass == class && edge.To == to {
				return append(clonePath(current.path), cloneTransition(edge)), nil
			}
			if edgeClass != protocol.ActionNavigation || seen[edge.To] {
				continue
			}
			path := append(clonePath(current.path), cloneTransition(edge))
			seen[edge.To] = true
			queue = append(queue, node{state: edge.To, path: path})
		}
	}
	return nil, fmt.Errorf(
		"%s: не найден безопасный путь из %s с финальным переходом класса %s в %s",
		methodCtx,
		from,
		class,
		to,
	)
}

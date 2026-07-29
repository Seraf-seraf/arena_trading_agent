// Package navigation строит безопасные пути по известным состояниям UI.
package navigation

import (
	"fmt"

	"github.com/arena-trading-agent/arena-trading-agent/internal/domain"
	"github.com/arena-trading-agent/arena-trading-agent/internal/protocol"
)

// VerificationRule задаёт ожидаемый результат перехода.
type VerificationRule struct {
	State         domain.ScreenState
	MinConfidence float64
}

// Transition описывает проверяемое ребро графа экранов.
type Transition struct {
	From     domain.ScreenState
	To       domain.ScreenState
	Action   protocol.Action
	Verify   VerificationRule
	MaxRetry int
}

// Navigator хранит известный граф интерфейса.
type Navigator struct {
	edges map[domain.ScreenState][]Transition
}

// New создаёт навигатор и отклоняет небезопасные переходы.
func New(transitions []Transition) (*Navigator, error) {
	n := &Navigator{edges: make(map[domain.ScreenState][]Transition)}
	for _, transition := range transitions {
		if transition.From == domain.StateUnknown || transition.To == domain.StateUnknown {
			return nil, fmt.Errorf("переходы через UNKNOWN должны выполняться recovery-политикой")
		}
		if transition.Verify.State != transition.To || transition.MaxRetry < 0 {
			return nil, fmt.Errorf("некорректная проверка перехода %s → %s", transition.From, transition.To)
		}
		n.edges[transition.From] = append(n.edges[transition.From], transition)
	}
	return n, nil
}

// Path возвращает кратчайшую известную последовательность переходов.
func (n *Navigator) Path(from, to domain.ScreenState) ([]Transition, error) {
	if from == to {
		return []Transition{}, nil
	}
	type node struct {
		state domain.ScreenState
		path  []Transition
	}
	queue := []node{{state: from}}
	seen := map[domain.ScreenState]bool{from: true}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		for _, edge := range n.edges[current.state] {
			if seen[edge.To] {
				continue
			}
			path := append(append([]Transition(nil), current.path...), edge)
			if edge.To == to {
				return path, nil
			}
			seen[edge.To] = true
			queue = append(queue, node{state: edge.To, path: path})
		}
	}
	return nil, fmt.Errorf("не найден путь из %s в %s", from, to)
}

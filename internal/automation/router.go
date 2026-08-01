// Package automation wires observation, navigation, scanners and trading
// policy without giving vision services access to the input transport.
package automation

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/arena-trading-agent/arena-trading-agent/internal/controller"
	"github.com/arena-trading-agent/arena-trading-agent/internal/domain"
	"github.com/arena-trading-agent/arena-trading-agent/internal/navigation"
	"github.com/arena-trading-agent/arena-trading-agent/internal/protocol"
	"github.com/arena-trading-agent/arena-trading-agent/internal/session"
)

var ErrCommitValidation = errors.New("денежный переход отклонён до отправки ввода")

// CommitValidator проверяет критичные экономические значения на том же
// наблюдении, к которому будет привязан финальный ACTION_REQUEST.
type CommitValidator func(domain.Observation) error

// RouteNavigator is the scanner-facing subset of Router and is convenient for
// deterministic scanner tests.
type RouteNavigator interface {
	Navigate(
		context.Context,
		string,
		string,
		domain.ScreenState,
		map[string]string,
	) (navigation.Result, error)
}

// TradeRouteNavigator is the controller-owned capability used only by
// TradeRunner. Commit requires one preselected final monetary class.
type TradeRouteNavigator interface {
	RouteNavigator
	Commit(
		context.Context,
		string,
		string,
		domain.ScreenState,
		protocol.ActionClass,
		map[string]string,
		CommitValidator,
	) (navigation.Result, error)
}

// Router captures a fresh starting observation, finds the shortest calibrated
// path and delegates all input to navigation.Executor.
type Router struct {
	transport   *controller.Server
	coordinator *session.Coordinator
	navigator   *navigation.Navigator
	executor    *navigation.Executor
	gate        sync.Mutex
}

func NewRouter(
	transport *controller.Server,
	coordinator *session.Coordinator,
	navigator *navigation.Navigator,
	executor *navigation.Executor,
) (*Router, error) {
	const methodCtx = "automation.NewRouter"

	if transport == nil || coordinator == nil || navigator == nil || executor == nil {
		return nil, fmt.Errorf("%s: маршрутизатору автоматизации требуются транспорт, координатор, навигатор и исполнитель", methodCtx)
	}
	return &Router{
		transport: transport, coordinator: coordinator, navigator: navigator, executor: executor,
	}, nil
}

func (r *Router) Navigate(
	ctx context.Context,
	agentID string,
	sessionID string,
	target domain.ScreenState,
	variables map[string]string,
) (navigation.Result, error) {
	const methodCtx = "automation.Router.Navigate"

	result, err := r.route(
		ctx,
		agentID,
		sessionID,
		target,
		protocol.ActionNavigation,
		variables,
		nil,
	)
	if err != nil {
		return result, fmt.Errorf("%s: безопасная навигация завершилась ошибкой: %w", methodCtx, err)
	}
	return result, nil
}

// Commit executes a route whose final edge is exactly the requested monetary
// class. The path is selected and checked before Executor receives any input.
func (r *Router) Commit(
	ctx context.Context,
	agentID string,
	sessionID string,
	target domain.ScreenState,
	class protocol.ActionClass,
	variables map[string]string,
	validate CommitValidator,
) (navigation.Result, error) {
	const methodCtx = "automation.Router.Commit"

	switch class {
	case protocol.ActionPurchase, protocol.ActionBarter,
		protocol.ActionListing, protocol.ActionReprice:
	default:
		return navigation.Result{}, fmt.Errorf(
			"%s: требуется явный денежный класс, получено %q",
			methodCtx,
			class,
		)
	}
	if validate == nil {
		return navigation.Result{}, fmt.Errorf(
			"%s: проверка критичных значений денежного перехода обязательна",
			methodCtx,
		)
	}
	result, err := r.route(
		ctx,
		agentID,
		sessionID,
		target,
		class,
		variables,
		validate,
	)
	if err != nil {
		return result, fmt.Errorf("%s: фиксация денежного перехода завершилась ошибкой: %w", methodCtx, err)
	}
	return result, nil
}

func (r *Router) route(
	ctx context.Context,
	agentID string,
	sessionID string,
	target domain.ScreenState,
	class protocol.ActionClass,
	variables map[string]string,
	validate CommitValidator,
) (navigation.Result, error) {
	const methodCtx = "automation.Router.route"

	if ctx == nil {
		return navigation.Result{}, fmt.Errorf("%s: контекст не задан", methodCtx)
	}
	if strings.TrimSpace(sessionID) == "" {
		return navigation.Result{}, fmt.Errorf("%s: поле session_id не задано", methodCtx)
	}
	if target == "" || target == domain.StateUnknown {
		return navigation.Result{}, fmt.Errorf("%s: недопустимое целевое состояние %q", methodCtx, target)
	}
	if !r.gate.TryLock() {
		return navigation.Result{}, fmt.Errorf("%s: маршрутизатор занят другим UI-маршрутом", methodCtx)
	}
	defer r.gate.Unlock()
	resolvedAgent, err := r.resolveAgent(agentID)
	if err != nil {
		return navigation.Result{}, fmt.Errorf("%s: не удалось определить агента: %w", methodCtx, err)
	}
	afterFrame := uint64(0)
	if state, ok := r.transport.AgentState(resolvedAgent); ok && state.LatestFrame != nil {
		afterFrame = state.LatestFrame.ID
	}
	frame, observation, err := r.coordinator.ObserveAfter(ctx, resolvedAgent, afterFrame)
	if err != nil {
		return navigation.Result{}, fmt.Errorf("%s: не удалось получить начальное наблюдение: %w", methodCtx, err)
	}
	result := navigation.Result{Frame: frame, Observation: observation}
	if observation.State == target {
		if class != protocol.ActionNavigation {
			return result, fmt.Errorf(
				"%s: денежный переход класса %s не выполнен: экран уже находится в состоянии %s",
				methodCtx,
				class,
				target,
			)
		}
		return result, nil
	}
	var path navigation.Path
	if class == protocol.ActionNavigation {
		path, err = r.navigator.PathForClass(
			observation.State,
			target,
			protocol.ActionNavigation,
		)
	} else {
		path, err = r.navigator.PathForCommit(observation.State, target, class)
	}
	if err != nil {
		return result, fmt.Errorf("%s: не удалось построить безопасный путь: %w", methodCtx, err)
	}
	path, err = specializePathForClass(path, variables, class)
	if err != nil {
		return result, fmt.Errorf("%s: не удалось подставить параметры пути: %w", methodCtx, err)
	}
	window, err := r.transport.RequestWindowStatus(ctx, resolvedAgent)
	if err != nil {
		return result, fmt.Errorf("%s: не удалось получить состояние окна: %w", methodCtx, err)
	}
	geometry := navigation.Geometry{
		Width: window.Width, Height: window.Height, DPIPercent: window.DPIPercent,
	}
	if class == protocol.ActionNavigation {
		result, err = r.executor.Execute(ctx, navigation.Request{
			AgentID: resolvedAgent, SessionID: sessionID, Path: path,
			Frame: frame, Observation: observation, Geometry: geometry,
		})
		if err != nil {
			return result, fmt.Errorf("%s: не удалось выполнить маршрут: %w", methodCtx, err)
		}
		return result, nil
	}

	prefix := path[:len(path)-1]
	final := path[len(path)-1:]
	if len(prefix) > 0 {
		result, err = r.executor.Execute(ctx, navigation.Request{
			AgentID: resolvedAgent, SessionID: sessionID, Path: prefix,
			Frame: frame, Observation: observation, Geometry: geometry,
		})
		if err != nil {
			return result, fmt.Errorf(
				"%s: не удалось выполнить навигационную часть денежного маршрута: %w",
				methodCtx,
				err,
			)
		}
	}
	if err := validate(result.Observation); err != nil {
		return result, errors.Join(
			ErrCommitValidation,
			fmt.Errorf("%s: критичные значения перед денежным вводом изменились: %w", methodCtx, err),
		)
	}
	committed, err := r.executor.Execute(ctx, navigation.Request{
		AgentID: resolvedAgent, SessionID: sessionID, Path: final,
		Frame: result.Frame, Observation: result.Observation, Geometry: geometry,
	})
	result = mergeNavigationResults(result, committed)
	if err != nil {
		return result, fmt.Errorf("%s: не удалось выполнить финальный денежный переход: %w", methodCtx, err)
	}
	return result, nil
}

func mergeNavigationResults(
	prefix navigation.Result,
	final navigation.Result,
) navigation.Result {
	return navigation.Result{
		CompletedTransitions: prefix.CompletedTransitions + final.CompletedTransitions,
		Attempts: append(
			append([]navigation.Attempt(nil), prefix.Attempts...),
			final.Attempts...,
		),
		Frame:       final.Frame,
		Observation: final.Observation,
	}
}

func (r *Router) resolveAgent(agentID string) (string, error) {
	const methodCtx = "automation.Router.resolveAgent"

	if agentID != "" {
		if _, ok := r.transport.AgentState(agentID); !ok {
			return "", fmt.Errorf("%s: агент недоступен: %w", methodCtx, controller.ErrAgentNotConnected)
		}
		return agentID, nil
	}
	state := r.transport.State()
	if len(state.Agents) != 1 {
		if len(state.Agents) == 0 {
			return "", fmt.Errorf("%s: агент недоступен: %w", methodCtx, controller.ErrAgentNotConnected)
		}
		return "", fmt.Errorf("%s: поле agent_id обязательно при нескольких агентах", methodCtx)
	}
	return state.Agents[0].AgentID, nil
}

func specializePath(path navigation.Path, variables map[string]string) (navigation.Path, error) {
	const methodCtx = "automation.specializePath"

	result, err := specializePathForClass(path, variables, protocol.ActionNavigation)
	if err != nil {
		return nil, fmt.Errorf("%s: навигационный путь отклонён: %w", methodCtx, err)
	}
	return result, nil
}

func specializePathForClass(
	path navigation.Path,
	variables map[string]string,
	class protocol.ActionClass,
) (navigation.Path, error) {
	const methodCtx = "automation.specializePathForClass"

	result := append(navigation.Path(nil), path...)
	for index := range result {
		transitionClass := result[index].Class
		if transitionClass == "" {
			transitionClass = protocol.ActionNavigation
		}
		allowed := transitionClass == protocol.ActionNavigation
		if class != protocol.ActionNavigation &&
			index == len(result)-1 &&
			transitionClass == class {
			allowed = true
		}
		if !allowed {
			return nil, fmt.Errorf(
				"%s: переход %s → %s имеет недопустимый класс %s для маршрута %s",
				methodCtx,
				result[index].From,
				result[index].To,
				transitionClass,
				class,
			)
		}
		action, err := specializeAction(result[index].Action, variables)
		if err != nil {
			return nil, fmt.Errorf(
				"%s: не удалось подготовить переход %s → %s: %w",
				methodCtx,
				result[index].From,
				result[index].To,
				err,
			)
		}
		result[index].Action = action
		if result[index].Verify.BBox != nil {
			bbox := *result[index].Verify.BBox
			result[index].Verify.BBox = &bbox
		}
	}
	if class != protocol.ActionNavigation {
		if len(result) == 0 {
			return nil, fmt.Errorf("%s: денежный путь пуст", methodCtx)
		}
		finalClass := result[len(result)-1].Class
		if finalClass == "" {
			finalClass = protocol.ActionNavigation
		}
		if finalClass != class {
			return nil, fmt.Errorf(
				"%s: финальный переход имеет класс %s вместо %s",
				methodCtx,
				finalClass,
				class,
			)
		}
	}
	return result, nil
}

func specializeAction(value protocol.Action, variables map[string]string) (protocol.Action, error) {
	const methodCtx = "automation.specializeAction"

	result := value
	if value.Point != nil {
		point := *value.Point
		result.Point = &point
	}
	var err error
	result.Value, err = expandVariables(value.Value, variables)
	if err != nil {
		return protocol.Action{}, fmt.Errorf("%s: не удалось подставить параметры действия: %w", methodCtx, err)
	}
	if value.Steps != nil {
		result.Steps = make([]protocol.Action, len(value.Steps))
		for index, step := range value.Steps {
			result.Steps[index], err = specializeAction(step, variables)
			if err != nil {
				return protocol.Action{}, fmt.Errorf("%s: шаг %d последовательности: %w", methodCtx, index+1, err)
			}
		}
	}
	return result, nil
}

func expandVariables(template string, variables map[string]string) (string, error) {
	const methodCtx = "automation.expandVariables"

	var output strings.Builder
	for {
		start := strings.Index(template, "${")
		if start < 0 {
			output.WriteString(template)
			return output.String(), nil
		}
		output.WriteString(template[:start])
		template = template[start+2:]
		end := strings.IndexByte(template, '}')
		if end < 0 {
			return "", fmt.Errorf("%s: незакрытый заполнитель", methodCtx)
		}
		key := template[:end]
		if key == "" {
			return "", fmt.Errorf("%s: пустой заполнитель", methodCtx)
		}
		replacement, exists := variables[key]
		if !exists {
			return "", fmt.Errorf("%s: не задан заполнитель %q", methodCtx, key)
		}
		output.WriteString(replacement)
		template = template[end+1:]
	}
}

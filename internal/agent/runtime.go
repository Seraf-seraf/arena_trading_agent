package agent

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/arena-trading-agent/arena-trading-agent/internal/domain"
	"github.com/arena-trading-agent/arena-trading-agent/internal/protocol"
)

// CaptureDriver получает полный кадр или его нормализованную область.
type CaptureDriver interface {
	Capture(context.Context) (protocol.Frame, error)
	CaptureRegion(context.Context, domain.Rectangle) (protocol.Frame, error)
}

// InputDriver выполняет низкоуровневый ввод в окно игры.
type InputDriver interface {
	Move(context.Context, domain.Point) error
	Click(context.Context, string) error
	Scroll(context.Context, int) error
	Key(context.Context, string) error
	Text(context.Context, string) error
}

// WindowManager возвращает актуальные свойства окна и переводит координаты.
type WindowManager interface {
	Status(context.Context) (protocol.WindowStatus, error)
}

// StateDetector определяет известное состояние по кадру.
type StateDetector interface {
	Detect(context.Context, protocol.Frame) (domain.ScreenState, float64, error)
}

// ActionExecutor является единственным владельцем InputDriver.
type ActionExecutor struct {
	input       InputDriver
	capture     CaptureDriver
	window      WindowManager
	detector    StateDetector
	mu          sync.Mutex
	latestFrame func() uint64
	stopped     func() bool
}

// NewActionExecutor создаёт последовательный исполнитель команд.
func NewActionExecutor(input InputDriver, capture CaptureDriver, window WindowManager, detector StateDetector, latestFrame func() uint64, stopped func() bool) *ActionExecutor {
	return &ActionExecutor{input: input, capture: capture, window: window, detector: detector, latestFrame: latestFrame, stopped: stopped}
}

// Execute проверяет предусловия, выполняет ввод и подтверждает переход.
func (e *ActionExecutor) Execute(ctx context.Context, request protocol.ActionRequest) protocol.ActionResult {
	e.mu.Lock()
	defer e.mu.Unlock()
	result := protocol.ActionResult{ID: request.ID, CompletedAt: time.Now().UTC()}
	fail := func(err error) protocol.ActionResult {
		result.Error = err.Error()
		result.CompletedAt = time.Now().UTC()
		return result
	}
	if e.stopped() {
		return fail(fmt.Errorf("агент остановлен аварийной кнопкой"))
	}
	if !request.Deadline.IsZero() && time.Now().After(request.Deadline) {
		return fail(fmt.Errorf("срок исполнения команды истёк"))
	}
	if request.BasedOnFrame != e.latestFrame() {
		return fail(fmt.Errorf("команда основана на устаревшем кадре"))
	}
	status, err := e.window.Status(ctx)
	if err != nil {
		return fail(fmt.Errorf("не удалось проверить окно: %w", err))
	}
	if !status.Active || status.Minimized {
		return fail(fmt.Errorf("окно игры неактивно или свёрнуто"))
	}
	if err := e.perform(ctx, request.Action); err != nil {
		return fail(err)
	}
	frame, err := e.capture.Capture(ctx)
	if err != nil {
		return fail(fmt.Errorf("не удалось получить контрольный кадр: %w", err))
	}
	state, _, err := e.detector.Detect(ctx, frame)
	if err != nil {
		return fail(fmt.Errorf("не удалось проверить результат действия: %w", err))
	}
	result.ResultFrame, result.ResultState = frame.ID, state
	if state != request.ExpectedState {
		return fail(fmt.Errorf("ожидался экран %s, получен %s", request.ExpectedState, state))
	}
	result.Success = true
	return result
}

func (e *ActionExecutor) perform(ctx context.Context, action protocol.Action) error {
	switch action.Kind {
	case "MOVE":
		if action.Point == nil || !validPoint(*action.Point) {
			return fmt.Errorf("команда MOVE содержит некорректную координату")
		}
		return e.input.Move(ctx, *action.Point)
	case "CLICK":
		return e.input.Click(ctx, action.Value)
	case "SCROLL":
		return fmt.Errorf("для SCROLL требуется числовая нагрузка")
	case "KEY":
		return e.input.Key(ctx, action.Value)
	case "TEXT":
		return e.input.Text(ctx, action.Value)
	default:
		return fmt.Errorf("неподдерживаемое действие %q", action.Kind)
	}
}

func validPoint(point domain.Point) bool {
	return point.X >= 0 && point.X <= 1 && point.Y >= 0 && point.Y <= 1
}

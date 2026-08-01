package agent

import (
	"context"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/arena-trading-agent/arena-trading-agent/internal/domain"
	"github.com/arena-trading-agent/arena-trading-agent/internal/protocol"
)

const (
	defaultVerificationConfidence = 0.80
	maximumActionBasisAge         = 15 * time.Second
	maximumClockSkew              = time.Second
	inputSafetyPollInterval       = 5 * time.Millisecond
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
	input        InputDriver
	capture      CaptureDriver
	window       WindowManager
	detector     StateDetector
	mu           sync.Mutex
	latestFrame  func() uint64
	inputBlocked func() bool
}

// NewActionExecutor создаёт последовательный исполнитель команд. inputBlocked
// должен учитывать как паузу, так и аварийную остановку SafetySupervisor.
func NewActionExecutor(input InputDriver, capture CaptureDriver, window WindowManager, detector StateDetector, latestFrame func() uint64, inputBlocked func() bool) *ActionExecutor {
	if inputBlocked == nil {
		inputBlocked = func() bool { return false }
	}
	return &ActionExecutor{
		input:        input,
		capture:      capture,
		window:       window,
		detector:     detector,
		latestFrame:  latestFrame,
		inputBlocked: inputBlocked,
	}
}

// Execute проверяет предусловия, выполняет ввод и подтверждает переход.
func (e *ActionExecutor) Execute(ctx context.Context, request protocol.ActionRequest) protocol.ActionResult {
	const methodCtx = "agent.ActionExecutor.Execute"

	if !e.mu.TryLock() {
		return protocol.ActionResult{
			ID:          request.ID,
			Error:       methodCtx + ": исполнитель уже занят другой командой",
			CompletedAt: time.Now().UTC(),
		}
	}
	defer e.mu.Unlock()
	result := protocol.ActionResult{ID: request.ID}
	inputAttempted := false
	fail := func(err error) protocol.ActionResult {
		const methodCtx = "agent.ActionExecutor.Execute.fail"

		result.RetrySafe = !inputAttempted
		result.Error = fmt.Sprintf("%s: действие завершилось ошибкой: %v", methodCtx, err)
		result.CompletedAt = time.Now().UTC()
		return result
	}
	if err := e.ensureInputAllowed(); err != nil {
		return fail(err)
	}
	if strings.TrimSpace(request.ID) == "" || strings.TrimSpace(request.SessionID) == "" {
		return fail(fmt.Errorf("команда не содержит идентификатор или session_id"))
	}
	if request.BasedOnFrame == 0 || request.ExpectedState == "" || request.ExpectedState == domain.StateUnknown {
		return fail(fmt.Errorf("команда не содержит проверяемый кадр или ожидаемое состояние"))
	}
	if request.BasedOnCapturedAt == nil || request.BasedOnCapturedAt.IsZero() ||
		!protocol.ValidFrameDigest(request.BasedOnFrameDigest) {
		return fail(fmt.Errorf("команда не содержит проверяемые timestamp и digest исходного кадра"))
	}
	if request.BasedOnState == "" || request.BasedOnState == domain.StateUnknown {
		return fail(fmt.Errorf("команда не содержит ожидаемое исходное состояние"))
	}
	if err := validateActionClassAndBasis(request.Class, request.FrameBasis); err != nil {
		return fail(err)
	}
	verificationConfidence := request.MinVerificationConfidence
	if verificationConfidence == 0 {
		verificationConfidence = defaultVerificationConfidence
	}
	if !validConfidence(verificationConfidence) || verificationConfidence == 0 {
		return fail(fmt.Errorf(
			"минимальная уверенность проверки %.3f находится вне диапазона (0, 1]",
			verificationConfidence,
		))
	}
	if request.ExpectedWidth <= 0 || request.ExpectedHeight <= 0 || request.ExpectedDPIPercent <= 0 {
		return fail(fmt.Errorf("команда не содержит ожидаемую геометрию окна"))
	}
	if request.Deadline.IsZero() || time.Now().After(request.Deadline) {
		return fail(fmt.Errorf("срок исполнения команды истёк"))
	}
	if e.latestFrame == nil {
		return fail(fmt.Errorf("исполнитель команды не отслеживает кадры"))
	}
	if request.BasedOnFrame != e.latestFrame() {
		return fail(fmt.Errorf("команда основана на устаревшем кадре"))
	}
	if e.window == nil || e.input == nil || e.capture == nil || e.detector == nil {
		return fail(fmt.Errorf("исполнитель команды настроен не полностью"))
	}
	if err := validateAgentAction(request.Action, false); err != nil {
		return fail(err)
	}
	status, err := e.window.Status(ctx)
	if err != nil {
		return fail(fmt.Errorf("не удалось проверить окно: %w", err))
	}
	if err := validateWindowStatus(status, request); err != nil {
		return fail(err)
	}

	basisCapturedAt := request.BasedOnCapturedAt.UTC()
	now := time.Now().UTC()
	if basisCapturedAt.After(now.Add(maximumClockSkew)) {
		return fail(fmt.Errorf("время исходного кадра находится в будущем"))
	}
	preInputFrame, err := e.capture.Capture(ctx)
	if err != nil {
		return fail(fmt.Errorf("не удалось получить свежий кадр перед вводом: %w", err))
	}
	preInputFrame, err = protocol.NormalizeFrameDigest(preInputFrame)
	if err != nil {
		return fail(fmt.Errorf("свежий кадр перед вводом не прошёл проверку digest: %w", err))
	}
	result.ResultFrame = preInputFrame.ID
	result.Frame = &preInputFrame
	if preInputFrame.ID <= request.BasedOnFrame {
		return fail(fmt.Errorf(
			"кадр перед вводом %d не новее исходного %d",
			preInputFrame.ID,
			request.BasedOnFrame,
		))
	}
	if preInputFrame.CapturedAt.IsZero() {
		return fail(fmt.Errorf("кадр перед вводом не содержит captured_at"))
	}
	preInputCapturedAt := preInputFrame.CapturedAt.UTC()
	if !preInputCapturedAt.After(basisCapturedAt) {
		return fail(fmt.Errorf("кадр перед вводом снят не позже исходного кадра"))
	}
	if preInputCapturedAt.Sub(basisCapturedAt) > maximumActionBasisAge {
		return fail(fmt.Errorf(
			"основание команды устарело на %s, допустимо не более %s",
			preInputCapturedAt.Sub(basisCapturedAt).Round(time.Millisecond),
			maximumActionBasisAge,
		))
	}
	if preInputCapturedAt.After(time.Now().UTC().Add(maximumClockSkew)) {
		return fail(fmt.Errorf("время кадра перед вводом находится в будущем"))
	}
	if len(request.FrameBasis) > 0 {
		if err := protocol.VerifyFrameRegionBasis(preInputFrame, request.FrameBasis); err != nil {
			return fail(fmt.Errorf("ROI исходного экрана изменились перед вводом: %w", err))
		}
	} else if preInputFrame.ContentDigest != request.BasedOnFrameDigest {
		return fail(fmt.Errorf("содержимое экрана изменилось после исходного кадра"))
	}

	preInputState, preInputConfidence, err := e.detector.Detect(ctx, preInputFrame)
	if err != nil {
		return fail(fmt.Errorf("не удалось проверить состояние перед вводом: %w", err))
	}
	result.ResultState = preInputState
	result.VerificationConfidence = preInputConfidence
	if !validConfidence(preInputConfidence) ||
		preInputConfidence < verificationConfidence {
		return fail(fmt.Errorf(
			"недостаточная уверенность перед вводом: %.3f, требуется %.3f",
			preInputConfidence,
			verificationConfidence,
		))
	}
	if preInputState != request.BasedOnState {
		return fail(fmt.Errorf(
			"перед вводом ожидался экран %s, получен %s",
			request.BasedOnState,
			preInputState,
		))
	}
	if latest := e.latestFrame(); latest != preInputFrame.ID {
		return fail(fmt.Errorf(
			"кадр перед вводом %d уже заменён более новым кадром %d",
			preInputFrame.ID,
			latest,
		))
	}
	status, err = e.window.Status(ctx)
	if err != nil {
		return fail(fmt.Errorf("не удалось повторно проверить окно перед вводом: %w", err))
	}
	if err := validateWindowStatus(status, request); err != nil {
		return fail(err)
	}
	if err := e.ensureInputAllowed(); err != nil {
		return fail(err)
	}
	if time.Now().UTC().After(request.Deadline) {
		return fail(fmt.Errorf("срок исполнения команды истёк перед вводом"))
	}
	inputCtx, stopInputMonitor := e.monitoredInputContext(ctx)
	inputAttempted = true
	err = e.perform(inputCtx, request.Action)
	stopInputMonitor()
	if err != nil {
		return fail(err)
	}
	frame, err := e.capture.Capture(ctx)
	if err != nil {
		return fail(fmt.Errorf("не удалось получить контрольный кадр: %w", err))
	}
	frame, err = protocol.NormalizeFrameDigest(frame)
	if err != nil {
		return fail(fmt.Errorf("контрольный кадр не прошёл проверку digest: %w", err))
	}
	if frame.ID <= preInputFrame.ID {
		result.ResultFrame = frame.ID
		result.Frame = &frame
		return fail(fmt.Errorf(
			"контрольный кадр %d не новее предвводного %d",
			frame.ID,
			preInputFrame.ID,
		))
	}
	if frame.CapturedAt.IsZero() || !frame.CapturedAt.After(preInputFrame.CapturedAt) {
		result.ResultFrame = frame.ID
		result.Frame = &frame
		return fail(fmt.Errorf("контрольный кадр не содержит более новую временную метку"))
	}
	state, confidence, err := e.detector.Detect(ctx, frame)
	if err != nil {
		return fail(fmt.Errorf("не удалось проверить результат действия: %w", err))
	}
	result.ResultFrame, result.ResultState = frame.ID, state
	result.VerificationConfidence = confidence
	result.Frame = &frame
	if !validConfidence(confidence) || confidence < verificationConfidence {
		return fail(fmt.Errorf(
			"недостаточная уверенность проверки: %.3f, требуется %.3f",
			confidence,
			verificationConfidence,
		))
	}
	if state != request.ExpectedState {
		return fail(fmt.Errorf("ожидался экран %s, получен %s", request.ExpectedState, state))
	}
	completedAt := time.Now().UTC()
	if completedAt.After(request.Deadline) {
		return fail(fmt.Errorf("действие завершилось после срока исполнения"))
	}
	result.Success = true
	result.CompletedAt = completedAt
	return result
}

func validateActionClassAndBasis(
	class protocol.ActionClass,
	basis []protocol.FrameRegionDigest,
) error {
	const methodCtx = "agent.validateActionClassAndBasis"

	switch class {
	case protocol.ActionNavigation:
	case protocol.ActionPurchase, protocol.ActionBarter,
		protocol.ActionListing, protocol.ActionReprice:
		if len(basis) == 0 {
			return fmt.Errorf(
				"%s: денежное действие класса %s требует непустое ROI-основание",
				methodCtx,
				class,
			)
		}
	default:
		return fmt.Errorf("%s: команда содержит недопустимый класс %q", methodCtx, class)
	}
	if err := protocol.ValidateFrameRegionBasis(basis); err != nil {
		return fmt.Errorf("%s: ROI-основание команды некорректно: %w", methodCtx, err)
	}
	return nil
}

func validateWindowStatus(status protocol.WindowStatus, request protocol.ActionRequest) error {
	const methodCtx = "agent.validateWindowStatus"

	if !status.Active || status.Minimized {
		return fmt.Errorf("%s: окно игры неактивно или свёрнуто", methodCtx)
	}
	if status.Width != request.ExpectedWidth {
		return fmt.Errorf(
			"%s: ширина клиентской области изменилась: ожидалось %d, получено %d",
			methodCtx,
			request.ExpectedWidth,
			status.Width,
		)
	}
	if status.Height != request.ExpectedHeight {
		return fmt.Errorf(
			"%s: высота клиентской области изменилась: ожидалось %d, получено %d",
			methodCtx,
			request.ExpectedHeight,
			status.Height,
		)
	}
	if status.DPIPercent != request.ExpectedDPIPercent {
		return fmt.Errorf(
			"%s: DPI изменился: ожидалось %d%%, получено %d%%",
			methodCtx,
			request.ExpectedDPIPercent,
			status.DPIPercent,
		)
	}
	return nil
}

func validConfidence(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value >= 0 && value <= 1
}

func validateAgentAction(action protocol.Action, nested bool) error {
	const methodCtx = "agent.validateAgentAction"

	kind := strings.ToUpper(strings.TrimSpace(action.Kind))
	switch kind {
	case "MOVE":
		if action.Point == nil || !validPoint(*action.Point) {
			return fmt.Errorf("%s: команда MOVE содержит некорректную координату", methodCtx)
		}
	case "CLICK":
		if action.Point != nil && !validPoint(*action.Point) {
			return fmt.Errorf("%s: команда CLICK содержит некорректную координату", methodCtx)
		}
	case "SCROLL":
		if action.Point != nil && !validPoint(*action.Point) {
			return fmt.Errorf("%s: команда SCROLL содержит некорректную координату", methodCtx)
		}
		if action.Delta == 0 {
			return fmt.Errorf("%s: для SCROLL требуется ненулевое смещение", methodCtx)
		}
	case "KEY", "TEXT":
		if strings.TrimSpace(action.Value) == "" {
			return fmt.Errorf("%s: команда %s требует значение", methodCtx, kind)
		}
	case "SEQUENCE":
		if nested {
			return fmt.Errorf("%s: вложенная SEQUENCE запрещена", methodCtx)
		}
		if len(action.Steps) == 0 || len(action.Steps) > 64 {
			return fmt.Errorf("%s: SEQUENCE должна содержать от 1 до 64 шагов", methodCtx)
		}
		for index, step := range action.Steps {
			if err := validateAgentAction(step, true); err != nil {
				return fmt.Errorf("%s: шаг %d последовательности: %w", methodCtx, index+1, err)
			}
		}
	default:
		return fmt.Errorf("%s: неподдерживаемое действие %q", methodCtx, action.Kind)
	}
	return nil
}

// monitoredInputContext отменяет выполняющийся многочастный ввод сразу после
// локальной паузы или emergency stop. Это позволяет TEXT/SEQUENCE остановиться
// между пакетами, не ожидая завершения всей команды.
func (e *ActionExecutor) monitoredInputContext(
	ctx context.Context,
) (context.Context, context.CancelFunc) {
	inputCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(inputSafetyPollInterval)
		defer ticker.Stop()
		for {
			if e.inputBlocked != nil && e.inputBlocked() {
				cancel()
				return
			}
			select {
			case <-inputCtx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return inputCtx, func() {
		cancel()
		<-done
	}
}

func (e *ActionExecutor) perform(ctx context.Context, action protocol.Action) error {
	const methodCtx = "agent.ActionExecutor.perform"

	switch strings.ToUpper(strings.TrimSpace(action.Kind)) {
	case "MOVE":
		if action.Point == nil || !validPoint(*action.Point) {
			return fmt.Errorf("%s: команда MOVE содержит некорректную координату", methodCtx)
		}
		if err := e.ensureInputAllowed(); err != nil {
			return fmt.Errorf("%s: перемещение указателя запрещено: %w", methodCtx, err)
		}
		if err := e.input.Move(ctx, *action.Point); err != nil {
			return fmt.Errorf("%s: не удалось переместить указатель: %w", methodCtx, err)
		}
		return nil
	case "CLICK":
		if action.Point != nil {
			if !validPoint(*action.Point) {
				return fmt.Errorf("%s: команда CLICK содержит некорректную координату", methodCtx)
			}
			if err := e.ensureInputAllowed(); err != nil {
				return fmt.Errorf("%s: перемещение указателя перед кликом запрещено: %w", methodCtx, err)
			}
			if err := e.input.Move(ctx, *action.Point); err != nil {
				return fmt.Errorf("%s: не удалось переместить указатель перед кликом: %w", methodCtx, err)
			}
		}
		if err := e.ensureInputAllowed(); err != nil {
			return fmt.Errorf("%s: клик запрещён: %w", methodCtx, err)
		}
		if err := e.input.Click(ctx, action.Value); err != nil {
			return fmt.Errorf("%s: не удалось выполнить клик: %w", methodCtx, err)
		}
		return nil
	case "SCROLL":
		if action.Point != nil {
			if !validPoint(*action.Point) {
				return fmt.Errorf("%s: команда SCROLL содержит некорректную координату", methodCtx)
			}
			if err := e.ensureInputAllowed(); err != nil {
				return fmt.Errorf("%s: перемещение указателя перед прокруткой запрещено: %w", methodCtx, err)
			}
			if err := e.input.Move(ctx, *action.Point); err != nil {
				return fmt.Errorf("%s: не удалось переместить указатель перед прокруткой: %w", methodCtx, err)
			}
		}
		if action.Delta == 0 {
			return fmt.Errorf("%s: для SCROLL требуется ненулевое смещение", methodCtx)
		}
		if err := e.ensureInputAllowed(); err != nil {
			return fmt.Errorf("%s: прокрутка запрещена: %w", methodCtx, err)
		}
		if err := e.input.Scroll(ctx, action.Delta); err != nil {
			return fmt.Errorf("%s: не удалось выполнить прокрутку: %w", methodCtx, err)
		}
		return nil
	case "KEY":
		if err := e.ensureInputAllowed(); err != nil {
			return fmt.Errorf("%s: нажатие клавиши запрещено: %w", methodCtx, err)
		}
		if err := e.input.Key(ctx, action.Value); err != nil {
			return fmt.Errorf("%s: не удалось нажать клавишу: %w", methodCtx, err)
		}
		return nil
	case "TEXT":
		if err := e.ensureInputAllowed(); err != nil {
			return fmt.Errorf("%s: ввод текста запрещён: %w", methodCtx, err)
		}
		if err := e.input.Text(ctx, action.Value); err != nil {
			return fmt.Errorf("%s: не удалось ввести текст: %w", methodCtx, err)
		}
		return nil
	case "SEQUENCE":
		if len(action.Steps) == 0 || len(action.Steps) > 64 {
			return fmt.Errorf("%s: SEQUENCE должна содержать от 1 до 64 шагов", methodCtx)
		}
		for index, step := range action.Steps {
			if strings.EqualFold(strings.TrimSpace(step.Kind), "SEQUENCE") {
				return fmt.Errorf("%s: вложенная SEQUENCE запрещена", methodCtx)
			}
			if err := e.ensureInputAllowed(); err != nil {
				return fmt.Errorf("%s: шаг %d последовательности запрещён: %w", methodCtx, index+1, err)
			}
			if err := e.perform(ctx, step); err != nil {
				return fmt.Errorf("%s: шаг %d последовательности: %w", methodCtx, index+1, err)
			}
		}
		return nil
	default:
		return fmt.Errorf("%s: неподдерживаемое действие %q", methodCtx, action.Kind)
	}
}

func (e *ActionExecutor) ensureInputAllowed() error {
	const methodCtx = "agent.ActionExecutor.ensureInputAllowed"

	if e.inputBlocked != nil && e.inputBlocked() {
		return fmt.Errorf("%s: автоматика приостановлена или агент остановлен аварийной кнопкой", methodCtx)
	}
	return nil
}

func validPoint(point domain.Point) bool {
	return point.X >= 0 && point.X <= 1 && point.Y >= 0 && point.Y <= 1
}

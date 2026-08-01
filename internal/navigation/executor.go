package navigation

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/arena-trading-agent/arena-trading-agent/internal/domain"
	"github.com/arena-trading-agent/arena-trading-agent/internal/protocol"
)

var (
	// ErrBusy means another route owns this executor. Callers must obtain a new
	// observation instead of queueing behind a route whose context can age.
	ErrBusy = errors.New("исполнитель навигации занят")

	// ErrInvalidRequest covers malformed paths, geometry and actions.
	ErrInvalidRequest = errors.New("некорректный запрос навигации")

	// ErrStaleObservation means an observation/frame pair is too old,
	// inconsistent or was replayed after an action.
	ErrStaleObservation = errors.New("устаревшее наблюдение")

	// ErrUnknownState prevents input when the observer cannot identify the UI.
	ErrUnknownState = errors.New("неизвестное состояние интерфейса")

	// ErrLowConfidence prevents input when visual evidence is insufficient.
	ErrLowConfidence = errors.New("недостаточная уверенность наблюдения")

	// ErrStateMismatch means the live UI no longer matches the declarative
	// route, or moved to an unexpected state after an action.
	ErrStateMismatch = errors.New("состояние не соответствует маршруту")

	// ErrInvalidActionResult means Windows returned an inconsistent or
	// unverifiable acknowledgement.
	ErrInvalidActionResult = errors.New("некорректный результат действия")

	// ErrActionRejected means Windows conclusively rejected or failed an
	// action. A retry is possible only under the strict conditions documented
	// by canRetry.
	ErrActionRejected = errors.New("действие отклонено")
)

const (
	defaultActionTimeout     = 3 * time.Second
	defaultObservationMaxAge = 12 * time.Second
	defaultMinConfidence     = 0.80
	defaultMaxRetryLimit     = 3
)

// Geometry is the immutable client-area contract copied into every action.
// A route never infers pixel geometry from encoded frame bytes.
type Geometry struct {
	Width      int
	Height     int
	DPIPercent int
}

// ActionClient is the only output capability available to Executor. In
// production it is implemented by the controller broker, not by InputDriver.
type ActionClient interface {
	RequestAction(context.Context, string, protocol.ActionRequest) (protocol.ActionResult, error)
}

// ObservationSource captures and recognizes a new frame after an action.
// Implementations must honor afterFrame and return a strictly newer frame.
type ObservationSource interface {
	ObserveAfter(context.Context, string, uint64) (protocol.Frame, domain.Observation, error)
}

// Config contains fail-closed limits. Now and NewActionID are injection points
// for deterministic tests; production callers normally leave them nil.
type Config struct {
	ActionTimeout     time.Duration
	ObservationMaxAge time.Duration
	MinConfidence     float64
	MaxRetryLimit     int
	Now               func() time.Time
	NewActionID       func(sessionID string, step, attempt int) string
}

// Request starts a route from a frame and its normalized observation.
type Request struct {
	AgentID     string
	SessionID   string
	Path        Path
	Frame       protocol.Frame
	Observation domain.Observation
	Geometry    Geometry
}

// Attempt is an immutable audit record returned to the caller.
type Attempt struct {
	Step        int
	Attempt     int
	Request     protocol.ActionRequest
	Result      protocol.ActionResult
	Frame       protocol.Frame
	Observation domain.Observation
}

// Result contains the last verified UI state and all completed action
// attempts. CompletedTransitions is incremented only after both Windows and
// the fresh observer confirm the target state.
type Result struct {
	CompletedTransitions int
	Attempts             []Attempt
	Frame                protocol.Frame
	Observation          domain.Observation
}

// Executor serializes complete routes. It deliberately has no InputDriver.
type Executor struct {
	actions      ActionClient
	observations ObservationSource
	config       Config
	gate         sync.Mutex
	sequence     atomic.Uint64
}

// NewExecutor creates a safe navigation executor.
func NewExecutor(actions ActionClient, observations ObservationSource, config Config) (*Executor, error) {
	const methodCtx = "navigation.NewExecutor"

	if actions == nil {
		return nil, fmt.Errorf("%s: %w: клиент действий не настроен", methodCtx, ErrInvalidRequest)
	}
	if observations == nil {
		return nil, fmt.Errorf("%s: %w: источник наблюдений не настроен", methodCtx, ErrInvalidRequest)
	}
	if config.ActionTimeout <= 0 {
		config.ActionTimeout = defaultActionTimeout
	}
	if config.ObservationMaxAge <= 0 {
		config.ObservationMaxAge = defaultObservationMaxAge
	}
	if config.MinConfidence == 0 {
		config.MinConfidence = defaultMinConfidence
	}
	if !validConfidence(config.MinConfidence) || config.MinConfidence == 0 {
		return nil, fmt.Errorf("%s: %w: минимальная уверенность должна быть в диапазоне (0, 1]", methodCtx, ErrInvalidRequest)
	}
	if config.MaxRetryLimit == 0 {
		config.MaxRetryLimit = defaultMaxRetryLimit
	}
	if config.MaxRetryLimit < 0 {
		return nil, fmt.Errorf("%s: %w: максимальный лимит повторов не может быть отрицательным", methodCtx, ErrInvalidRequest)
	}
	if config.Now == nil {
		config.Now = func() time.Time { return time.Now().UTC() }
	}
	executor := &Executor{actions: actions, observations: observations, config: config}
	if executor.config.NewActionID == nil {
		executor.config.NewActionID = func(sessionID string, step, attempt int) string {
			sequence := executor.sequence.Add(1)
			return fmt.Sprintf("%s-nav-%d-%d-%d", sessionID, step+1, attempt+1, sequence)
		}
	}
	return executor, nil
}

// Execute validates and runs a complete route. Concurrent calls fail
// immediately rather than wait for an initial observation to become stale.
func (e *Executor) Execute(ctx context.Context, request Request) (Result, error) {
	const methodCtx = "navigation.Executor.Execute"

	if ctx == nil {
		return Result{}, fmt.Errorf("%s: %w: контекст не задан", methodCtx, ErrInvalidRequest)
	}
	if !e.gate.TryLock() {
		return Result{}, fmt.Errorf("%s: маршрут не запущен: %w", methodCtx, ErrBusy)
	}
	defer e.gate.Unlock()

	request.Path = clonePath(request.Path)
	request.Frame.Data = append([]byte(nil), request.Frame.Data...)
	result := Result{Frame: request.Frame, Observation: request.Observation}
	if err := ctx.Err(); err != nil {
		return result, fmt.Errorf("%s: контекст завершён до запуска: %w", methodCtx, err)
	}
	if err := e.validateRequest(request); err != nil {
		return result, fmt.Errorf("%s: запрос не прошёл проверку: %w", methodCtx, err)
	}
	if err := e.validateSnapshot(request.Frame, request.Observation, 0, e.config.MinConfidence); err != nil {
		return result, fmt.Errorf("%s: начальный снимок не прошёл проверку: %w", methodCtx, err)
	}

	actionIDs := make(map[string]struct{})
	for step, transition := range request.Path {
		if err := ctx.Err(); err != nil {
			return result, fmt.Errorf("%s: контекст завершён на шаге %d: %w", methodCtx, step+1, err)
		}
		requiredConfidence := math.Max(e.config.MinConfidence, transition.Verify.MinConfidence)
		if err := validateState(result.Observation, transition.From, requiredConfidence); err != nil {
			return result, fmt.Errorf("%s: шаг %d (%s → %s): %w", methodCtx, step+1, transition.From, transition.To, err)
		}

		completed := false
		for attempt := 0; attempt <= transition.MaxRetry; attempt++ {
			if err := ctx.Err(); err != nil {
				return result, fmt.Errorf("%s: контекст завершён на шаге %d, попытке %d: %w", methodCtx, step+1, attempt+1, err)
			}
			if err := e.validateSnapshot(result.Frame, result.Observation, 0, requiredConfidence); err != nil {
				return result, fmt.Errorf("%s: шаг %d, попытка %d: %w", methodCtx, step+1, attempt+1, err)
			}
			if err := validateState(result.Observation, transition.From, requiredConfidence); err != nil {
				return result, fmt.Errorf("%s: шаг %d, попытка %d: %w", methodCtx, step+1, attempt+1, err)
			}

			actionRequest, err := e.actionRequest(
				ctx,
				request,
				transition,
				result.Frame,
				result.Observation,
				step,
				attempt,
			)
			if err != nil {
				return result, fmt.Errorf("%s: шаг %d, попытка %d: %w", methodCtx, step+1, attempt+1, err)
			}
			if _, duplicate := actionIDs[actionRequest.ID]; duplicate {
				return result, fmt.Errorf(
					"%s: шаг %d, попытка %d: %w: повторный идентификатор действия %q",
					methodCtx, step+1, attempt+1, ErrInvalidRequest, actionRequest.ID,
				)
			}
			actionIDs[actionRequest.ID] = struct{}{}
			actionResult, actionErr := e.actions.RequestAction(ctx, request.AgentID, actionRequest)
			if actionErr != nil {
				// Delivery errors are ambiguous: the input may have happened.
				// Never turn transport uncertainty into a duplicate click.
				if ctxErr := ctx.Err(); ctxErr != nil {
					return result, fmt.Errorf("%s: контекст завершён при отправке действия: %w", methodCtx, ctxErr)
				}
				return result, fmt.Errorf("%s: шаг %d, попытка %d: не удалось отправить действие %q: %w", methodCtx, step+1, attempt+1, actionRequest.ID, actionErr)
			}
			if err := ctx.Err(); err != nil {
				return result, fmt.Errorf("%s: контекст завершён после действия: %w", methodCtx, err)
			}
			if err := validateActionResult(actionRequest, transition, actionResult, requiredConfidence); err != nil {
				return result, fmt.Errorf("%s: шаг %d, попытка %d: %w", methodCtx, step+1, attempt+1, err)
			}

			afterFrame := max(result.Frame.ID, actionResult.ResultFrame)
			frame, observation, observeErr := e.observations.ObserveAfter(ctx, request.AgentID, afterFrame)
			if observeErr != nil {
				if ctxErr := ctx.Err(); ctxErr != nil {
					return result, fmt.Errorf("%s: контекст завершён при повторном наблюдении: %w", methodCtx, ctxErr)
				}
				return result, fmt.Errorf("%s: шаг %d, попытка %d: повторное наблюдение: %w", methodCtx, step+1, attempt+1, observeErr)
			}
			if err := ctx.Err(); err != nil {
				return result, fmt.Errorf("%s: контекст завершён после повторного наблюдения: %w", methodCtx, err)
			}
			attemptRecord := Attempt{
				Step: step, Attempt: attempt, Request: actionRequest, Result: actionResult,
				Frame: frame, Observation: observation,
			}
			result.Attempts = append(result.Attempts, attemptRecord)
			result.Frame, result.Observation = frame, observation

			if err := e.validateSnapshot(frame, observation, afterFrame, requiredConfidence); err != nil {
				return result, fmt.Errorf("%s: шаг %d, попытка %d: %w", methodCtx, step+1, attempt+1, err)
			}
			if observation.State == transition.To {
				if !actionResult.Success {
					return result, fmt.Errorf(
						"%s: шаг %d, попытка %d: %w: Windows сообщил %q, хотя наблюдатель видит %s",
						methodCtx, step+1, attempt+1, ErrActionRejected, actionResult.Error, observation.State,
					)
				}
				completed = true
				result.CompletedTransitions++
				break
			}
			if observation.State != transition.From {
				return result, fmt.Errorf(
					"%s: шаг %d, попытка %d: %w: ожидалось %s, получено %s",
					methodCtx, step+1, attempt+1, ErrStateMismatch, transition.To, observation.State,
				)
			}
			if !canRetry(transition, attempt, actionResult) {
				if !actionResult.Success {
					return result, fmt.Errorf(
						"%s: шаг %d, попытка %d: %w: причина: %s",
						methodCtx, step+1, attempt+1, ErrActionRejected, actionResult.Error,
					)
				}
				return result, fmt.Errorf(
					"%s: шаг %d, попытка %d: %w: Windows подтвердил %s, наблюдатель видит %s",
					methodCtx, step+1, attempt+1, ErrStateMismatch, actionResult.ResultState, observation.State,
				)
			}
		}
		if !completed {
			return result, fmt.Errorf("%s: шаг %d: %w", methodCtx, step+1, ErrStateMismatch)
		}
	}
	return result, nil
}

func (e *Executor) validateRequest(request Request) error {
	const methodCtx = "navigation.Executor.validateRequest"

	if strings.TrimSpace(request.AgentID) == "" {
		return fmt.Errorf("%s: %w: идентификатор агента не задан", methodCtx, ErrInvalidRequest)
	}
	if strings.TrimSpace(request.SessionID) == "" {
		return fmt.Errorf("%s: %w: идентификатор сессии не задан", methodCtx, ErrInvalidRequest)
	}
	if request.Geometry.Width <= 0 || request.Geometry.Height <= 0 || request.Geometry.DPIPercent <= 0 {
		return fmt.Errorf("%s: %w: ожидаемая геометрия должна быть полностью задана", methodCtx, ErrInvalidRequest)
	}
	for index, transition := range request.Path {
		if transition.From == "" || transition.To == "" ||
			transition.From == domain.StateUnknown || transition.To == domain.StateUnknown {
			return fmt.Errorf("%s: %w: шаг %d использует пустое или UNKNOWN-состояние", methodCtx, ErrInvalidRequest, index+1)
		}
		if transition.Verify.State != transition.To {
			return fmt.Errorf("%s: %w: шаг %d проверяет %s вместо %s", methodCtx, ErrInvalidRequest, index+1, transition.Verify.State, transition.To)
		}
		switch normalizedClass(transition.Class) {
		case protocol.ActionNavigation, protocol.ActionPurchase, protocol.ActionBarter,
			protocol.ActionListing, protocol.ActionReprice:
		default:
			return fmt.Errorf("%s: %w: шаг %d содержит неизвестный класс %q", methodCtx, ErrInvalidRequest, index+1, transition.Class)
		}
		if normalizedClass(transition.Class) != protocol.ActionNavigation && transition.MaxRetry != 0 {
			return fmt.Errorf("%s: %w: денежный шаг %d не может автоматически повторяться", methodCtx, ErrInvalidRequest, index+1)
		}
		if normalizedClass(transition.Class) != protocol.ActionNavigation {
			if err := validateMonetaryAction(transition.Action); err != nil {
				return fmt.Errorf(
					"%s: %w: денежный шаг %d: %v",
					methodCtx,
					ErrInvalidRequest,
					index+1,
					err,
				)
			}
		}
		if !validConfidence(transition.Verify.MinConfidence) {
			return fmt.Errorf("%s: %w: шаг %d содержит некорректную уверенность", methodCtx, ErrInvalidRequest, index+1)
		}
		if transition.Verify.Timeout < 0 {
			return fmt.Errorf("%s: %w: шаг %d содержит отрицательный срок проверки", methodCtx, ErrInvalidRequest, index+1)
		}
		if transition.Verify.BBox != nil {
			return fmt.Errorf(
				"%s: %w: шаг %d содержит неподдерживаемую область проверки; verification bbox должна быть пустой",
				methodCtx,
				ErrInvalidRequest,
				index+1,
			)
		}
		if transition.MaxRetry < 0 || transition.MaxRetry > e.config.MaxRetryLimit {
			return fmt.Errorf(
				"%s: %w: шаг %d имеет число повторов %d вне диапазона [0, %d]",
				methodCtx, ErrInvalidRequest, index+1, transition.MaxRetry, e.config.MaxRetryLimit,
			)
		}
		if index > 0 && request.Path[index-1].To != transition.From {
			return fmt.Errorf(
				"%s: %w: разрыв между шагами %d и %d (%s != %s)",
				methodCtx, ErrInvalidRequest, index, index+1, request.Path[index-1].To, transition.From,
			)
		}
		if err := validateAction(transition.Action); err != nil {
			return fmt.Errorf("%s: %w: шаг %d: %v", methodCtx, ErrInvalidRequest, index+1, err)
		}
	}
	if len(request.Path) > 0 && request.Observation.State != request.Path[0].From {
		if request.Observation.State == "" || request.Observation.State == domain.StateUnknown {
			return fmt.Errorf("%s: начальное состояние неизвестно: %w", methodCtx, ErrUnknownState)
		}
		return fmt.Errorf(
			"%s: %w: начальное состояние %s, маршрут начинается с %s",
			methodCtx, ErrStateMismatch, request.Observation.State, request.Path[0].From,
		)
	}
	return nil
}

func (e *Executor) actionRequest(
	ctx context.Context,
	request Request,
	transition Transition,
	frame protocol.Frame,
	observation domain.Observation,
	step, attempt int,
) (protocol.ActionRequest, error) {
	const methodCtx = "navigation.Executor.actionRequest"

	now := e.config.Now()
	if err := ctx.Err(); err != nil {
		return protocol.ActionRequest{}, fmt.Errorf("%s: контекст завершён: %w", methodCtx, err)
	}
	actionTimeout := e.config.ActionTimeout
	if transition.Verify.Timeout > 0 {
		actionTimeout = transition.Verify.Timeout
	}
	deadline := now.Add(actionTimeout)
	if contextDeadline, exists := ctx.Deadline(); exists && contextDeadline.Before(deadline) {
		deadline = contextDeadline
	}
	if !deadline.After(now) {
		if err := ctx.Err(); err != nil {
			return protocol.ActionRequest{}, fmt.Errorf("%s: контекст завершён: %w", methodCtx, err)
		}
		return protocol.ActionRequest{}, fmt.Errorf("%s: срок действия команды истёк: %w", methodCtx, context.DeadlineExceeded)
	}
	id := strings.TrimSpace(e.config.NewActionID(request.SessionID, step, attempt))
	if id == "" {
		return protocol.ActionRequest{}, fmt.Errorf("%s: %w: генератор вернул пустой идентификатор действия", methodCtx, ErrInvalidRequest)
	}
	frame, err := protocol.NormalizeFrameDigest(frame)
	if err != nil {
		return protocol.ActionRequest{}, fmt.Errorf("%s: исходный кадр не прошёл проверку digest: %w", methodCtx, err)
	}
	if frame.CapturedAt.IsZero() || frame.ContentDigest == "" {
		return protocol.ActionRequest{}, fmt.Errorf("%s: %w: исходный кадр не содержит метаданные свежести", methodCtx, ErrStaleObservation)
	}
	regions, err := observationBasisRegions(observation, transition.Action)
	if err != nil {
		return protocol.ActionRequest{}, fmt.Errorf(
			"%s: %w: не удалось собрать ROI исходного наблюдения: %v",
			methodCtx,
			ErrInvalidRequest,
			err,
		)
	}
	frameBasis, err := protocol.BuildFrameRegionBasis(frame, regions)
	if err != nil {
		return protocol.ActionRequest{}, fmt.Errorf(
			"%s: %w: не удалось построить ROI-основание исходного кадра: %v",
			methodCtx,
			ErrInvalidRequest,
			err,
		)
	}
	class := normalizedClass(transition.Class)
	if class != protocol.ActionNavigation && len(frameBasis) == 0 {
		return protocol.ActionRequest{}, fmt.Errorf(
			"%s: %w: денежное действие класса %s требует непустое ROI-основание",
			methodCtx,
			ErrInvalidRequest,
			class,
		)
	}
	basedOnCapturedAt := frame.CapturedAt
	return protocol.ActionRequest{
		ID:                        id,
		SessionID:                 request.SessionID,
		BasedOnFrame:              frame.ID,
		BasedOnCapturedAt:         &basedOnCapturedAt,
		BasedOnFrameDigest:        frame.ContentDigest,
		FrameBasis:                frameBasis,
		BasedOnState:              transition.From,
		ExpectedState:             transition.Verify.State,
		MinVerificationConfidence: math.Max(e.config.MinConfidence, transition.Verify.MinConfidence),
		ExpectedWidth:             request.Geometry.Width,
		ExpectedHeight:            request.Geometry.Height,
		ExpectedDPIPercent:        request.Geometry.DPIPercent,
		Class:                     class,
		Deadline:                  deadline,
		Action:                    normalizedAction(transition.Action),
	}, nil
}

func observationBasisRegions(
	observation domain.Observation,
	action protocol.Action,
) ([]domain.Rectangle, error) {
	const methodCtx = "navigation.observationBasisRegions"

	const maxObservationBasisCandidates = 256
	if len(observation.Values) > maxObservationBasisCandidates ||
		len(observation.Elements) > maxObservationBasisCandidates {
		return nil, fmt.Errorf(
			"%s: наблюдение содержит слишком много значений (%d) или элементов (%d), лимит %d",
			methodCtx,
			len(observation.Values),
			len(observation.Elements),
			maxObservationBasisCandidates,
		)
	}
	type rectangleKey [4]uint64
	seen := make(map[rectangleKey]struct{}, protocol.MaxFrameBasisRegions)
	regions := make([]domain.Rectangle, 0, protocol.MaxFrameBasisRegions)
	add := func(region domain.Rectangle) error {
		const methodCtx = "navigation.observationBasisRegions.add"

		key := rectangleKey{
			math.Float64bits(region.X),
			math.Float64bits(region.Y),
			math.Float64bits(region.Width),
			math.Float64bits(region.Height),
		}
		if _, exists := seen[key]; exists {
			return nil
		}
		seen[key] = struct{}{}
		if len(regions) == protocol.MaxFrameBasisRegions {
			return fmt.Errorf(
				"%s: число уникальных областей наблюдения превышает лимит %d",
				methodCtx,
				protocol.MaxFrameBasisRegions,
			)
		}
		regions = append(regions, region)
		return nil
	}
	for _, value := range observation.Values {
		if zeroRectangle(value.Region) {
			continue
		}
		if err := add(value.Region); err != nil {
			return nil, err
		}
	}
	actionPoints := basisActionPoints(action)
	for _, element := range observation.Elements {
		if zeroRectangle(element.Region) ||
			!regionContainsAnyPoint(element.Region, actionPoints) {
			continue
		}
		if err := add(element.Region); err != nil {
			return nil, err
		}
	}
	return regions, nil
}

func basisActionPoints(action protocol.Action) []domain.Point {
	points := make([]domain.Point, 0, 1)
	if action.Point != nil {
		points = append(points, *action.Point)
	}
	for _, step := range action.Steps {
		points = append(points, basisActionPoints(step)...)
	}
	return points
}

func regionContainsAnyPoint(
	region domain.Rectangle,
	points []domain.Point,
) bool {
	for _, point := range points {
		if point.X >= region.X && point.X <= region.X+region.Width &&
			point.Y >= region.Y && point.Y <= region.Y+region.Height {
			return true
		}
	}
	return false
}

func zeroRectangle(region domain.Rectangle) bool {
	return region.X == 0 && region.Y == 0 && region.Width == 0 && region.Height == 0
}

func normalizedClass(value protocol.ActionClass) protocol.ActionClass {
	if value == "" {
		return protocol.ActionNavigation
	}
	return value
}

func (e *Executor) validateSnapshot(
	frame protocol.Frame,
	observation domain.Observation,
	afterFrame uint64,
	minConfidence float64,
) error {
	const methodCtx = "navigation.Executor.validateSnapshot"

	now := e.config.Now()
	if frame.ID == 0 || observation.FrameID != frame.ID {
		return fmt.Errorf(
			"%s: %w: идентификатор кадра %d, идентификатор кадра наблюдения %d",
			methodCtx, ErrStaleObservation, frame.ID, observation.FrameID,
		)
	}
	if strings.TrimSpace(frame.Encoding) == "" || len(frame.Data) == 0 {
		return fmt.Errorf("%s: %w: кадр не содержит закодированного изображения", methodCtx, ErrStaleObservation)
	}
	if frame.ID <= afterFrame {
		return fmt.Errorf("%s: %w: идентификатор кадра %d не новее %d", methodCtx, ErrStaleObservation, frame.ID, afterFrame)
	}
	if frame.CapturedAt.IsZero() || observation.CreatedAt.IsZero() {
		return fmt.Errorf("%s: %w: отсутствуют временные метки", methodCtx, ErrStaleObservation)
	}
	if observation.CreatedAt.Before(frame.CapturedAt) {
		return fmt.Errorf("%s: %w: наблюдение создано раньше кадра", methodCtx, ErrStaleObservation)
	}
	if isStale(now, frame.CapturedAt, e.config.ObservationMaxAge) ||
		isStale(now, observation.CreatedAt, e.config.ObservationMaxAge) {
		return fmt.Errorf("%s: %w: кадр или наблюдение старше %s", methodCtx, ErrStaleObservation, e.config.ObservationMaxAge)
	}
	if err := validateState(observation, observation.State, minConfidence); err != nil {
		return fmt.Errorf("%s: состояние снимка не прошло проверку: %w", methodCtx, err)
	}
	return nil
}

func validateState(observation domain.Observation, expected domain.ScreenState, minConfidence float64) error {
	const methodCtx = "navigation.validateState"

	if observation.State == "" || observation.State == domain.StateUnknown {
		return fmt.Errorf("%s: проверка состояния: %w", methodCtx, ErrUnknownState)
	}
	if !validConfidence(observation.Confidence) || observation.Confidence < minConfidence {
		return fmt.Errorf(
			"%s: %w: уверенность %.3f, требуется %.3f",
			methodCtx, ErrLowConfidence, observation.Confidence, minConfidence,
		)
	}
	if observation.State != expected {
		return fmt.Errorf("%s: %w: ожидалось %s, получено %s", methodCtx, ErrStateMismatch, expected, observation.State)
	}
	return nil
}

func validateActionResult(
	request protocol.ActionRequest,
	transition Transition,
	result protocol.ActionResult,
	minConfidence float64,
) error {
	const methodCtx = "navigation.validateActionResult"

	if result.ID != request.ID {
		return fmt.Errorf("%s: %w: идентификатор действия %q, идентификатор результата %q", methodCtx, ErrInvalidActionResult, request.ID, result.ID)
	}
	if result.CompletedAt.IsZero() {
		return fmt.Errorf("%s: %w: отсутствует completed_at", methodCtx, ErrInvalidActionResult)
	}
	if result.CompletedAt.After(request.Deadline) {
		return fmt.Errorf("%s: %w: результат получен после срока действия команды", methodCtx, ErrInvalidActionResult)
	}
	if result.ResultFrame != 0 && result.ResultFrame <= request.BasedOnFrame {
		return fmt.Errorf(
			"%s: %w: кадр результата %d не новее исходного кадра %d",
			methodCtx, ErrInvalidActionResult, result.ResultFrame, request.BasedOnFrame,
		)
	}
	if result.ResultFrame != 0 {
		if result.Frame == nil || result.Frame.ID != result.ResultFrame {
			return fmt.Errorf("%s: %w: отсутствует точный контрольный кадр", methodCtx, ErrInvalidActionResult)
		}
		if result.Frame.CapturedAt.IsZero() || strings.TrimSpace(result.Frame.Encoding) == "" ||
			len(result.Frame.Data) == 0 {
			return fmt.Errorf("%s: %w: контрольный кадр неполон", methodCtx, ErrInvalidActionResult)
		}
		if result.Frame.CapturedAt.After(result.CompletedAt) {
			return fmt.Errorf("%s: %w: кадр снят после completed_at", methodCtx, ErrInvalidActionResult)
		}
		if !validConfidence(result.VerificationConfidence) ||
			result.VerificationConfidence < minConfidence {
			return fmt.Errorf(
				"%s: %w: уверенность Windows %.3f, требуется %.3f",
				methodCtx,
				ErrInvalidActionResult,
				result.VerificationConfidence,
				minConfidence,
			)
		}
	}
	if result.Success {
		if result.Error != "" {
			return fmt.Errorf("%s: %w: успешный результат содержит ошибку", methodCtx, ErrInvalidActionResult)
		}
		if result.ResultFrame == 0 {
			return fmt.Errorf("%s: %w: успешный результат не содержит кадр", methodCtx, ErrInvalidActionResult)
		}
		if result.ResultState != transition.To {
			return fmt.Errorf(
				"%s: %w: Windows подтвердил %s вместо %s",
				methodCtx, ErrInvalidActionResult, result.ResultState, transition.To,
			)
		}
		return nil
	}
	if strings.TrimSpace(result.Error) == "" {
		return fmt.Errorf("%s: %w: неуспешный результат не содержит причины", methodCtx, ErrInvalidActionResult)
	}
	if result.ResultFrame != 0 && (result.ResultState == "" || result.ResultState == domain.StateUnknown) {
		return fmt.Errorf("%s: %w: Windows вернул UNKNOWN-состояние", methodCtx, ErrInvalidActionResult)
	}
	return nil
}

// canRetry is deliberately narrow: Windows must have captured a newer frame
// and conclusively found the unchanged source state. The fresh observer is
// checked separately and must agree. Success/transport ambiguity is never
// retried.
func canRetry(transition Transition, attempt int, result protocol.ActionResult) bool {
	return attempt < transition.MaxRetry &&
		!result.Success &&
		result.RetrySafe &&
		result.ResultFrame != 0 &&
		result.ResultState == transition.From
}

func validateAction(action protocol.Action) error {
	const methodCtx = "navigation.validateAction"

	switch normalizedAction(action).Kind {
	case "MOVE":
		if action.Point == nil || !validPoint(*action.Point) {
			return fmt.Errorf("%s: MOVE требует нормализованную координату", methodCtx)
		}
	case "CLICK":
		if action.Point != nil && !validPoint(*action.Point) {
			return fmt.Errorf("%s: CLICK содержит некорректную нормализованную координату", methodCtx)
		}
		switch strings.ToUpper(strings.TrimSpace(action.Value)) {
		case "", "LEFT", "PRIMARY", "RIGHT", "SECONDARY", "MIDDLE", "X1", "X2":
		default:
			return fmt.Errorf("%s: неподдерживаемая кнопка %q", methodCtx, action.Value)
		}
	case "SCROLL":
		if action.Point != nil && !validPoint(*action.Point) {
			return fmt.Errorf("%s: SCROLL содержит некорректную нормализованную координату", methodCtx)
		}
		if action.Delta == 0 {
			return fmt.Errorf("%s: SCROLL требует ненулевое смещение", methodCtx)
		}
	case "KEY", "TEXT":
		if strings.TrimSpace(action.Value) == "" {
			return fmt.Errorf("%s: действие %s требует значение", methodCtx, action.Kind)
		}
	case "SEQUENCE":
		if len(action.Steps) == 0 || len(action.Steps) > 64 {
			return fmt.Errorf("%s: SEQUENCE должна содержать от 1 до 64 шагов", methodCtx)
		}
		for index, step := range action.Steps {
			if strings.EqualFold(strings.TrimSpace(step.Kind), "SEQUENCE") {
				return fmt.Errorf("%s: вложенная SEQUENCE запрещена", methodCtx)
			}
			if err := validateAction(step); err != nil {
				return fmt.Errorf("%s: шаг %d последовательности: %w", methodCtx, index+1, err)
			}
		}
	default:
		return fmt.Errorf("%s: неподдерживаемое действие %q", methodCtx, action.Kind)
	}
	return nil
}

func normalizedAction(action protocol.Action) protocol.Action {
	action.Kind = strings.ToUpper(strings.TrimSpace(action.Kind))
	if action.Point != nil {
		point := *action.Point
		action.Point = &point
	}
	if action.Steps != nil {
		action.Steps = append([]protocol.Action(nil), action.Steps...)
		for index := range action.Steps {
			action.Steps[index] = normalizedAction(action.Steps[index])
		}
	}
	return action
}

func clonePath(path Path) Path {
	result := append(Path(nil), path...)
	for index := range result {
		result[index] = cloneTransition(result[index])
	}
	return result
}

func cloneTransition(value Transition) Transition {
	value.Action = normalizedAction(value.Action)
	if value.Verify.BBox != nil {
		bbox := *value.Verify.BBox
		value.Verify.BBox = &bbox
	}
	return value
}

func validPoint(point domain.Point) bool {
	return point.X >= 0 && point.X <= 1 && point.Y >= 0 && point.Y <= 1
}

func validRectangle(value domain.Rectangle) bool {
	return !math.IsNaN(value.X) && !math.IsInf(value.X, 0) &&
		!math.IsNaN(value.Y) && !math.IsInf(value.Y, 0) &&
		!math.IsNaN(value.Width) && !math.IsInf(value.Width, 0) &&
		!math.IsNaN(value.Height) && !math.IsInf(value.Height, 0) &&
		value.X >= 0 && value.Y >= 0 &&
		value.Width > 0 && value.Height > 0 &&
		value.X+value.Width <= 1 &&
		value.Y+value.Height <= 1
}

func validConfidence(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value >= 0 && value <= 1
}

func isStale(now, timestamp time.Time, maxAge time.Duration) bool {
	if timestamp.After(now) {
		return true
	}
	return now.Sub(timestamp) > maxAge
}

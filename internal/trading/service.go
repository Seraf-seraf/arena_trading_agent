package trading

import (
	"errors"
	"fmt"
	"math"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/arena-trading-agent/arena-trading-agent/internal/domain"
	"github.com/arena-trading-agent/arena-trading-agent/internal/economy"
	"github.com/arena-trading-agent/arena-trading-agent/internal/inventory"
	"github.com/arena-trading-agent/arena-trading-agent/internal/money"
	"github.com/arena-trading-agent/arena-trading-agent/internal/opportunity"
	"github.com/arena-trading-agent/arena-trading-agent/internal/reconciliation"
	"github.com/arena-trading-agent/arena-trading-agent/internal/risk"
)

var (
	// ErrNotFound means a saga ID is unknown to this service instance.
	ErrNotFound = errors.New("торговая сага не найдена")
	// ErrActiveSaga enforces the v1 invariant of one trading transaction at a time.
	ErrActiveSaga = errors.New("другая торговая сага уже активна")
	// ErrIdempotencyConflict means an event/request ID was reused with other data.
	ErrIdempotencyConflict = errors.New("конфликт ключа идемпотентности")
	// ErrInvalidTransition means a verified event does not fit the current state.
	ErrInvalidTransition = errors.New("некорректный переход торговой саги")
	// ErrInvalidCheckpoint rejects incomplete or internally inconsistent
	// durable state instead of attempting a best-effort monetary recovery.
	ErrInvalidCheckpoint = errors.New("некорректная контрольная точка торговли")
)

const checkpointSchemaVersion = 1

type processedEvent struct {
	event    Event
	snapshot Saga
}

type sagaState struct {
	snapshot  Saga
	limits    domain.RiskLimits
	holdings  map[string]int64
	processed map[string]processedEvent
}

// Service connects discovery, economic normalization, risk checks, inventory
// accounting and reconciliation. It never invokes an action or transport API.
type Service struct {
	mu        sync.Mutex
	finder    *opportunity.Finder
	inventory *inventory.Tracker
	risk      risk.Manager
	config    Config
	sagas     map[string]*sagaState
	activeID  string
}

// NewService validates dependencies and creates an isolated in-memory
// orchestrator. Persistence can store returned snapshots without becoming
// part of the transition rules.
func NewService(
	finder *opportunity.Finder,
	tracker *inventory.Tracker,
	config Config,
) (*Service, error) {
	const methodCtx = "trading.NewService"

	if finder == nil {
		return nil, fmt.Errorf("%s: поиск торговых возможностей не задан", methodCtx)
	}
	if tracker == nil {
		return nil, fmt.Errorf("%s: трекер инвентаря не задан", methodCtx)
	}
	if config.Clock == nil {
		config.Clock = time.Now
	}
	if config.ProfitScale == 0 {
		config.ProfitScale = 1
	}
	if !finitePositive(config.ProfitScale) {
		return nil, fmt.Errorf("%s: масштаб прибыли должен быть конечным и положительным", methodCtx)
	}
	if config.MaxStepAttempts < 0 {
		return nil, fmt.Errorf("%s: максимальное число попыток шага не может быть отрицательным", methodCtx)
	}
	if config.MaxStepAttempts == 0 {
		config.MaxStepAttempts = 3
	}
	if err := validateWeights(config.ScoreWeights); err != nil {
		return nil, fmt.Errorf("%s: некорректные веса оценки: %w", methodCtx, err)
	}
	return &Service{
		finder:    finder,
		inventory: tracker,
		risk:      risk.NewManagerWithClock(config.Clock),
		config:    config,
		sagas:     make(map[string]*sagaState),
	}, nil
}

// Evaluate discovers all opportunities, canonicalizes their financials and
// applies current inventory capacity plus mandatory risk limits.
func (s *Service) Evaluate(
	input opportunity.Input,
	limits domain.RiskLimits,
) (Evaluation, error) {
	const methodCtx = "trading.Service.Evaluate"

	if s == nil {
		return Evaluation{}, fmt.Errorf("%s: торговый сервис не задан", methodCtx)
	}
	values, err := s.finder.Find(input)
	if err != nil {
		return Evaluation{}, fmt.Errorf("%s: не удалось найти торговые возможности: %w", methodCtx, err)
	}
	limits.AvailableSlots = min(limits.AvailableSlots, s.inventory.FreeSlots())
	result := Evaluation{
		Candidates: make([]Candidate, 0, len(values)),
		Rejected:   make([]Rejection, 0),
	}
	for _, value := range values {
		canonical, err := canonicalOpportunity(value)
		if err != nil {
			return Evaluation{}, fmt.Errorf("%s: найдена некорректная возможность %q: %w", methodCtx, value.ID, err)
		}
		if err := s.risk.Validate(canonical, limits); err != nil {
			result.Rejected = append(result.Rejected, Rejection{
				OpportunityID: canonical.ID,
				Reason:        err.Error(),
			})
			continue
		}
		score := economy.Score(canonical, s.config.ScoreWeights, s.config.ProfitScale)
		if math.IsNaN(score) || math.IsInf(score, 0) {
			return Evaluation{}, fmt.Errorf("%s: оценка возможности %q не является конечной", methodCtx, canonical.ID)
		}
		result.Candidates = append(result.Candidates, Candidate{
			Opportunity: cloneOpportunity(canonical),
			Score:       score,
		})
	}
	sort.Slice(result.Candidates, func(i, j int) bool {
		left, right := result.Candidates[i], result.Candidates[j]
		if left.Score != right.Score {
			return left.Score > right.Score
		}
		if left.Opportunity.ExpectedProfit != right.Opportunity.ExpectedProfit {
			return left.Opportunity.ExpectedProfit > right.Opportunity.ExpectedProfit
		}
		if left.Opportunity.InputCost != right.Opportunity.InputCost {
			return left.Opportunity.InputCost < right.Opportunity.InputCost
		}
		return left.Opportunity.ID < right.Opportunity.ID
	})
	sort.Slice(result.Rejected, func(i, j int) bool {
		return result.Rejected[i].OpportunityID < result.Rejected[j].OpportunityID
	})
	return result, nil
}

// Begin reserves the opportunity budget logically and starts a saga. Repeating
// the exact request is idempotent; changing data under the same ID is rejected.
func (s *Service) Begin(
	executionID string,
	value domain.TradeOpportunity,
	limits domain.RiskLimits,
) (Saga, error) {
	const methodCtx = "trading.Service.Begin"

	if s == nil {
		return Saga{}, fmt.Errorf("%s: торговый сервис не задан", methodCtx)
	}
	if executionID == "" {
		return Saga{}, fmt.Errorf("%s: идентификатор исполнения обязателен", methodCtx)
	}
	canonical, err := canonicalOpportunity(value)
	if err != nil {
		return Saga{}, fmt.Errorf("%s: некорректная торговая возможность: %w", methodCtx, err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if existing, ok := s.sagas[executionID]; ok {
		if !reflect.DeepEqual(existing.snapshot.Opportunity, canonical) ||
			!reflect.DeepEqual(existing.limits, limits) {
			return Saga{}, fmt.Errorf("%s: %w: исполнение %q", methodCtx, ErrIdempotencyConflict, executionID)
		}
		return cloneSaga(existing.snapshot), nil
	}
	if s.activeID != "" {
		return Saga{}, fmt.Errorf("%s: %w: идентификатор %q", methodCtx, ErrActiveSaga, s.activeID)
	}
	effectiveLimits := limits
	effectiveLimits.AvailableSlots = min(effectiveLimits.AvailableSlots, s.inventory.FreeSlots())
	if err := s.risk.Validate(canonical, effectiveLimits); err != nil {
		return Saga{}, fmt.Errorf("%s: проверка риска отклонила возможность %q: %w", methodCtx, canonical.ID, err)
	}

	now := s.now()
	state := &sagaState{
		snapshot: Saga{
			ID:             executionID,
			Opportunity:    cloneOpportunity(canonical),
			Status:         SagaRunning,
			ReservedBudget: canonical.InputCost,
			Version:        1,
			StartedAt:      now,
			UpdatedAt:      now,
		},
		limits:    limits,
		holdings:  make(map[string]int64),
		processed: make(map[string]processedEvent),
	}
	s.sagas[executionID] = state
	s.activeID = executionID
	return cloneSaga(state.snapshot), nil
}

// Get returns a defensive snapshot.
func (s *Service) Get(executionID string) (Saga, error) {
	const methodCtx = "trading.Service.Get"

	if s == nil {
		return Saga{}, fmt.Errorf("%s: торговый сервис не задан", methodCtx)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	state, ok := s.sagas[executionID]
	if !ok {
		return Saga{}, fmt.Errorf("%s: %w: идентификатор %q", methodCtx, ErrNotFound, executionID)
	}
	return cloneSaga(state.snapshot), nil
}

// SynchronizeInventory installs a complete verified account snapshot before a
// new saga. Replacing inventory while a saga is active would erase attribution
// of partial holdings, so that case is rejected.
func (s *Service) SynchronizeInventory(snapshot domain.InventorySnapshot) error {
	const methodCtx = "trading.Service.SynchronizeInventory"

	if s == nil {
		return fmt.Errorf("%s: торговый сервис не задан", methodCtx)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.activeID != "" {
		return fmt.Errorf(
			"%s: нельзя заменить инвентарь во время активной саги %q: %w",
			methodCtx,
			s.activeID,
			ErrActiveSaga,
		)
	}
	if err := s.inventory.ReplaceSnapshot(snapshot); err != nil {
		return fmt.Errorf("%s: проверенный снимок инвентаря отклонён: %w", methodCtx, err)
	}
	return nil
}

// InventorySnapshot returns the current deterministic tracker state.
func (s *Service) InventorySnapshot() domain.InventorySnapshot {
	if s == nil {
		return domain.InventorySnapshot{}
	}
	return s.inventory.Snapshot()
}

// Checkpoint returns a complete, defensive durable representation of a saga.
func (s *Service) Checkpoint(executionID string) (Checkpoint, error) {
	const methodCtx = "trading.Service.Checkpoint"

	if s == nil {
		return Checkpoint{}, fmt.Errorf("%s: торговый сервис не задан", methodCtx)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	state, ok := s.sagas[executionID]
	if !ok {
		return Checkpoint{}, fmt.Errorf("%s: %w: идентификатор %q", methodCtx, ErrNotFound, executionID)
	}
	processed := make([]ProcessedEventCheckpoint, 0, len(state.processed))
	for _, record := range state.processed {
		processed = append(processed, ProcessedEventCheckpoint{
			Event:    cloneEvent(record.event),
			Snapshot: cloneSaga(record.snapshot),
		})
	}
	sort.Slice(processed, func(i, j int) bool {
		if processed[i].Snapshot.Version != processed[j].Snapshot.Version {
			return processed[i].Snapshot.Version < processed[j].Snapshot.Version
		}
		return processed[i].Event.ID < processed[j].Event.ID
	})
	return Checkpoint{
		SchemaVersion: checkpointSchemaVersion,
		Saga:          cloneSaga(state.snapshot),
		Limits:        state.limits,
		Inventory:     s.inventory.Snapshot(),
		Processed:     processed,
	}, nil
}

// Restore installs a previously persisted checkpoint without replaying game
// actions. Event idempotency and the exact inventory baseline are restored.
func (s *Service) Restore(checkpoint Checkpoint) (Saga, error) {
	const methodCtx = "trading.Service.Restore"

	if s == nil {
		return Saga{}, fmt.Errorf("%s: торговый сервис не задан", methodCtx)
	}
	checkpoint = cloneCheckpoint(checkpoint)
	if err := ValidateCheckpointStrict(checkpoint, s.config.MaxStepAttempts); err != nil {
		return Saga{}, fmt.Errorf("%s: %w: причина: %v", methodCtx, ErrInvalidCheckpoint, err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, exists := s.sagas[checkpoint.Saga.ID]; exists {
		if reflect.DeepEqual(existing.snapshot, checkpoint.Saga) {
			return cloneSaga(existing.snapshot), nil
		}
		return Saga{}, fmt.Errorf("%s: %w: сага %q уже существует", methodCtx, ErrIdempotencyConflict, checkpoint.Saga.ID)
	}
	if s.activeID != "" && !isTerminal(checkpoint.Saga.Status) {
		return Saga{}, fmt.Errorf("%s: %w: идентификатор %q", methodCtx, ErrActiveSaga, s.activeID)
	}
	items := append([]domain.InventoryItem(nil), checkpoint.Inventory.Items...)
	for index := range items {
		if items[index].ReservedQuantity != 0 {
			return Saga{}, fmt.Errorf(
				"%s: %w: инвентарь содержит анонимные резервы",
				methodCtx,
				ErrInvalidCheckpoint,
			)
		}
	}
	checkpoint.Inventory.Items = items
	if err := s.inventory.ReplaceSnapshot(checkpoint.Inventory); err != nil {
		return Saga{}, fmt.Errorf("%s: %w: инвентарь: %v", methodCtx, ErrInvalidCheckpoint, err)
	}
	state := &sagaState{
		snapshot:  cloneSaga(checkpoint.Saga),
		limits:    checkpoint.Limits,
		holdings:  make(map[string]int64, len(checkpoint.Saga.Holdings)),
		processed: make(map[string]processedEvent, len(checkpoint.Processed)),
	}
	for _, holding := range checkpoint.Saga.Holdings {
		state.holdings[holding.ItemID] = holding.Quantity
	}
	for _, record := range checkpoint.Processed {
		state.processed[record.Event.ID] = processedEvent{
			event:    cloneEvent(record.Event),
			snapshot: cloneSaga(record.Snapshot),
		}
	}
	s.sagas[checkpoint.Saga.ID] = state
	if !isTerminal(checkpoint.Saga.Status) {
		s.activeID = checkpoint.Saga.ID
	}
	return cloneSaga(state.snapshot), nil
}

// Next returns the only legal next directive. It is pure and cannot mutate the
// saga or invoke UI input.
func (s *Service) Next(executionID string) (Directive, error) {
	const methodCtx = "trading.Service.Next"

	if s == nil {
		return Directive{}, fmt.Errorf("%s: торговый сервис не задан", methodCtx)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	state, ok := s.sagas[executionID]
	if !ok {
		return Directive{}, fmt.Errorf("%s: %w: идентификатор %q", methodCtx, ErrNotFound, executionID)
	}
	snapshot := state.snapshot
	key := fmt.Sprintf("%s:%d:%d", snapshot.ID, snapshot.CurrentStep, snapshot.Attempt)
	switch snapshot.Status {
	case SagaRunning:
		if snapshot.CurrentStep < 0 || snapshot.CurrentStep >= len(snapshot.Opportunity.Steps) {
			return Directive{}, fmt.Errorf("%s: %w: активная сага не содержит текущего шага", methodCtx, ErrInvalidTransition)
		}
		step := snapshot.Opportunity.Steps[snapshot.CurrentStep]
		remaining, err := money.Subtract(step.Quantity, snapshot.StepProgress)
		if err != nil || remaining <= 0 {
			return Directive{}, fmt.Errorf("%s: %w: некорректный прогресс шага", methodCtx, ErrInvalidTransition)
		}
		step.Quantity = remaining
		return Directive{
			Kind:           DirectiveExecuteStep,
			SagaID:         snapshot.ID,
			IdempotencyKey: key,
			StepIndex:      snapshot.CurrentStep,
			Step:           &step,
		}, nil
	case SagaRecovering, SagaWaitingCooldown, SagaWaitingMarketSlot:
		compensation := cloneCompensation(snapshot.Compensation)
		return Directive{
			Kind:           DirectiveRecover,
			SagaID:         snapshot.ID,
			IdempotencyKey: key,
			StepIndex:      snapshot.CurrentStep,
			Compensation:   compensation,
		}, nil
	case SagaAwaitingSale:
		return Directive{
			Kind:           DirectiveMonitorSale,
			SagaID:         snapshot.ID,
			IdempotencyKey: key,
			StepIndex:      snapshot.CurrentStep,
		}, nil
	default:
		return Directive{
			Kind:           DirectiveDone,
			SagaID:         snapshot.ID,
			IdempotencyKey: key,
			StepIndex:      snapshot.CurrentStep,
		}, nil
	}
}

// Apply accepts one verified fact atomically. Inventory is changed at most
// once because duplicate event IDs return the snapshot produced by the first
// application.
func (s *Service) Apply(event Event) (Saga, error) {
	const methodCtx = "trading.Service.Apply"

	if s == nil {
		return Saga{}, fmt.Errorf("%s: торговый сервис не задан", methodCtx)
	}
	event = cloneEvent(event)
	if event.ID == "" {
		return Saga{}, fmt.Errorf("%s: идентификатор события обязателен", methodCtx)
	}
	if event.SagaID == "" {
		return Saga{}, fmt.Errorf("%s: идентификатор саги обязателен", methodCtx)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	current, ok := s.sagas[event.SagaID]
	if !ok {
		return Saga{}, fmt.Errorf("%s: %w: идентификатор %q", methodCtx, ErrNotFound, event.SagaID)
	}
	if previous, duplicate := current.processed[event.ID]; duplicate {
		replay := normalizeReplayEvent(event, previous.event)
		if !reflect.DeepEqual(previous.event, replay) {
			return Saga{}, fmt.Errorf("%s: %w: событие %q", methodCtx, ErrIdempotencyConflict, event.ID)
		}
		return cloneSaga(previous.snapshot), nil
	}
	event = s.normalizeEvent(event)
	if monetaryActionID := event.Outcome.MonetaryActionID; monetaryActionID != "" {
		for _, processed := range current.processed {
			if processed.event.Outcome.MonetaryActionID == monetaryActionID {
				return Saga{}, fmt.Errorf(
					"%s: %w: денежное UI-действие %q уже связано с событием %q",
					methodCtx,
					ErrIdempotencyConflict,
					monetaryActionID,
					processed.event.ID,
				)
			}
		}
	}
	if event.OccurredAt.Before(current.snapshot.UpdatedAt) {
		return Saga{}, fmt.Errorf("%s: %w: событие %q старше состояния саги", methodCtx, ErrInvalidTransition, event.ID)
	}
	if current.snapshot.Version == math.MaxUint64 {
		return Saga{}, fmt.Errorf("%s: переполнение версии саги", methodCtx)
	}

	next := cloneState(current)
	var deltas []domain.InventoryDelta
	var err error
	switch event.Kind {
	case EventStepSucceeded:
		deltas, err = applyStepEvent(next, event, true)
	case EventStepFailed:
		deltas, err = applyStepEvent(next, event, false)
	case EventRecoveryResolved:
		deltas, err = applyRecoveryEvent(next, event, s.config.MaxStepAttempts)
	case EventSaleSettled:
		err = applySettlementEvent(next, event)
	default:
		err = fmt.Errorf("%w: неизвестный тип события %q", ErrInvalidTransition, event.Kind)
	}
	if err != nil {
		return Saga{}, fmt.Errorf("%s: не удалось применить событие: %w", methodCtx, err)
	}
	if len(deltas) > 0 {
		if err := s.inventory.Apply(deltas...); err != nil {
			return Saga{}, fmt.Errorf("%s: подтверждённое изменение инвентаря отклонено: %w", methodCtx, err)
		}
	}
	next.snapshot.Version++
	next.snapshot.UpdatedAt = event.OccurredAt
	next.snapshot.Holdings = sortedHoldings(next.holdings)
	if isTerminal(next.snapshot.Status) {
		next.snapshot.ReservedBudget = 0
		if s.activeID == next.snapshot.ID {
			s.activeID = ""
		}
	}
	result := cloneSaga(next.snapshot)
	next.processed[event.ID] = processedEvent{
		event:    cloneEvent(event),
		snapshot: cloneSaga(result),
	}
	s.sagas[event.SagaID] = next
	return result, nil
}

func (s *Service) now() time.Time {
	return s.config.Clock().UTC()
}

func (s *Service) normalizeEvent(value Event) Event {
	value = cloneEvent(value)
	value.Outcome.MonetaryActionID = strings.TrimSpace(value.Outcome.MonetaryActionID)
	value.Outcome.MarketOrderID = strings.TrimSpace(value.Outcome.MarketOrderID)
	if value.OccurredAt.IsZero() {
		value.OccurredAt = s.now()
	} else {
		value.OccurredAt = value.OccurredAt.UTC()
	}
	if value.Settlement != nil {
		value.Settlement.MarketOrderID = strings.TrimSpace(value.Settlement.MarketOrderID)
		if value.Settlement.CompletedAt.IsZero() {
			value.Settlement.CompletedAt = value.OccurredAt
		} else {
			value.Settlement.CompletedAt = value.Settlement.CompletedAt.UTC()
		}
	}
	return value
}

func normalizeReplayEvent(value, previous Event) Event {
	value = cloneEvent(value)
	value.Outcome.MonetaryActionID = strings.TrimSpace(value.Outcome.MonetaryActionID)
	value.Outcome.MarketOrderID = strings.TrimSpace(value.Outcome.MarketOrderID)
	if value.OccurredAt.IsZero() {
		value.OccurredAt = previous.OccurredAt
	} else {
		value.OccurredAt = value.OccurredAt.UTC()
	}
	if value.Settlement != nil {
		value.Settlement.MarketOrderID = strings.TrimSpace(value.Settlement.MarketOrderID)
		if value.Settlement.CompletedAt.IsZero() &&
			previous.Settlement != nil {
			value.Settlement.CompletedAt = previous.Settlement.CompletedAt
		} else {
			value.Settlement.CompletedAt = value.Settlement.CompletedAt.UTC()
		}
	}
	return value
}

func canonicalOpportunity(value domain.TradeOpportunity) (domain.TradeOpportunity, error) {
	const methodCtx = "trading.canonicalOpportunity"

	if value.ID == "" {
		return domain.TradeOpportunity{}, fmt.Errorf("%s: идентификатор торговой возможности обязателен", methodCtx)
	}
	if len(value.Steps) == 0 {
		return domain.TradeOpportunity{}, fmt.Errorf("%s: торговый план не содержит шагов", methodCtx)
	}
	if value.RequiredSlots <= 0 {
		return domain.TradeOpportunity{}, fmt.Errorf("%s: требуемое число слотов должно быть положительным", methodCtx)
	}
	if value.ResultItemID == "" || value.ResultQuantity <= 0 {
		return domain.TradeOpportunity{}, fmt.Errorf("%s: ожидаемый результат некорректен", methodCtx)
	}

	var buyCost int64
	var listRevenue int64
	var buyCount, barterCount, listCount int
	for index, step := range value.Steps {
		if step.Quantity <= 0 {
			return domain.TradeOpportunity{}, fmt.Errorf("%s: количество на шаге %d должно быть положительным", methodCtx, index)
		}
		switch step.Kind {
		case domain.TradeStepBuy:
			if step.ItemID == "" || step.RecipeID != "" || step.LimitPrice < 0 {
				return domain.TradeOpportunity{}, fmt.Errorf("%s: шаг покупки %d некорректен", methodCtx, index)
			}
			line, err := money.Multiply(step.LimitPrice, step.Quantity)
			if err != nil {
				return domain.TradeOpportunity{}, fmt.Errorf("%s: не удалось рассчитать стоимость шага покупки %d: %w", methodCtx, index, err)
			}
			buyCost, err = money.Add(buyCost, line)
			if err != nil {
				return domain.TradeOpportunity{}, fmt.Errorf("%s: переполнение плановой входной стоимости: %w", methodCtx, err)
			}
			buyCount++
		case domain.TradeStepBarter:
			if step.ItemID == "" || step.RecipeID == "" || step.LimitPrice != 0 {
				return domain.TradeOpportunity{}, fmt.Errorf("%s: шаг обмена %d некорректен", methodCtx, index)
			}
			barterCount++
		case domain.TradeStepList:
			if step.ItemID == "" || step.RecipeID != "" || step.LimitPrice < 0 ||
				index != len(value.Steps)-1 {
				return domain.TradeOpportunity{}, fmt.Errorf("%s: шаг выставления %d некорректен или не является последним", methodCtx, index)
			}
			line, err := money.Multiply(step.LimitPrice, step.Quantity)
			if err != nil {
				return domain.TradeOpportunity{}, fmt.Errorf("%s: не удалось рассчитать выручку шага выставления %d: %w", methodCtx, index, err)
			}
			listRevenue, err = money.Add(listRevenue, line)
			if err != nil {
				return domain.TradeOpportunity{}, fmt.Errorf("%s: переполнение плановой выручки: %w", methodCtx, err)
			}
			if step.ItemID != value.ResultItemID || step.Quantity != value.ResultQuantity {
				return domain.TradeOpportunity{}, fmt.Errorf("%s: финальное выставление не совпадает с ожидаемым результатом", methodCtx)
			}
			listCount++
		default:
			return domain.TradeOpportunity{}, fmt.Errorf("%s: шаг %d имеет неизвестный тип %q", methodCtx, index, step.Kind)
		}
	}
	if buyCount == 0 || listCount != 1 {
		return domain.TradeOpportunity{}, fmt.Errorf("%s: торговый план требует хотя бы одну покупку и ровно одно финальное выставление", methodCtx)
	}
	switch value.Kind {
	case domain.OpportunityDirectFlip:
		if barterCount != 0 {
			return domain.TradeOpportunity{}, fmt.Errorf("%s: прямая перепродажа содержит шаги обмена", methodCtx)
		}
	case domain.OpportunityContactBarter:
		if barterCount != 1 {
			return domain.TradeOpportunity{}, fmt.Errorf("%s: контактный обмен требует ровно один шаг обмена", methodCtx)
		}
	case domain.OpportunityMultistepTrade:
		if barterCount < 2 {
			return domain.TradeOpportunity{}, fmt.Errorf("%s: многоступенчатый обмен требует хотя бы два шага обмена", methodCtx)
		}
	default:
		return domain.TradeOpportunity{}, fmt.Errorf("%s: неизвестный тип торговой возможности %q", methodCtx, value.Kind)
	}
	if buyCost != value.InputCost {
		return domain.TradeOpportunity{}, fmt.Errorf(
			"%s: входная стоимость %d не совпадает со стоимостью шагов покупки %d",
			methodCtx,
			value.InputCost,
			buyCost,
		)
	}
	if listRevenue != value.ExpectedRevenue {
		return domain.TradeOpportunity{}, fmt.Errorf(
			"%s: ожидаемая выручка %d не совпадает с шагом выставления %d",
			methodCtx,
			value.ExpectedRevenue,
			listRevenue,
		)
	}
	result, err := economy.Complete(cloneOpportunity(value))
	if err != nil {
		return domain.TradeOpportunity{}, fmt.Errorf("%s: не удалось завершить экономический расчёт: %w", methodCtx, err)
	}
	return result, nil
}

func validateWeights(value economy.ScoreWeights) error {
	const methodCtx = "trading.validateWeights"

	values := []float64{
		value.Profit,
		value.ROI,
		value.Liquidity,
		value.Confidence,
		value.Risk,
		value.Slots,
	}
	for _, weight := range values {
		if math.IsNaN(weight) || math.IsInf(weight, 0) || weight < 0 {
			return fmt.Errorf("%s: веса оценки должны быть конечными и неотрицательными", methodCtx)
		}
	}
	return nil
}

func finitePositive(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value > 0
}

func cloneState(value *sagaState) *sagaState {
	result := &sagaState{
		snapshot:  cloneSaga(value.snapshot),
		limits:    value.limits,
		holdings:  make(map[string]int64, len(value.holdings)),
		processed: make(map[string]processedEvent, len(value.processed)+1),
	}
	for itemID, quantity := range value.holdings {
		result.holdings[itemID] = quantity
	}
	for id, record := range value.processed {
		result.processed[id] = record
	}
	return result
}

func cloneSaga(value Saga) Saga {
	value.Opportunity = cloneOpportunity(value.Opportunity)
	value.Holdings = append([]Holding(nil), value.Holdings...)
	value.Compensation = cloneCompensation(value.Compensation)
	if value.Reconciliation != nil {
		report := *value.Reconciliation
		report.Mismatches = append(
			[]domain.ReconciliationMismatch(nil),
			value.Reconciliation.Mismatches...,
		)
		value.Reconciliation = &report
	}
	return value
}

func cloneOpportunity(value domain.TradeOpportunity) domain.TradeOpportunity {
	value.Steps = append([]domain.TradeStep(nil), value.Steps...)
	return value
}

func cloneCompensation(value *Compensation) *Compensation {
	if value == nil {
		return nil
	}
	result := *value
	result.Holdings = append([]Holding(nil), value.Holdings...)
	return &result
}

func cloneEvent(value Event) Event {
	value.Outcome.InventoryDeltas = append(
		[]domain.InventoryDelta(nil),
		value.Outcome.InventoryDeltas...,
	)
	if value.Settlement != nil {
		settlement := *value.Settlement
		value.Settlement = &settlement
	}
	return value
}

func cloneCheckpoint(value Checkpoint) Checkpoint {
	value.Saga = cloneSaga(value.Saga)
	value.Inventory.Items = append([]domain.InventoryItem(nil), value.Inventory.Items...)
	value.Processed = append([]ProcessedEventCheckpoint(nil), value.Processed...)
	for index := range value.Processed {
		value.Processed[index].Event = cloneEvent(value.Processed[index].Event)
		value.Processed[index].Snapshot = cloneSaga(value.Processed[index].Snapshot)
	}
	return value
}

func validateCheckpoint(value Checkpoint) error {
	const methodCtx = "trading.validateCheckpoint"

	if value.SchemaVersion != checkpointSchemaVersion {
		return fmt.Errorf("%s: версия схемы %d, требуется %d", methodCtx, value.SchemaVersion, checkpointSchemaVersion)
	}
	saga := value.Saga
	if saga.ID == "" || saga.Version == 0 || saga.StartedAt.IsZero() || saga.UpdatedAt.IsZero() {
		return fmt.Errorf("%s: обязательны идентификатор, версия и временные метки саги", methodCtx)
	}
	if saga.UpdatedAt.Before(saga.StartedAt) {
		return fmt.Errorf("%s: updated_at саги находится раньше started_at", methodCtx)
	}
	if saga.CurrentStep < 0 || saga.CurrentStep > len(saga.Opportunity.Steps) {
		return fmt.Errorf("%s: текущий шаг выходит за пределы торговой возможности", methodCtx)
	}
	if saga.Status == SagaRunning && saga.CurrentStep >= len(saga.Opportunity.Steps) {
		return fmt.Errorf("%s: активная сага не содержит текущего шага", methodCtx)
	}
	if saga.Status == SagaAwaitingSale && strings.TrimSpace(saga.MarketOrderID) == "" {
		return fmt.Errorf(
			"%s: сага мониторинга продажи не содержит идентификатор рыночного ордера",
			methodCtx,
		)
	}
	if isTerminal(saga.Status) && saga.ReservedBudget != 0 {
		return fmt.Errorf("%s: завершённая сага сохраняет зарезервированный бюджет", methodCtx)
	}
	if !isTerminal(saga.Status) && saga.ReservedBudget != saga.Opportunity.InputCost {
		return fmt.Errorf("%s: активная сага содержит несогласованный резерв бюджета", methodCtx)
	}
	if saga.StepProgress < 0 {
		return fmt.Errorf("%s: прогресс шага отрицателен", methodCtx)
	}
	if saga.CurrentStep < len(saga.Opportunity.Steps) &&
		saga.StepProgress >= saga.Opportunity.Steps[saga.CurrentStep].Quantity {
		return fmt.Errorf("%s: прогресс выходит за пределы текущего шага", methodCtx)
	}
	if saga.Actual.InputCost < 0 || saga.Actual.Revenue < 0 || saga.Actual.Fees < 0 {
		return fmt.Errorf("%s: фактические финансовые значения отрицательны", methodCtx)
	}
	profit, err := money.Subtract(saga.Actual.Revenue, saga.Actual.Fees, saga.Actual.InputCost)
	if err != nil || profit != saga.Actual.Profit {
		return fmt.Errorf("%s: фактическая прибыль не согласована с выручкой и затратами", methodCtx)
	}
	holdingIDs := make(map[string]struct{}, len(saga.Holdings))
	for _, holding := range saga.Holdings {
		if holding.ItemID == "" || holding.Quantity <= 0 {
			return fmt.Errorf("%s: сага содержит некорректное удержание предмета", methodCtx)
		}
		if _, duplicate := holdingIDs[holding.ItemID]; duplicate {
			return fmt.Errorf("%s: сага содержит повторное удержание предмета %q", methodCtx, holding.ItemID)
		}
		holdingIDs[holding.ItemID] = struct{}{}
	}
	if value.Inventory.CapacitySlots < 0 || value.Inventory.UsedSlots < 0 ||
		value.Inventory.UsedSlots > value.Inventory.CapacitySlots {
		return fmt.Errorf("%s: значения слотов инвентаря некорректны", methodCtx)
	}
	seenEvents := make(map[string]struct{}, len(value.Processed))
	seenVersions := make(map[uint64]struct{}, len(value.Processed))
	seenMonetaryActions := make(map[string]string, len(value.Processed))
	var latest Saga
	for _, record := range value.Processed {
		if record.Event.ID == "" || record.Event.SagaID != saga.ID ||
			record.Snapshot.ID != saga.ID {
			return fmt.Errorf("%s: идентификаторы обработанного события некорректны", methodCtx)
		}
		if _, duplicate := seenEvents[record.Event.ID]; duplicate {
			return fmt.Errorf("%s: обработанное событие %q продублировано", methodCtx, record.Event.ID)
		}
		seenEvents[record.Event.ID] = struct{}{}
		monetaryActionID := record.Event.Outcome.MonetaryActionID
		if monetaryActionID != strings.TrimSpace(monetaryActionID) {
			return fmt.Errorf(
				"%s: событие %q содержит ненормализованный идентификатор денежного UI-действия",
				methodCtx,
				record.Event.ID,
			)
		}
		switch record.Event.Kind {
		case EventStepSucceeded:
			if monetaryActionID == "" {
				return fmt.Errorf(
					"%s: успешное событие шага %q не связано с денежным UI-действием",
					methodCtx,
					record.Event.ID,
				)
			}
		case EventStepFailed:
			if record.Event.Outcome.CompletedQuantity > 0 && monetaryActionID == "" {
				return fmt.Errorf(
					"%s: частичный результат шага %q не связан с денежным UI-действием",
					methodCtx,
					record.Event.ID,
				)
			}
		case EventRecoveryResolved:
			if record.Event.Resolution == RecoveryCompensated {
				if monetaryActionID == "" {
					return fmt.Errorf(
						"%s: компенсация %q не связана с денежным UI-действием",
						methodCtx,
						record.Event.ID,
					)
				}
			} else if monetaryActionID != "" {
				return fmt.Errorf(
					"%s: неденежное восстановление %q содержит постороннее UI-действие",
					methodCtx,
					record.Event.ID,
				)
			}
		case EventSaleSettled:
			if monetaryActionID != "" {
				return fmt.Errorf(
					"%s: событие сверки продажи %q содержит постороннее денежное UI-действие",
					methodCtx,
					record.Event.ID,
				)
			}
		default:
			return fmt.Errorf(
				"%s: обработанное событие %q имеет неизвестный тип %q",
				methodCtx,
				record.Event.ID,
				record.Event.Kind,
			)
		}
		if monetaryActionID != "" {
			if previousEventID, duplicate := seenMonetaryActions[monetaryActionID]; duplicate {
				return fmt.Errorf(
					"%s: денежное UI-действие %q связано с событиями %q и %q",
					methodCtx,
					monetaryActionID,
					previousEventID,
					record.Event.ID,
				)
			}
			seenMonetaryActions[monetaryActionID] = record.Event.ID
		}
		if record.Snapshot.Version < 2 || record.Snapshot.Version > saga.Version {
			return fmt.Errorf("%s: версия снимка обработанного события некорректна", methodCtx)
		}
		if _, duplicate := seenVersions[record.Snapshot.Version]; duplicate {
			return fmt.Errorf("%s: версия обработанного снимка %d продублирована", methodCtx, record.Snapshot.Version)
		}
		seenVersions[record.Snapshot.Version] = struct{}{}
		if latest.Version < record.Snapshot.Version {
			latest = record.Snapshot
		}
	}
	if saga.Version != uint64(len(value.Processed)+1) {
		return fmt.Errorf("%s: версия саги не совпадает с числом обработанных событий", methodCtx)
	}
	if len(value.Processed) > 0 && !reflect.DeepEqual(latest, saga) {
		return fmt.Errorf("%s: последний обработанный снимок не совпадает с сагой", methodCtx)
	}
	return nil
}

// ValidateCheckpointStrict proves that every persisted snapshot can be
// deterministically derived from the initial saga by replaying durable events.
// Inventory is still validated structurally because its account-wide baseline
// is not part of the saga event stream.
func ValidateCheckpointStrict(value Checkpoint, maxStepAttempts int) error {
	const methodCtx = "trading.ValidateCheckpointStrict"

	value = cloneCheckpoint(value)
	if err := validateCheckpoint(value); err != nil {
		return fmt.Errorf("%s: структурная проверка отклонена: %w", methodCtx, err)
	}
	if maxStepAttempts <= 0 {
		return fmt.Errorf("%s: максимальное число попыток шага должно быть положительным", methodCtx)
	}
	canonical, err := canonicalOpportunity(value.Saga.Opportunity)
	if err != nil {
		return fmt.Errorf("%s: торговая возможность некорректна: %w", methodCtx, err)
	}
	if !reflect.DeepEqual(canonical, value.Saga.Opportunity) {
		return fmt.Errorf("%s: торговая возможность не приведена к канонической форме", methodCtx)
	}

	processed := append([]ProcessedEventCheckpoint(nil), value.Processed...)
	sort.Slice(processed, func(i, j int) bool {
		if processed[i].Snapshot.Version != processed[j].Snapshot.Version {
			return processed[i].Snapshot.Version < processed[j].Snapshot.Version
		}
		return processed[i].Event.ID < processed[j].Event.ID
	})
	initial := Saga{
		ID:             value.Saga.ID,
		Opportunity:    cloneOpportunity(canonical),
		Status:         SagaRunning,
		ReservedBudget: canonical.InputCost,
		Version:        1,
		StartedAt:      value.Saga.StartedAt,
		UpdatedAt:      value.Saga.StartedAt,
	}
	state := &sagaState{
		snapshot:  initial,
		limits:    value.Limits,
		holdings:  make(map[string]int64),
		processed: make(map[string]processedEvent, len(processed)),
	}
	for index, record := range processed {
		expectedVersion := uint64(index + 2)
		if record.Snapshot.Version != expectedVersion {
			return fmt.Errorf(
				"%s: версия снимка события %q равна %d, ожидалась %d",
				methodCtx,
				record.Event.ID,
				record.Snapshot.Version,
				expectedVersion,
			)
		}
		event, err := normalizeCheckpointEvent(record.Event)
		if err != nil {
			return fmt.Errorf("%s: событие %q не нормализовано: %w", methodCtx, record.Event.ID, err)
		}
		if event.OccurredAt.Before(state.snapshot.UpdatedAt) {
			return fmt.Errorf(
				"%s: событие %q старше предыдущего снимка",
				methodCtx,
				event.ID,
			)
		}
		if state.snapshot.Version == math.MaxUint64 {
			return fmt.Errorf("%s: переполнение версии саги", methodCtx)
		}

		next := cloneState(state)
		switch event.Kind {
		case EventStepSucceeded:
			_, err = applyStepEvent(next, event, true)
		case EventStepFailed:
			_, err = applyStepEvent(next, event, false)
		case EventRecoveryResolved:
			_, err = applyRecoveryEvent(next, event, maxStepAttempts)
		case EventSaleSettled:
			err = applySettlementEvent(next, event)
		default:
			err = fmt.Errorf("%s: неизвестный тип события %q", methodCtx, event.Kind)
		}
		if err != nil {
			return fmt.Errorf("%s: воспроизведение события %q отклонено: %w", methodCtx, event.ID, err)
		}
		next.snapshot.Version++
		next.snapshot.UpdatedAt = event.OccurredAt
		next.snapshot.Holdings = sortedHoldings(next.holdings)
		if isTerminal(next.snapshot.Status) {
			next.snapshot.ReservedBudget = 0
		}
		if !reflect.DeepEqual(next.snapshot, record.Snapshot) {
			return fmt.Errorf(
				"%s: снимок события %q не совпадает с результатом воспроизведения",
				methodCtx,
				event.ID,
			)
		}
		next.processed[event.ID] = processedEvent{
			event:    cloneEvent(event),
			snapshot: cloneSaga(next.snapshot),
		}
		state = next
	}
	if !reflect.DeepEqual(state.snapshot, value.Saga) {
		return fmt.Errorf("%s: итог воспроизведения не совпадает с сохранённой сагой", methodCtx)
	}
	return nil
}

func normalizeCheckpointEvent(value Event) (Event, error) {
	const methodCtx = "trading.normalizeCheckpointEvent"

	if value.OccurredAt.IsZero() {
		return Event{}, fmt.Errorf("%s: occurred_at обязателен", methodCtx)
	}
	normalized := cloneEvent(value)
	normalized.Outcome.MonetaryActionID = strings.TrimSpace(normalized.Outcome.MonetaryActionID)
	normalized.Outcome.MarketOrderID = strings.TrimSpace(normalized.Outcome.MarketOrderID)
	normalized.OccurredAt = normalized.OccurredAt.UTC()
	if normalized.Settlement != nil {
		normalized.Settlement.MarketOrderID = strings.TrimSpace(normalized.Settlement.MarketOrderID)
		if normalized.Settlement.CompletedAt.IsZero() {
			normalized.Settlement.CompletedAt = normalized.OccurredAt
		} else {
			normalized.Settlement.CompletedAt = normalized.Settlement.CompletedAt.UTC()
		}
	}
	if !reflect.DeepEqual(value, normalized) {
		return Event{}, fmt.Errorf("%s: событие отличается от нормализованной формы Service.Apply", methodCtx)
	}
	return normalized, nil
}

// MonetaryActionIDs validates a durable checkpoint and returns the exact set
// of monetary UI action IDs explicitly covered by its processed events.
func MonetaryActionIDs(value Checkpoint) ([]string, error) {
	const methodCtx = "trading.MonetaryActionIDs"

	result, err := MonetaryActionIDsWithMaxStepAttempts(value, 3)
	if err != nil {
		return nil, fmt.Errorf("%s: контрольная точка не прошла проверку: %w", methodCtx, err)
	}
	return result, nil
}

// MonetaryActionIDsWithMaxStepAttempts performs strict semantic checkpoint
// validation with an explicit recovery limit before trusting action IDs.
func MonetaryActionIDsWithMaxStepAttempts(
	value Checkpoint,
	maxStepAttempts int,
) ([]string, error) {
	const methodCtx = "trading.MonetaryActionIDsWithMaxStepAttempts"

	value = cloneCheckpoint(value)
	if err := ValidateCheckpointStrict(value, maxStepAttempts); err != nil {
		return nil, fmt.Errorf("%s: контрольная точка не прошла проверку: %w", methodCtx, err)
	}
	result := make([]string, 0, len(value.Processed))
	for _, record := range value.Processed {
		if id := record.Event.Outcome.MonetaryActionID; id != "" {
			result = append(result, id)
		}
	}
	sort.Strings(result)
	return result, nil
}

func sortedHoldings(values map[string]int64) []Holding {
	ids := make([]string, 0, len(values))
	for itemID, quantity := range values {
		if quantity > 0 {
			ids = append(ids, itemID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	sort.Strings(ids)
	result := make([]Holding, 0, len(ids))
	for _, itemID := range ids {
		result = append(result, Holding{ItemID: itemID, Quantity: values[itemID]})
	}
	return result
}

func isTerminal(status SagaStatus) bool {
	switch status {
	case SagaCompleted, SagaCompletedMismatch, SagaCompensated, SagaHeld, SagaFailed:
		return true
	default:
		return false
	}
}

func emptyOutcome(value StepOutcome) bool {
	return value.CompletedQuantity == 0 &&
		value.PurchaseCost == 0 &&
		value.Revenue == 0 &&
		value.Fees == 0 &&
		value.MonetaryActionID == "" &&
		value.MarketOrderID == "" &&
		len(value.InventoryDeltas) == 0
}

func applySettlementEvent(state *sagaState, event Event) error {
	const methodCtx = "trading.applySettlementEvent"

	if state.snapshot.Status != SagaAwaitingSale {
		return fmt.Errorf("%s: %w: продажу нельзя закрыть из состояния %s", methodCtx, ErrInvalidTransition, state.snapshot.Status)
	}
	if event.Settlement == nil {
		return fmt.Errorf("%s: %w: требуется сверка продажи", methodCtx, ErrInvalidTransition)
	}
	if !emptyOutcome(event.Outcome) || event.Reason != "" || event.Resolution != "" {
		return fmt.Errorf("%s: %w: событие продажи содержит посторонние поля", methodCtx, ErrInvalidTransition)
	}
	if len(sortedHoldings(state.holdings)) != 0 {
		return fmt.Errorf("%s: %w: выставленный результат всё ещё учтён в инвентаре саги", methodCtx, ErrInvalidTransition)
	}
	actual := *event.Settlement
	if actual.ExecutionID != state.snapshot.ID {
		return fmt.Errorf("%s: %w: идентификатор исполнения сверки не совпадает с сагой", methodCtx, ErrInvalidTransition)
	}
	if strings.TrimSpace(actual.MarketOrderID) == "" ||
		actual.MarketOrderID != state.snapshot.MarketOrderID {
		return fmt.Errorf(
			"%s: %w: идентификатор рыночного ордера сверки %q не совпадает с %q",
			methodCtx,
			ErrInvalidTransition,
			actual.MarketOrderID,
			state.snapshot.MarketOrderID,
		)
	}
	if actual.InputCost != state.snapshot.Actual.InputCost {
		return fmt.Errorf("%s: %w: входная стоимость сверки конфликтует с подтверждёнными шагами", methodCtx, ErrInvalidTransition)
	}
	if actual.Revenue < state.snapshot.Actual.Revenue ||
		actual.Fees < state.snapshot.Actual.Fees {
		return fmt.Errorf("%s: %w: итоги сверки меньше значений подтверждённых шагов", methodCtx, ErrInvalidTransition)
	}
	report, err := reconciliation.Reconcile(state.snapshot.Opportunity, actual)
	if err != nil {
		return fmt.Errorf("%s: не удалось сверить результат продажи: %w", methodCtx, err)
	}
	state.snapshot.Actual = report.Actual
	state.snapshot.Reconciliation = &report
	state.snapshot.CurrentStep = len(state.snapshot.Opportunity.Steps)
	state.snapshot.StepProgress = 0
	state.snapshot.Compensation = nil
	state.snapshot.Failure = ""
	if report.Matched {
		state.snapshot.Status = SagaCompleted
	} else {
		state.snapshot.Status = SagaCompletedMismatch
	}
	return nil
}

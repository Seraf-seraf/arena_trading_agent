package automation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	appconfig "github.com/arena-trading-agent/arena-trading-agent/internal/config"
	"github.com/arena-trading-agent/arena-trading-agent/internal/controller"
	"github.com/arena-trading-agent/arena-trading-agent/internal/domain"
	"github.com/arena-trading-agent/arena-trading-agent/internal/opportunity"
	"github.com/arena-trading-agent/arena-trading-agent/internal/protocol"
	"github.com/arena-trading-agent/arena-trading-agent/internal/repository"
	"github.com/arena-trading-agent/arena-trading-agent/internal/trading"
)

const (
	recordKindEngineSnapshot = "automation_engine"
	recordKindEvaluation     = "opportunity_evaluation"
	recordKindCheckpoint     = "trading_checkpoint"

	engineSnapshotKey = "automation/latest"
	evaluationKey     = "evaluation/latest"

	defaultEngineTick = 500 * time.Millisecond
	defaultErrorLimit = 3
	auditPageSize     = 500
	maxAuditedActions = 10_000
)

var ErrReconciliationRequired = errors.New("требуется обязательная сверка перед продолжением")

type runtimeControl interface {
	Mode() domain.AgentMode
	State() controller.RuntimeState
	SetModeContext(context.Context, domain.AgentMode) error
	SendEmergencyStop(context.Context, string, string) error
}

type accountScanService interface {
	Scan(context.Context, string, string) (AccountSnapshot, error)
}

type marketScanService interface {
	Scan(context.Context, string, string) ScanReport
}

type contactScanService interface {
	Scan(context.Context, string, string) ScanReport
}

type orderSnapshotConsumer interface {
	takeOrderSnapshot(string) (OrderSnapshot, bool)
}

// EngineConfig contains process-level scheduling knobs. Business policy is
// always read from the validated runtime configuration.
type EngineConfig struct {
	Tick       time.Duration
	ErrorLimit int
	Now        func() time.Time
}

// EngineSnapshot is the compact restart-safe and dashboard-facing state.
type EngineSnapshot struct {
	SessionID       string              `json:"session_id"`
	Mode            domain.AgentMode    `json:"mode"`
	Running         bool                `json:"running"`
	ErrorStreak     int                 `json:"error_streak"`
	LastError       string              `json:"last_error,omitempty"`
	RecoveryBlocked bool                `json:"recovery_blocked"`
	RecoveryReason  string              `json:"recovery_reason,omitempty"`
	Account         *AccountSnapshot    `json:"account,omitempty"`
	MarketScan      *ScanReport         `json:"market_scan,omitempty"`
	ContactScan     *ScanReport         `json:"contact_scan,omitempty"`
	Evaluation      *trading.Evaluation `json:"evaluation,omitempty"`
	ActiveSaga      *trading.Saga       `json:"active_saga,omitempty"`
	UpdatedAt       time.Time           `json:"updated_at"`
}

// Engine is the only module that schedules scanners, strategy and the trade
// runner. Its single goroutine preserves the one-transaction v1 invariant.
type Engine struct {
	control  runtimeControl
	account  accountScanService
	market   marketScanService
	contacts contactScanService
	service  *trading.Service
	runner   DirectiveRunner
	store    repository.Store
	runtime  appconfig.Runtime
	logger   *slog.Logger
	config   EngineConfig

	mu       sync.RWMutex
	snapshot EngineSnapshot
	activeID string

	nextMarket   time.Time
	nextContacts time.Time
	nextOrders   time.Time
	sequence     atomic.Uint64
	recoverOnce  sync.Once
	recoverErr   error
}

func NewEngine(
	control runtimeControl,
	account accountScanService,
	market marketScanService,
	contacts contactScanService,
	service *trading.Service,
	runner DirectiveRunner,
	store repository.Store,
	runtime appconfig.Runtime,
	logger *slog.Logger,
	config EngineConfig,
) (*Engine, error) {
	const methodCtx = "automation.NewEngine"

	if control == nil || account == nil || market == nil || contacts == nil ||
		service == nil || runner == nil || store == nil {
		return nil, fmt.Errorf("%s: все runtime-зависимости обязательны", methodCtx)
	}
	if err := runtime.Validate(); err != nil {
		return nil, fmt.Errorf("%s: некорректная конфигурация движка автоматизации: %w", methodCtx, err)
	}
	if config.Tick <= 0 {
		config.Tick = defaultEngineTick
	}
	if config.Tick < 100*time.Millisecond || config.Tick > time.Minute {
		return nil, fmt.Errorf("%s: период движка автоматизации должен быть в диапазоне 100ms..1m", methodCtx)
	}
	if config.ErrorLimit == 0 {
		config.ErrorLimit = defaultErrorLimit
	}
	if config.ErrorLimit < 1 || config.ErrorLimit > 100 {
		return nil, fmt.Errorf("%s: лимит ошибок движка автоматизации должен быть в диапазоне 1..100", methodCtx)
	}
	if config.Now == nil {
		config.Now = func() time.Time { return time.Now().UTC() }
	}
	if logger == nil {
		logger = slog.Default()
	}
	now := config.Now().UTC()
	engine := &Engine{
		control: control, account: account, market: market, contacts: contacts,
		service: service, runner: runner, store: store, runtime: runtime,
		logger: logger, config: config,
		snapshot: EngineSnapshot{
			SessionID: fmt.Sprintf("session-%d", now.UnixNano()),
			Mode:      domain.ModePaused,
			UpdatedAt: now,
		},
	}
	return engine, nil
}

// Recover restores the newest non-terminal checkpoint and audits every
// monetary action against exact IDs recorded in durable checkpoints.
func (e *Engine) Recover(ctx context.Context) error {
	const methodCtx = "automation.Engine.Recover"

	if ctx == nil {
		return fmt.Errorf("%s: контекст восстановления автоматизации не задан", methodCtx)
	}
	e.recoverOnce.Do(func() {
		e.recoverErr = e.recover(ctx)
	})
	if e.recoverErr != nil {
		return fmt.Errorf("%s: восстановление завершилось ошибкой: %w", methodCtx, e.recoverErr)
	}
	return nil
}

// Run owns all scheduling. Call Recover before starting it when startup must
// fail synchronously on repository errors.
func (e *Engine) Run(ctx context.Context) {
	const methodCtx = "automation.Engine.Run"

	if err := e.Recover(ctx); err != nil {
		e.blockRecovery(methodCtx + ": не удалось восстановить состояние среды выполнения: " + err.Error())
	}
	ticker := time.NewTicker(e.config.Tick)
	defer ticker.Stop()
	e.setRunning(true)
	defer e.setRunning(false)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := e.tick(ctx); err != nil {
				e.recordFailure(ctx, err)
			} else {
				e.recordSuccess()
			}
		}
	}
}

// Snapshot returns a deep copy safe for JSON/API callers.
func (e *Engine) Snapshot() EngineSnapshot {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return cloneEngineSnapshot(e.snapshot)
}

// AuthorizeMode adds domain readiness to Session Coordinator's hardware and
// service preflight. SIMULATE/OBSERVE/PAUSED never open the input gate.
func (e *Engine) AuthorizeMode(ctx context.Context, mode domain.AgentMode) error {
	const methodCtx = "automation.Engine.AuthorizeMode"

	if mode != domain.ModeScan && mode != domain.ModeTrade {
		return nil
	}
	if err := e.validateRoutes(mode); err != nil {
		return fmt.Errorf("%s: маршруты режима %s не готовы: %w", methodCtx, mode, err)
	}
	if mode == domain.ModeScan {
		return nil
	}
	snapshot := e.Snapshot()
	if snapshot.RecoveryBlocked {
		return fmt.Errorf("%s: восстановление заблокировано: %s", methodCtx, snapshot.RecoveryReason)
	}
	if err := e.auditCheckpointBoundary(ctx); err != nil {
		e.blockRecovery(err.Error())
		return fmt.Errorf("%s: граница контрольной точки не прошла аудит: %w", methodCtx, err)
	}
	account, err := loadJSON[AccountSnapshot](ctx, e.store, "account/latest")
	if err != nil {
		return fmt.Errorf("%s: нет синхронизированного снимка аккаунта: %w", methodCtx, err)
	}
	now := e.config.Now().UTC()
	if account.ObservedAt.IsZero() ||
		now.Sub(account.ObservedAt) > e.runtime.Scanners.Market.Staleness.Value() {
		return fmt.Errorf("%s: снимок аккаунта устарел", methodCtx)
	}
	if account.Confidence < e.runtime.Risk.MinConfidence {
		return fmt.Errorf("%s: уверенность снимка аккаунта %.3f ниже %.3f", methodCtx, account.Confidence, e.runtime.Risk.MinConfidence)
	}
	if account.Balance <= 0 {
		return fmt.Errorf("%s: баланс не подтверждён", methodCtx)
	}
	if account.FreeInventorySlots <= 0 {
		return fmt.Errorf("%s: нет свободных слотов инвентаря", methodCtx)
	}
	if account.FreeMarketSlots <= 0 {
		return fmt.Errorf("%s: нет свободных рыночных слотов", methodCtx)
	}
	for _, itemID := range e.runtime.TrackedItemIDs() {
		quote, err := e.store.LatestTradeQuote(ctx, itemID)
		if err != nil {
			return fmt.Errorf("%s: нет полной котировки %q: %w", methodCtx, itemID, err)
		}
		if quote.ObservedAt.IsZero() || now.Sub(quote.ObservedAt) > e.runtime.Risk.MaxQuoteAge.Value() {
			return fmt.Errorf("%s: котировка %q устарела", methodCtx, itemID)
		}
		if quote.Confidence < e.runtime.Risk.MinConfidence {
			return fmt.Errorf("%s: котировка %q имеет низкую уверенность", methodCtx, itemID)
		}
	}
	for _, recipe := range e.runtime.Recipes {
		if !recipe.Enabled {
			continue
		}
		record, err := e.store.RuntimeRecord(ctx, "recipe/"+recipe.ID)
		if err != nil {
			return fmt.Errorf("%s: рецепт %q не синхронизирован: %w", methodCtx, recipe.ID, err)
		}
		if now.Sub(record.UpdatedAt) > e.runtime.Scanners.Contacts.Staleness.Value() {
			return fmt.Errorf("%s: рецепт %q устарел", methodCtx, recipe.ID)
		}
	}
	return nil
}

func (e *Engine) tick(ctx context.Context) error {
	const methodCtx = "automation.Engine.tick"

	mode := e.control.Mode()
	e.mu.Lock()
	previousMode := e.snapshot.Mode
	if mode != previousMode {
		e.snapshot.Mode = mode
		e.snapshot.UpdatedAt = e.config.Now().UTC()
		e.nextMarket = time.Time{}
		e.nextContacts = time.Time{}
		e.nextOrders = time.Time{}
	}
	e.mu.Unlock()
	if mode != previousMode {
		_ = e.persistSnapshot(ctx)
	}
	switch mode {
	case domain.ModePaused, domain.ModeObserve:
		return nil
	case domain.ModeSimulate:
		return e.runSimulation(ctx)
	case domain.ModeScan:
		return e.runScanners(ctx)
	case domain.ModeTrade:
		if e.Snapshot().RecoveryBlocked {
			return fmt.Errorf("%s: режим TRADE заблокирован: %s", methodCtx, e.Snapshot().RecoveryReason)
		}
		if err := e.runTrade(ctx); err != nil {
			return fmt.Errorf("%s: торговый цикл завершился ошибкой: %w", methodCtx, err)
		}
		return nil
	default:
		return fmt.Errorf("%s: неизвестный режим %q", methodCtx, mode)
	}
}

func (e *Engine) runScanners(ctx context.Context) error {
	const methodCtx = "automation.Engine.runScanners"

	now := e.config.Now().UTC()
	if e.nextMarket.IsZero() || !now.Before(e.nextMarket) {
		account, err := e.account.Scan(ctx, "", e.sessionID())
		if err != nil {
			return fmt.Errorf("%s: сканер аккаунта завершился ошибкой: %w", methodCtx, err)
		}
		if err := e.service.SynchronizeInventory(account.Inventory); err != nil {
			return fmt.Errorf("%s: не удалось синхронизировать трекер инвентаря: %w", methodCtx, err)
		}
		report := e.market.Scan(ctx, "", e.sessionID())
		if err := reportError("сканер рынка", report); err != nil {
			return fmt.Errorf("%s: ошибка сканирования рынка: %w", methodCtx, err)
		}
		e.mu.Lock()
		e.snapshot.Account = &account
		e.snapshot.MarketScan = &report
		e.snapshot.UpdatedAt = now
		e.nextMarket = now.Add(e.runtime.Scanners.Market.Interval.Value())
		e.mu.Unlock()
		if err := e.persistSnapshot(ctx); err != nil {
			return fmt.Errorf("%s: не удалось сохранить снимок после сканирования рынка: %w", methodCtx, err)
		}
	}
	if e.nextContacts.IsZero() || !now.Before(e.nextContacts) {
		report := e.contacts.Scan(ctx, "", e.sessionID())
		if err := reportError("сканер контактов", report); err != nil {
			return fmt.Errorf("%s: ошибка сканирования контактов: %w", methodCtx, err)
		}
		e.mu.Lock()
		e.snapshot.ContactScan = &report
		e.snapshot.UpdatedAt = now
		e.nextContacts = now.Add(e.runtime.Scanners.Contacts.Interval.Value())
		e.mu.Unlock()
		if err := e.persistSnapshot(ctx); err != nil {
			return fmt.Errorf("%s: не удалось сохранить снимок после сканирования контактов: %w", methodCtx, err)
		}
		return nil
	}
	return nil
}

func (e *Engine) runSimulation(ctx context.Context) error {
	const methodCtx = "automation.Engine.runSimulation"

	now := e.config.Now().UTC()
	if !e.nextMarket.IsZero() && now.Before(e.nextMarket) {
		return nil
	}
	_, err := e.evaluate(ctx)
	if err != nil {
		return fmt.Errorf("%s: симуляция оценки завершилась ошибкой: %w", methodCtx, err)
	}
	e.nextMarket = now.Add(e.runtime.Scanners.Market.Interval.Value())
	return nil
}

func (e *Engine) runTrade(ctx context.Context) error {
	const methodCtx = "automation.Engine.runTrade"

	if e.activeID != "" {
		if err := e.driveSaga(ctx); err != nil {
			return fmt.Errorf("%s: не удалось продолжить сагу %q: %w", methodCtx, e.activeID, err)
		}
		return nil
	}
	now := e.config.Now().UTC()
	account, err := e.account.Scan(ctx, "", e.sessionID())
	if err != nil {
		return fmt.Errorf("%s: не удалось обновить состояние аккаунта перед сделкой: %w", methodCtx, err)
	}
	if err := e.service.SynchronizeInventory(account.Inventory); err != nil {
		return fmt.Errorf("%s: не удалось синхронизировать трекер инвентаря перед сделкой: %w", methodCtx, err)
	}
	report := e.market.Scan(ctx, "", e.sessionID())
	if err := reportError("обновление торговых котировок", report); err != nil {
		return fmt.Errorf("%s: ошибка обновления котировок: %w", methodCtx, err)
	}
	if e.nextContacts.IsZero() || !now.Before(e.nextContacts) {
		contactReport := e.contacts.Scan(ctx, "", e.sessionID())
		if err := reportError("обновление торговых рецептов", contactReport); err != nil {
			return fmt.Errorf("%s: ошибка обновления рецептов: %w", methodCtx, err)
		}
		e.mu.Lock()
		e.snapshot.ContactScan = &contactReport
		e.nextContacts = now.Add(e.runtime.Scanners.Contacts.Interval.Value())
		e.mu.Unlock()
	}
	e.mu.Lock()
	e.snapshot.Account = &account
	e.snapshot.MarketScan = &report
	e.snapshot.UpdatedAt = now
	e.mu.Unlock()
	evaluation, err := e.evaluate(ctx)
	if err != nil {
		return fmt.Errorf("%s: не удалось оценить возможности: %w", methodCtx, err)
	}
	if len(evaluation.Candidates) == 0 {
		e.nextMarket = now.Add(e.runtime.Scanners.Market.Interval.Value())
		return nil
	}
	limits := e.runtime.Risk.Domain()
	limits.AvailableSlots = min(limits.AvailableSlots, account.FreeInventorySlots)
	limits.MaxBudget = min(limits.MaxBudget, account.Balance)
	executionID := fmt.Sprintf(
		"trade-%d-%06d",
		now.UnixNano(),
		e.sequence.Add(1),
	)
	saga, err := e.service.Begin(
		executionID,
		evaluation.Candidates[0].Opportunity,
		limits,
	)
	if err != nil {
		return fmt.Errorf("%s: не удалось начать торговую сагу: %w", methodCtx, err)
	}
	e.activeID = saga.ID
	e.setActiveSaga(&saga)
	if err := e.persistSaga(ctx, saga); err != nil {
		return e.stopForReconciliation(
			ctx,
			fmt.Errorf("%s: не удалось сохранить начатую сагу: %w", methodCtx, err),
		)
	}
	return nil
}

func (e *Engine) driveSaga(ctx context.Context) error {
	const methodCtx = "automation.Engine.driveSaga"

	saga, err := e.service.Get(e.activeID)
	if err != nil {
		return fmt.Errorf("%s: не удалось получить сагу %q: %w", methodCtx, e.activeID, err)
	}
	directive, err := e.service.Next(e.activeID)
	if err != nil {
		return fmt.Errorf("%s: не удалось определить следующую директиву: %w", methodCtx, err)
	}
	if directive.Kind == trading.DirectiveDone {
		e.activeID = ""
		e.setActiveSaga(&saga)
		if err := e.persistSaga(ctx, saga); err != nil {
			return fmt.Errorf("%s: не удалось сохранить завершённую сагу: %w", methodCtx, err)
		}
		return nil
	}
	now := e.config.Now().UTC()
	if directive.Kind == trading.DirectiveMonitorSale &&
		!e.nextOrders.IsZero() && now.Before(e.nextOrders) {
		return nil
	}
	event, err := e.runner.Execute(ctx, "", e.sessionID(), saga, directive)
	if directive.Kind == trading.DirectiveMonitorSale {
		if snapshotErr := e.persistOrderSnapshot(ctx, saga); snapshotErr != nil {
			cause := fmt.Errorf(
				"%s: не удалось сохранить проверенный снимок ордера: %w",
				methodCtx,
				snapshotErr,
			)
			if err != nil {
				cause = errors.Join(
					fmt.Errorf(
						"%s: мониторинг ордера завершился ошибкой: %w",
						methodCtx,
						err,
					),
					cause,
				)
			}
			return e.stopForReconciliation(
				ctx,
				cause,
			)
		}
	}
	switch {
	case errors.Is(err, ErrSalePending):
		e.nextOrders = now.Add(e.runtime.Scanners.Orders.Interval.Value())
		return nil
	case errors.Is(err, ErrRecoveryPending):
		e.nextOrders = now.Add(e.runtime.Scanners.Orders.Interval.Value())
		return nil
	case err != nil:
		return e.stopForReconciliation(
			ctx,
			fmt.Errorf("%s: исполнитель директивы завершился неоднозначной ошибкой: %w", methodCtx, err),
		)
	}
	updated, err := e.service.Apply(event)
	if err != nil {
		return e.stopForReconciliation(
			ctx,
			fmt.Errorf("%s: не удалось применить подтверждённое торговое событие: %w", methodCtx, err),
		)
	}
	e.setActiveSaga(&updated)
	if err := e.persistSaga(ctx, updated); err != nil {
		return e.stopForReconciliation(
			ctx,
			fmt.Errorf("%s: не удалось сохранить обновлённую сагу: %w", methodCtx, err),
		)
	}
	if updated.Status == trading.SagaCompletedMismatch {
		e.activeID = ""
		return e.stopForReconciliation(
			ctx,
			fmt.Errorf(
				"%s: итоговая сверка саги %q завершилась расхождением",
				methodCtx,
				updated.ID,
			),
		)
	}
	if isTerminalSaga(updated.Status) {
		e.activeID = ""
	}
	return nil
}

func (e *Engine) persistOrderSnapshot(
	ctx context.Context,
	saga trading.Saga,
) error {
	const methodCtx = "automation.Engine.persistOrderSnapshot"

	consumer, ok := e.runner.(orderSnapshotConsumer)
	if !ok {
		return fmt.Errorf("%s: исполнитель не предоставляет снимок контролируемого ордера", methodCtx)
	}
	snapshot, ok := consumer.takeOrderSnapshot(saga.ID)
	if !ok {
		return fmt.Errorf("%s: снимок ордера саги %q отсутствует", methodCtx, saga.ID)
	}
	if err := validateOrderSnapshot(snapshot, &saga); err != nil {
		return fmt.Errorf("%s: снимок ордера саги %q отклонён: %w", methodCtx, saga.ID, err)
	}
	if err := saveJSON(
		ctx,
		e.store,
		orderSnapshotKey(saga.ID),
		orderSnapshotRecordKind,
		snapshot,
		snapshot.ObservedAt,
	); err != nil {
		return fmt.Errorf("%s: запись снимка ордера саги %q завершилась ошибкой: %w", methodCtx, saga.ID, err)
	}
	return nil
}

func (e *Engine) evaluate(ctx context.Context) (trading.Evaluation, error) {
	const methodCtx = "automation.Engine.evaluate"

	input, err := e.planningInput(ctx)
	if err != nil {
		return trading.Evaluation{}, fmt.Errorf("%s: не удалось подготовить входные данные: %w", methodCtx, err)
	}
	evaluation, err := e.service.Evaluate(input, e.runtime.Risk.Domain())
	if err != nil {
		return trading.Evaluation{}, fmt.Errorf("%s: торговая стратегия завершилась ошибкой: %w", methodCtx, err)
	}
	now := e.config.Now().UTC()
	if err := saveJSON(ctx, e.store, evaluationKey, recordKindEvaluation, evaluation, now); err != nil {
		return trading.Evaluation{}, fmt.Errorf("%s: не удалось сохранить оценку: %w", methodCtx, err)
	}
	e.mu.Lock()
	copy := evaluation
	e.snapshot.Evaluation = &copy
	e.snapshot.UpdatedAt = now
	e.mu.Unlock()
	if err := e.persistSnapshot(ctx); err != nil {
		return trading.Evaluation{}, fmt.Errorf("%s: не удалось сохранить снимок движка: %w", methodCtx, err)
	}
	return evaluation, nil
}

func (e *Engine) planningInput(ctx context.Context) (opportunity.Input, error) {
	const methodCtx = "automation.Engine.planningInput"

	input := opportunity.Input{AsOf: e.config.Now().UTC()}
	for _, itemID := range e.runtime.TrackedItemIDs() {
		quote, err := e.store.LatestTradeQuote(ctx, itemID)
		if errors.Is(err, repository.ErrNotFound) {
			continue
		}
		if err != nil {
			return opportunity.Input{}, fmt.Errorf("%s: не удалось получить котировку %q: %w", methodCtx, itemID, err)
		}
		input.Quotes = append(input.Quotes, quote)
	}
	for _, configured := range e.runtime.Recipes {
		if !configured.Enabled {
			continue
		}
		recipe, err := loadJSON[domain.BarterRecipe](ctx, e.store, "recipe/"+configured.ID)
		if errors.Is(err, repository.ErrNotFound) {
			continue
		}
		if err != nil {
			return opportunity.Input{}, fmt.Errorf("%s: не удалось получить рецепт %q: %w", methodCtx, configured.ID, err)
		}
		input.Recipes = append(input.Recipes, recipe)
	}
	return input, nil
}

func (e *Engine) persistSaga(ctx context.Context, saga trading.Saga) error {
	const methodCtx = "automation.Engine.persistSaga"

	checkpoint, err := e.service.Checkpoint(saga.ID)
	if err != nil {
		return fmt.Errorf("%s: не удалось создать контрольную точку саги %q: %w", methodCtx, saga.ID, err)
	}
	payload, err := json.Marshal(checkpoint)
	if err != nil {
		return fmt.Errorf("%s: не удалось сериализовать контрольную точку саги %q: %w", methodCtx, saga.ID, err)
	}
	execution := executionRecord(saga)
	if err := e.store.WithinTransaction(ctx, func(store repository.Store) error {
		const methodCtx = "automation.Engine.persistSaga.transaction"

		if err := store.SaveRuntimeRecord(ctx, domain.RuntimeRecord{
			Key: "saga/" + saga.ID, Kind: recordKindCheckpoint,
			Payload: payload, UpdatedAt: saga.UpdatedAt,
		}); err != nil {
			return fmt.Errorf("%s: не удалось сохранить контрольную точку: %w", methodCtx, err)
		}
		if err := store.SaveExecution(ctx, execution); err != nil {
			return fmt.Errorf("%s: не удалось сохранить исполнение: %w", methodCtx, err)
		}
		return nil
	}); err != nil {
		return fmt.Errorf("%s: транзакция сохранения саги завершилась ошибкой: %w", methodCtx, err)
	}
	return nil
}

func (e *Engine) persistSnapshot(ctx context.Context) error {
	const methodCtx = "automation.Engine.persistSnapshot"

	snapshot := e.Snapshot()
	if err := saveJSON(
		ctx,
		e.store,
		engineSnapshotKey,
		recordKindEngineSnapshot,
		snapshot,
		e.config.Now().UTC(),
	); err != nil {
		return fmt.Errorf("%s: не удалось сохранить runtime-снимок: %w", methodCtx, err)
	}
	return nil
}

func (e *Engine) recover(ctx context.Context) error {
	const methodCtx = "automation.Engine.recover"

	persistedSnapshot, err := loadJSON[EngineSnapshot](
		ctx,
		e.store,
		engineSnapshotKey,
	)
	switch {
	case err == nil && persistedSnapshot.RecoveryBlocked:
		reason := strings.TrimSpace(persistedSnapshot.RecoveryReason)
		if reason == "" {
			reason = "сохранённая блокировка восстановления не содержит причины"
		}
		e.blockRecovery(reason)
	case err != nil && !errors.Is(err, repository.ErrNotFound):
		e.blockRecovery(fmt.Sprintf(
			"сохранённый снимок автоматизации повреждён или недоступен: %v",
			err,
		))
	}

	checkpoints, covered, err := e.checkpointCoverage(ctx)
	coverageValid := err == nil
	if err != nil {
		e.blockRecovery(err.Error())
		checkpoints = nil
		covered = map[string]struct{}{}
	}
	for _, checkpoint := range checkpoints {
		if checkpoint.Saga.Status == trading.SagaCompletedMismatch {
			e.blockRecovery(fmt.Sprintf(
				"контрольная точка саги %q содержит незакрытое расхождение итоговой сверки",
				checkpoint.Saga.ID,
			))
			continue
		}
		if isTerminalSaga(checkpoint.Saga.Status) {
			continue
		}
		if e.activeID != "" {
			e.blockRecovery("найдено несколько незавершённых торговых контрольных точек")
			break
		}
		saga, err := e.service.Restore(checkpoint)
		if err != nil {
			e.blockRecovery(fmt.Sprintf(
				"контрольную точку саги %q нельзя восстановить: %v",
				checkpoint.Saga.ID,
				err,
			))
			break
		}
		e.activeID = saga.ID
		e.setActiveSaga(&saga)
	}
	if coverageValid {
		if err := e.auditActions(ctx, covered); err != nil {
			e.blockRecovery(err.Error())
		}
	}
	if e.Snapshot().RecoveryBlocked {
		if err := e.control.SetModeContext(ctx, domain.ModePaused); err != nil {
			return fmt.Errorf(
				"%s: не удалось восстановить безопасный режим PAUSED: %w",
				methodCtx,
				err,
			)
		}
		e.mu.Lock()
		e.snapshot.Mode = domain.ModePaused
		e.snapshot.UpdatedAt = e.config.Now().UTC()
		e.mu.Unlock()
	}
	if err := e.persistSnapshot(ctx); err != nil {
		return fmt.Errorf("%s: не удалось сохранить восстановленный снимок: %w", methodCtx, err)
	}
	return nil
}

func (e *Engine) auditCheckpointBoundary(ctx context.Context) error {
	const methodCtx = "automation.Engine.auditCheckpointBoundary"

	_, covered, err := e.checkpointCoverage(ctx)
	if err != nil {
		return fmt.Errorf("%s: не удалось проверить торговые контрольные точки: %w", methodCtx, err)
	}
	if err := e.auditActions(ctx, covered); err != nil {
		return fmt.Errorf("%s: аудит действий завершился ошибкой: %w", methodCtx, err)
	}
	return nil
}

func (e *Engine) checkpointCoverage(
	ctx context.Context,
) ([]trading.Checkpoint, map[string]struct{}, error) {
	const methodCtx = "automation.Engine.checkpointCoverage"

	checkpoints := make([]trading.Checkpoint, 0)
	covered := make(map[string]struct{})
	for offset := 0; ; offset += auditPageSize {
		if offset >= maxAuditedActions {
			return nil, nil, fmt.Errorf(
				"%s: число торговых контрольных точек превышает лимит %d",
				methodCtx,
				maxAuditedActions,
			)
		}
		records, err := e.store.ListRuntimeRecords(ctx, domain.RuntimeRecordFilter{
			Kind: recordKindCheckpoint, Limit: auditPageSize, Offset: offset,
		})
		if err != nil {
			return nil, nil, fmt.Errorf(
				"%s: не удалось получить торговые контрольные точки: %w",
				methodCtx,
				err,
			)
		}
		for _, record := range records {
			var checkpoint trading.Checkpoint
			if err := json.Unmarshal(record.Payload, &checkpoint); err != nil {
				return nil, nil, fmt.Errorf(
					"%s: контрольная точка %q повреждена: %w",
					methodCtx,
					record.Key,
					err,
				)
			}
			actionIDs, err := trading.MonetaryActionIDs(checkpoint)
			if err != nil {
				return nil, nil, fmt.Errorf(
					"%s: контрольная точка %q некорректна: %w",
					methodCtx,
					record.Key,
					err,
				)
			}
			checkpoints = append(checkpoints, checkpoint)
			for _, actionID := range actionIDs {
				if _, duplicate := covered[actionID]; duplicate {
					return nil, nil, fmt.Errorf(
						"%s: денежное UI-действие %q присутствует более чем в одной контрольной точке",
						methodCtx,
						actionID,
					)
				}
				covered[actionID] = struct{}{}
			}
		}
		if len(records) < auditPageSize {
			return checkpoints, covered, nil
		}
	}
}

func (e *Engine) auditActions(
	ctx context.Context,
	covered map[string]struct{},
) error {
	const methodCtx = "automation.Engine.auditActions"

	seenCovered := make(map[string]struct{}, len(covered))
	for offset := 0; ; offset += auditPageSize {
		if offset >= maxAuditedActions {
			return fmt.Errorf(
				"%s: журнал действий превышает лимит %d записей",
				methodCtx,
				maxAuditedActions,
			)
		}
		actions, err := e.store.ListActions(ctx, domain.ActionFilter{
			Limit: auditPageSize, Offset: offset,
		})
		if err != nil {
			return fmt.Errorf("%s: не удалось получить журнал действий: %w", methodCtx, err)
		}
		for _, action := range actions {
			class := protocol.ActionClass(action.Class)
			switch class {
			case "", protocol.ActionNavigation:
				continue
			case protocol.ActionPurchase, protocol.ActionBarter,
				protocol.ActionListing, protocol.ActionReprice:
			default:
				return fmt.Errorf(
					"%s: действие %q содержит неизвестный класс %q",
					methodCtx,
					action.ID,
					action.Class,
				)
			}
			result, resultErr := e.store.ActionResult(ctx, action.ID)
			if resultErr != nil {
				if errors.Is(resultErr, repository.ErrNotFound) {
					return fmt.Errorf(
						"%s: денежное действие %q (%s) не имеет сохранённого результата",
						methodCtx,
						action.ID,
						class,
					)
				}
				return fmt.Errorf(
					"%s: не удалось проверить исход денежного действия %q: %w",
					methodCtx,
					action.ID,
					resultErr,
				)
			}
			_, isCovered := covered[action.ID]
			if result.NotSent && result.RetrySafe {
				if isCovered {
					return fmt.Errorf(
						"%s: гарантированно неотправленное действие %q ошибочно включено в торговую контрольную точку",
						methodCtx,
						action.ID,
					)
				}
				continue
			}
			if !result.Success {
				return fmt.Errorf(
					"%s: денежное действие %q (%s) не имеет успешного однозначного результата",
					methodCtx,
					action.ID,
					class,
				)
			}
			if !isCovered {
				return fmt.Errorf(
					"%s: денежное действие %q (%s) не связано с торговым событием в контрольной точке",
					methodCtx,
					action.ID,
					class,
				)
			}
			seenCovered[action.ID] = struct{}{}
		}
		if len(actions) < auditPageSize {
			break
		}
	}
	for actionID := range covered {
		if _, seen := seenCovered[actionID]; !seen {
			return fmt.Errorf(
				"%s: контрольная точка ссылается на отсутствующее или неденежное UI-действие %q",
				methodCtx,
				actionID,
			)
		}
	}
	return nil
}

func (e *Engine) validateRoutes(mode domain.AgentMode) error {
	const methodCtx = "automation.Engine.validateRoutes"

	enabledItems := 0
	for _, item := range e.runtime.Watchlist {
		if item.Enabled {
			enabledItems++
		}
	}
	enabledRecipes := 0
	for _, recipe := range e.runtime.Recipes {
		if recipe.Enabled {
			enabledRecipes++
		}
	}
	if enabledItems == 0 && enabledRecipes == 0 {
		return fmt.Errorf("%s: не включён ни один предмет списка наблюдения или рецепт", methodCtx)
	}
	required := []domain.ScreenState{
		domain.StateMainMenu,
		domain.StateInventory,
		domain.StateMarketHome,
		domain.StateMarketResults,
		domain.StateItemCard,
	}
	if enabledRecipes > 0 {
		required = append(required, domain.StateContacts, domain.StateBarterCard)
	}
	if mode == domain.ModeTrade {
		required = append(
			required,
			domain.StatePurchaseDialog,
			domain.StateSaleDialog,
		)
	}
	if err := stronglyConnected(e.runtime.Navigation.Transitions, required); err != nil {
		return fmt.Errorf("%s: граф безопасной навигации не готов: %w", methodCtx, err)
	}
	if mode == domain.ModeTrade {
		if !reachableByNavigation(
			e.runtime.Navigation.Transitions,
			domain.StateConfirmation,
			domain.StateMainMenu,
		) {
			return fmt.Errorf(
				"%s: после CONFIRMATION нет безопасного NAVIGATION-пути к MAIN_MENU",
				methodCtx,
			)
		}
		requiredCommits := []commitRoute{
			{
				from:  domain.StatePurchaseDialog,
				to:    domain.StateConfirmation,
				class: protocol.ActionPurchase,
			},
			{
				from:  domain.StateSaleDialog,
				to:    domain.StateConfirmation,
				class: protocol.ActionListing,
			},
		}
		if enabledRecipes > 0 {
			requiredCommits = append(requiredCommits, commitRoute{
				from:  domain.StateBarterCard,
				to:    domain.StateConfirmation,
				class: protocol.ActionBarter,
			})
		}
		for _, route := range requiredCommits {
			if !hasExactCommitRoute(e.runtime.Navigation.Transitions, route) {
				return fmt.Errorf(
					"%s: отсутствует точный денежный переход %s → %s класса %s",
					methodCtx,
					route.from,
					route.to,
					route.class,
				)
			}
		}
	}
	return nil
}

func (e *Engine) recordFailure(ctx context.Context, err error) {
	const methodCtx = "automation.Engine.recordFailure"
	logger := e.logger.With("метод", methodCtx)

	e.mu.Lock()
	e.snapshot.ErrorStreak++
	e.snapshot.LastError = err.Error()
	e.snapshot.UpdatedAt = e.config.Now().UTC()
	streak := e.snapshot.ErrorStreak
	e.mu.Unlock()
	logger.Error(
		"цикл автоматизации завершился ошибкой",
		"ошибка", err,
		"число_последовательных_ошибок", streak,
	)
	_ = e.persistSnapshot(context.WithoutCancel(ctx))
	if streak < e.config.ErrorLimit {
		return
	}
	reason := fmt.Sprintf("%d последовательных ошибок автоматизации: %v", streak, err)
	state := e.control.State()
	for _, agent := range state.Agents {
		stopCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		stopErr := e.control.SendEmergencyStop(stopCtx, agent.AgentID, reason)
		cancel()
		if stopErr != nil {
			logger.Error(
				"не удалось отправить аварийную остановку",
				"идентификатор_агента", agent.AgentID,
				"ошибка", stopErr,
			)
		}
	}
	pauseCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	if pauseErr := e.control.SetModeContext(pauseCtx, domain.ModePaused); pauseErr != nil {
		logger.Error("не удалось установить режим PAUSED", "ошибка", pauseErr)
	}
	cancel()
}

func (e *Engine) recordSuccess() {
	e.mu.Lock()
	if e.snapshot.ErrorStreak != 0 || e.snapshot.LastError != "" {
		e.snapshot.ErrorStreak = 0
		e.snapshot.LastError = ""
		e.snapshot.UpdatedAt = e.config.Now().UTC()
	}
	e.mu.Unlock()
}

func (e *Engine) blockRecovery(reason string) {
	const methodCtx = "automation.Engine.blockRecovery"

	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "восстановление заблокировано без указанной причины"
	}
	e.mu.Lock()
	e.snapshot.RecoveryBlocked = true
	e.snapshot.RecoveryReason = methodCtx + ": " + reason
	e.snapshot.UpdatedAt = e.config.Now().UTC()
	e.mu.Unlock()
}

func (e *Engine) stopForReconciliation(ctx context.Context, cause error) error {
	const methodCtx = "automation.Engine.stopForReconciliation"

	if cause == nil {
		cause = errors.New("причина остановки не указана")
	}
	blocked := fmt.Errorf("%s: остановка: %w: %v", methodCtx, ErrReconciliationRequired, cause)
	e.blockRecovery(blocked.Error())

	stopCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	pauseErr := e.control.SetModeContext(stopCtx, domain.ModePaused)
	cancel()
	if pauseErr != nil {
		pauseErr = fmt.Errorf("%s: не удалось установить режим PAUSED: %w", methodCtx, pauseErr)
	} else {
		e.mu.Lock()
		e.snapshot.Mode = domain.ModePaused
		e.snapshot.UpdatedAt = e.config.Now().UTC()
		e.mu.Unlock()
	}

	persistCtx := context.Background()
	if ctx != nil {
		persistCtx = context.WithoutCancel(ctx)
	}
	persistErr := e.persistSnapshot(persistCtx)
	if persistErr != nil {
		persistErr = fmt.Errorf("%s: не удалось сохранить блокировку восстановления: %w", methodCtx, persistErr)
	}
	return errors.Join(blocked, pauseErr, persistErr)
}

func (e *Engine) setRunning(value bool) {
	e.mu.Lock()
	e.snapshot.Running = value
	e.snapshot.UpdatedAt = e.config.Now().UTC()
	e.mu.Unlock()
}

func (e *Engine) setActiveSaga(saga *trading.Saga) {
	e.mu.Lock()
	if saga == nil {
		e.snapshot.ActiveSaga = nil
	} else {
		copy := *saga
		copy.Opportunity.Steps = append([]domain.TradeStep(nil), saga.Opportunity.Steps...)
		copy.Holdings = append([]trading.Holding(nil), saga.Holdings...)
		e.snapshot.ActiveSaga = &copy
	}
	e.snapshot.UpdatedAt = e.config.Now().UTC()
	e.mu.Unlock()
}

func (e *Engine) sessionID() string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.snapshot.SessionID
}

func reportError(name string, report ScanReport) error {
	const methodCtx = "automation.reportError"

	if len(report.Errors) == 0 {
		return nil
	}
	return fmt.Errorf("%s: %s завершился с ошибками: %s", methodCtx, name, strings.Join(report.Errors, "; "))
}

func executionRecord(saga trading.Saga) domain.TradeExecution {
	status := domain.TradeRunning
	switch saga.Status {
	case trading.SagaRecovering, trading.SagaWaitingCooldown, trading.SagaWaitingMarketSlot:
		status = domain.TradeRecovering
	case trading.SagaCompleted:
		status = domain.TradeCompleted
	case trading.SagaCompletedMismatch:
		status = domain.TradeCompletedMismatch
	case trading.SagaCompensated:
		status = domain.TradeCompensated
	case trading.SagaHeld:
		status = domain.TradeHeld
	case trading.SagaFailed:
		status = domain.TradeFailed
	}
	return domain.TradeExecution{
		ID: saga.ID, OpportunityID: saga.Opportunity.ID, Status: status,
		CurrentStep: saga.CurrentStep, Reserved: saga.ReservedBudget,
		StartedAt: saga.StartedAt, UpdatedAt: saga.UpdatedAt, Failure: saga.Failure,
	}
}

func isTerminalSaga(status trading.SagaStatus) bool {
	switch status {
	case trading.SagaCompleted, trading.SagaCompletedMismatch,
		trading.SagaCompensated, trading.SagaHeld, trading.SagaFailed:
		return true
	default:
		return false
	}
}

func stronglyConnected(
	transitions []appconfig.Transition,
	states []domain.ScreenState,
) error {
	const methodCtx = "automation.stronglyConnected"

	edges := make(map[domain.ScreenState][]domain.ScreenState)
	for _, transition := range transitions {
		if transition.Class != "" && transition.Class != protocol.ActionNavigation {
			continue
		}
		edges[transition.From] = append(edges[transition.From], transition.To)
	}
	for _, from := range states {
		reached := map[domain.ScreenState]bool{from: true}
		queue := []domain.ScreenState{from}
		for len(queue) > 0 {
			current := queue[0]
			queue = queue[1:]
			for _, next := range edges[current] {
				if !reached[next] {
					reached[next] = true
					queue = append(queue, next)
				}
			}
		}
		for _, target := range states {
			if !reached[target] {
				return fmt.Errorf("%s: нет пути %s → %s", methodCtx, from, target)
			}
		}
	}
	return nil
}

type commitRoute struct {
	from  domain.ScreenState
	to    domain.ScreenState
	class protocol.ActionClass
}

func hasExactCommitRoute(
	transitions []appconfig.Transition,
	required commitRoute,
) bool {
	for _, transition := range transitions {
		if transition.From == required.from &&
			transition.To == required.to &&
			transition.Class == required.class {
			return true
		}
	}
	return false
}

func reachableByNavigation(
	transitions []appconfig.Transition,
	from domain.ScreenState,
	target domain.ScreenState,
) bool {
	if from == target {
		return true
	}
	edges := make(map[domain.ScreenState][]domain.ScreenState)
	for _, transition := range transitions {
		if transition.Class != "" && transition.Class != protocol.ActionNavigation {
			continue
		}
		edges[transition.From] = append(edges[transition.From], transition.To)
	}
	reached := map[domain.ScreenState]bool{from: true}
	queue := []domain.ScreenState{from}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		for _, next := range edges[current] {
			if reached[next] {
				continue
			}
			if next == target {
				return true
			}
			reached[next] = true
			queue = append(queue, next)
		}
	}
	return false
}

func cloneEngineSnapshot(value EngineSnapshot) EngineSnapshot {
	if value.Account != nil {
		account := *value.Account
		account.Inventory.Items = append(
			[]domain.InventoryItem(nil),
			value.Account.Inventory.Items...,
		)
		value.Account = &account
	}
	if value.MarketScan != nil {
		report := *value.MarketScan
		report.Errors = append([]string(nil), report.Errors...)
		value.MarketScan = &report
	}
	if value.ContactScan != nil {
		report := *value.ContactScan
		report.Errors = append([]string(nil), report.Errors...)
		value.ContactScan = &report
	}
	if value.Evaluation != nil {
		evaluation := *value.Evaluation
		evaluation.Candidates = append([]trading.Candidate(nil), value.Evaluation.Candidates...)
		evaluation.Rejected = append([]trading.Rejection(nil), value.Evaluation.Rejected...)
		value.Evaluation = &evaluation
	}
	if value.ActiveSaga != nil {
		saga := *value.ActiveSaga
		saga.Opportunity.Steps = append(
			[]domain.TradeStep(nil),
			value.ActiveSaga.Opportunity.Steps...,
		)
		saga.Holdings = append([]trading.Holding(nil), value.ActiveSaga.Holdings...)
		value.ActiveSaga = &saga
	}
	return value
}

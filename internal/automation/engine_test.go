package automation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	appconfig "github.com/arena-trading-agent/arena-trading-agent/internal/config"
	"github.com/arena-trading-agent/arena-trading-agent/internal/controller"
	"github.com/arena-trading-agent/arena-trading-agent/internal/domain"
	"github.com/arena-trading-agent/arena-trading-agent/internal/economy"
	"github.com/arena-trading-agent/arena-trading-agent/internal/inventory"
	"github.com/arena-trading-agent/arena-trading-agent/internal/opportunity"
	"github.com/arena-trading-agent/arena-trading-agent/internal/protocol"
	"github.com/arena-trading-agent/arena-trading-agent/internal/repository"
	"github.com/arena-trading-agent/arena-trading-agent/internal/trading"
)

type fakeControl struct {
	mode       domain.AgentMode
	stopCalls  int
	pauseCalls int
}

func (c *fakeControl) Mode() domain.AgentMode { return c.mode }
func (c *fakeControl) State() controller.RuntimeState {
	return controller.RuntimeState{Mode: c.mode, Agents: []controller.AgentState{{AgentID: "agent"}}}
}
func (c *fakeControl) SetModeContext(_ context.Context, mode domain.AgentMode) error {
	c.mode = mode
	c.pauseCalls++
	return nil
}
func (c *fakeControl) SendEmergencyStop(context.Context, string, string) error {
	c.stopCalls++
	return nil
}

type fakeAccountScanner struct {
	value AccountSnapshot
	err   error
}

func (s fakeAccountScanner) Scan(context.Context, string, string) (AccountSnapshot, error) {
	return s.value, s.err
}

type fakeMarketScanner struct{ report ScanReport }

func (s fakeMarketScanner) Scan(context.Context, string, string) ScanReport { return s.report }

type fakeContactScanner struct{ report ScanReport }

func (s fakeContactScanner) Scan(context.Context, string, string) ScanReport { return s.report }

type fakeDirectiveRunner struct{}

func (fakeDirectiveRunner) Execute(
	context.Context,
	string,
	string,
	trading.Saga,
	trading.Directive,
) (trading.Event, error) {
	return trading.Event{}, errors.New("runner не должен вызываться")
}

type countingDirectiveRunner struct {
	calls int
	err   error
}

func (r *countingDirectiveRunner) Execute(
	context.Context,
	string,
	string,
	trading.Saga,
	trading.Directive,
) (trading.Event, error) {
	r.calls++
	return trading.Event{}, r.err
}

type eventDirectiveRunner struct {
	calls    int
	event    trading.Event
	snapshot OrderSnapshot
}

func (r *eventDirectiveRunner) Execute(
	context.Context,
	string,
	string,
	trading.Saga,
	trading.Directive,
) (trading.Event, error) {
	r.calls++
	return r.event, nil
}

func (r *eventDirectiveRunner) takeOrderSnapshot(
	sagaID string,
) (OrderSnapshot, bool) {
	if r.snapshot.SagaID != sagaID {
		return OrderSnapshot{}, false
	}
	value := r.snapshot
	r.snapshot = OrderSnapshot{}
	return value, true
}

type snapshotDirectiveRunner struct {
	calls    int
	event    trading.Event
	err      error
	snapshot OrderSnapshot
	has      bool
}

func (r *snapshotDirectiveRunner) Execute(
	context.Context,
	string,
	string,
	trading.Saga,
	trading.Directive,
) (trading.Event, error) {
	r.calls++
	return r.event, r.err
}

func (r *snapshotDirectiveRunner) takeOrderSnapshot(
	sagaID string,
) (OrderSnapshot, bool) {
	if !r.has || r.snapshot.SagaID != sagaID {
		return OrderSnapshot{}, false
	}
	r.has = false
	return r.snapshot, true
}

func TestEngineSimulateEvaluatesPersistedQuotesWithoutInput(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	store := repository.NewMemory()
	if err := store.SaveTradeQuote(context.Background(), domain.TradeQuote{
		ItemID: "item", PurchasePrice: 10, SalePrice: 30,
		SaleCommission: 2, ListingFee: 1, ObservedAt: now,
		Confidence: 1, LiquidityScore: 1, ResaleKnown: true,
	}); err != nil {
		t.Fatal(err)
	}
	control := &fakeControl{mode: domain.ModeSimulate}
	engine := newTestEngine(t, control, store, now, nil)

	if err := engine.tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	snapshot := engine.Snapshot()
	if snapshot.Evaluation == nil || len(snapshot.Evaluation.Candidates) != 1 ||
		snapshot.Evaluation.Candidates[0].Opportunity.ID != "direct:item" {
		t.Fatalf("unexpected evaluation: %+v", snapshot.Evaluation)
	}
	if control.stopCalls != 0 || control.pauseCalls != 0 {
		t.Fatalf("SIMULATE touched input control: %+v", control)
	}
}

func TestExecutionRecordDoesNotMarkRecoveryOutcomesAsCompleted(t *testing.T) {
	t.Parallel()

	tests := []struct {
		sagaStatus trading.SagaStatus
		want       domain.TradeExecutionStatus
	}{
		{sagaStatus: trading.SagaCompleted, want: domain.TradeCompleted},
		{sagaStatus: trading.SagaCompletedMismatch, want: domain.TradeCompletedMismatch},
		{sagaStatus: trading.SagaCompensated, want: domain.TradeCompensated},
		{sagaStatus: trading.SagaHeld, want: domain.TradeHeld},
		{sagaStatus: trading.SagaFailed, want: domain.TradeFailed},
	}
	for _, test := range tests {
		test := test
		t.Run(string(test.sagaStatus), func(t *testing.T) {
			t.Parallel()
			got := executionRecord(trading.Saga{Status: test.sagaStatus})
			if got.Status != test.want {
				t.Fatalf(
					"статус execution для saga %s = %s, ожидался %s",
					test.sagaStatus,
					got.Status,
					test.want,
				)
			}
		})
	}
}

func TestEngineRecoveryBlocksUncheckpointedMoneyAction(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	store := repository.NewMemory()
	if err := store.SaveAction(context.Background(), domain.ActionRecord{
		ID: "purchase", SessionID: "session", Class: string(protocol.ActionPurchase),
		Kind: "CLICK", RequestedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	engine := newTestEngine(t, &fakeControl{mode: domain.ModePaused}, store, now, nil)

	if err := engine.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	snapshot := engine.Snapshot()
	if !snapshot.RecoveryBlocked || snapshot.RecoveryReason == "" {
		t.Fatalf("audit gap did not block recovery: %+v", snapshot)
	}
}

func TestEngineThirdConsecutiveFailureTriggersEmergencyStop(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	control := &fakeControl{mode: domain.ModeScan}
	engine := newTestEngine(t, control, repository.NewMemory(), now, errors.New("ошибка сканера"))

	for attempt := 0; attempt < 3; attempt++ {
		err := engine.tick(context.Background())
		if err == nil {
			t.Fatal("tick unexpectedly succeeded")
		}
		engine.recordFailure(context.Background(), err)
	}
	if control.stopCalls != 1 || control.mode != domain.ModePaused {
		t.Fatalf("emergency escalation=%+v", control)
	}
}

func TestEngineAmbiguousRunnerFailurePausesAndRequiresReconciliation(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	control := &fakeControl{mode: domain.ModeTrade}
	store := repository.NewMemory()
	engine := newTestEngine(t, control, store, now, nil)
	runner := &countingDirectiveRunner{err: errors.New("результат денежного действия неизвестен")}
	engine.runner = runner

	opportunity := domain.TradeOpportunity{
		ID:              "direct:item",
		Kind:            domain.OpportunityDirectFlip,
		InputCost:       10,
		ExpectedRevenue: 30,
		ExpectedFees:    3,
		Confidence:      1,
		LiquidityScore:  1,
		RequiredSlots:   1,
		QuoteObservedAt: now,
		ResaleKnown:     true,
		ResultItemID:    "item",
		ResultQuantity:  1,
		ExpiresAt:       now.Add(time.Hour),
		Steps: []domain.TradeStep{
			{Kind: domain.TradeStepBuy, ItemID: "item", Quantity: 1, LimitPrice: 10},
			{Kind: domain.TradeStepList, ItemID: "item", Quantity: 1, LimitPrice: 30},
		},
	}
	saga, err := engine.service.Begin("execution", opportunity, engine.runtime.Risk.Domain())
	if err != nil {
		t.Fatal(err)
	}
	engine.activeID = saga.ID
	engine.setActiveSaga(&saga)

	err = engine.tick(context.Background())
	if !errors.Is(err, ErrReconciliationRequired) {
		t.Fatalf("ошибка торгового цикла = %v", err)
	}
	snapshot := engine.Snapshot()
	if control.mode != domain.ModePaused || !snapshot.RecoveryBlocked ||
		snapshot.Mode != domain.ModePaused {
		t.Fatalf("движок не остановлен fail-closed: control=%+v snapshot=%+v", control, snapshot)
	}
	if runner.calls != 1 {
		t.Fatalf("число вызовов исполнителя = %d", runner.calls)
	}
	if err := engine.tick(context.Background()); err != nil {
		t.Fatalf("PAUSED tick завершился ошибкой: %v", err)
	}
	if runner.calls != 1 {
		t.Fatalf("денежная директива повторена после неоднозначной ошибки: %d", runner.calls)
	}
}

func TestAuditActionsAcceptsOnlyExplicitlyUnsentMoneyAction(t *testing.T) {
	t.Parallel()

	store := repository.NewMemory()
	action := domain.ActionRecord{
		ID:          "money-not-sent",
		Class:       string(protocol.ActionPurchase),
		RequestedAt: time.Date(2026, 7, 30, 1, 0, 0, 0, time.UTC),
	}
	if err := store.SaveAction(context.Background(), action); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveActionResult(context.Background(), domain.ActionResultRecord{
		ActionID:  action.ID,
		NotSent:   true,
		RetrySafe: true,
		Error:     "controller.Server.sendRequest: команда гарантированно не отправлена",
	}); err != nil {
		t.Fatal(err)
	}
	engine := &Engine{store: store}
	if err := engine.auditActions(
		context.Background(),
		map[string]struct{}{},
	); err != nil {
		t.Fatalf("однозначно неотправленное действие заблокировало восстановление: %v", err)
	}

	ambiguous := action
	ambiguous.ID = "money-ambiguous"
	ambiguous.RequestedAt = action.RequestedAt.Add(time.Second)
	if err := store.SaveAction(context.Background(), ambiguous); err != nil {
		t.Fatal(err)
	}
	if err := engine.auditActions(
		context.Background(),
		map[string]struct{}{},
	); err == nil ||
		!strings.Contains(err.Error(), ambiguous.ID) {
		t.Fatalf("неоднозначное денежное действие не заблокировало восстановление: %v", err)
	}
}

func TestAuditActionsChecksEveryActionPage(t *testing.T) {
	t.Parallel()

	store := repository.NewMemory()
	now := time.Date(2026, 7, 30, 1, 0, 0, 0, time.UTC)
	moneyAction := domain.ActionRecord{
		ID:          "money-hidden-on-second-page",
		Class:       string(protocol.ActionPurchase),
		RequestedAt: now,
	}
	if err := store.SaveAction(context.Background(), moneyAction); err != nil {
		t.Fatal(err)
	}
	for index := 0; index < auditPageSize; index++ {
		action := domain.ActionRecord{
			ID:          fmt.Sprintf("newer-navigation-%03d", index),
			Class:       string(protocol.ActionNavigation),
			RequestedAt: now.Add(time.Duration(index+1) * time.Second),
		}
		if err := store.SaveAction(context.Background(), action); err != nil {
			t.Fatal(err)
		}
	}
	engine := &Engine{store: store}
	if err := engine.auditActions(
		context.Background(),
		map[string]struct{}{},
	); err == nil ||
		!strings.Contains(err.Error(), moneyAction.ID) {
		t.Fatalf("денежное действие на второй странице не обнаружено: %v", err)
	}
}

func TestCheckpointAuditUsesExactActionIDsInsteadOfWallClock(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 30, 2, 0, 0, 0, time.UTC)
	store := repository.NewMemory()
	engine := newTestEngine(
		t,
		&fakeControl{mode: domain.ModePaused},
		store,
		now,
		nil,
	)
	value := testDirectOpportunity(now)
	if _, err := engine.service.Begin("execution", value, engine.runtime.Risk.Domain()); err != nil {
		t.Fatal(err)
	}
	saga, err := engine.service.Apply(trading.Event{
		ID:        "buy-event",
		SagaID:    "execution",
		Kind:      trading.EventStepSucceeded,
		StepIndex: 0,
		Outcome: trading.StepOutcome{
			CompletedQuantity: 1,
			PurchaseCost:      10,
			MonetaryActionID:  "purchase-action",
			InventoryDeltas: []domain.InventoryDelta{{
				ItemID: "item", QuantityDelta: 1, SlotsDelta: 1,
			}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.persistSaga(context.Background(), saga); err != nil {
		t.Fatal(err)
	}
	saveSuccessfulMoneyAction(
		t,
		store,
		"purchase-action",
		protocol.ActionPurchase,
		now.Add(-24*time.Hour),
	)
	if err := engine.auditCheckpointBoundary(context.Background()); err != nil {
		t.Fatalf("точно связанное действие не прошло аудит: %v", err)
	}

	saveSuccessfulMoneyAction(
		t,
		store,
		"unlinked-old-action",
		protocol.ActionPurchase,
		now.Add(-48*time.Hour),
	)
	if err := engine.auditCheckpointBoundary(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "unlinked-old-action") {
		t.Fatalf(
			"старое по wall-clock, но не связанное действие не заблокировало аудит: %v",
			err,
		)
	}
}

func TestEnginePersistsActiveOrderSnapshotOnSalePending(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 30, 2, 30, 0, 0, time.UTC)
	control := &fakeControl{mode: domain.ModeTrade}
	store := repository.NewMemory()
	engine := newTestEngine(t, control, store, now, nil)
	awaiting := prepareAwaitingSaleSaga(t, engine, now)
	runner := &snapshotDirectiveRunner{
		err: ErrSalePending, has: true,
		snapshot: engineOrderSnapshot(awaiting, orderStatusActive, now),
	}
	engine.runner = runner

	if err := engine.driveSaga(context.Background()); err != nil {
		t.Fatalf("активный ордер завершил мониторинг ошибкой: %v", err)
	}
	record, err := store.RuntimeRecord(
		context.Background(),
		orderSnapshotKey(awaiting.ID),
	)
	if err != nil {
		t.Fatalf("снимок активного ордера не сохранён: %v", err)
	}
	if record.Kind != orderSnapshotRecordKind {
		t.Fatalf("тип снимка = %q", record.Kind)
	}
	current, err := engine.service.Get(awaiting.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Status != trading.SagaAwaitingSale || current.Version != awaiting.Version {
		t.Fatalf("активный ордер изменил сагу: %+v", current)
	}
	if runner.calls != 1 || engine.nextOrders.IsZero() {
		t.Fatalf("мониторинг не запланирован повторно: calls=%d next=%s", runner.calls, engine.nextOrders)
	}
}

func TestEnginePersistsSoldSnapshotBeforeApplyingSettlement(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 30, 2, 40, 0, 0, time.UTC)
	control := &fakeControl{mode: domain.ModeTrade}
	store := repository.NewMemory()
	engine := newTestEngine(t, control, store, now, nil)
	awaiting := prepareAwaitingSaleSaga(t, engine, now)
	runner := &snapshotDirectiveRunner{
		has:      true,
		snapshot: engineOrderSnapshot(awaiting, orderStatusSold, now),
		event: trading.Event{
			ID: "settled-event", SagaID: awaiting.ID,
			Kind: trading.EventSaleSettled, OccurredAt: now,
			Settlement: &domain.TradeResult{
				ExecutionID: awaiting.ID, MarketOrderID: awaiting.MarketOrderID,
				InputCost: 10, Revenue: 30, Fees: 3,
				ResultItemID: "item", ResultQuantity: 1, CompletedAt: now,
			},
		},
	}
	engine.runner = runner

	if err := engine.driveSaga(context.Background()); err != nil {
		t.Fatalf("проданный ордер завершил мониторинг ошибкой: %v", err)
	}
	record, err := store.RuntimeRecord(
		context.Background(),
		orderSnapshotKey(awaiting.ID),
	)
	if err != nil {
		t.Fatalf("снимок проданного ордера не сохранён: %v", err)
	}
	var snapshot OrderSnapshot
	if err := json.Unmarshal(record.Payload, &snapshot); err != nil {
		t.Fatalf("снимок проданного ордера повреждён: %v", err)
	}
	if snapshot.SlotOccupied || snapshot.Status != orderStatusSold {
		t.Fatalf("неверный снимок проданного ордера: %+v", snapshot)
	}
	completed, err := engine.service.Get(awaiting.ID)
	if err != nil {
		t.Fatal(err)
	}
	if completed.Status != trading.SagaCompleted || engine.activeID != "" {
		t.Fatalf("settlement не применён: saga=%+v active=%q", completed, engine.activeID)
	}
}

func TestEngineMissingOrderSnapshotStopsForReconciliation(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 30, 2, 50, 0, 0, time.UTC)
	control := &fakeControl{mode: domain.ModeTrade}
	store := repository.NewMemory()
	engine := newTestEngine(t, control, store, now, nil)
	awaiting := prepareAwaitingSaleSaga(t, engine, now)
	engine.runner = &snapshotDirectiveRunner{err: ErrSalePending}

	err := engine.driveSaga(context.Background())
	if !errors.Is(err, ErrReconciliationRequired) {
		t.Fatalf("отсутствующий снимок вернул ошибку %v", err)
	}
	if control.mode != domain.ModePaused ||
		!engine.Snapshot().RecoveryBlocked {
		t.Fatalf("движок не остановлен fail-closed: control=%+v snapshot=%+v", control, engine.Snapshot())
	}
	if _, err := store.RuntimeRecord(
		context.Background(),
		orderSnapshotKey(awaiting.ID),
	); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("невалидный снимок был сохранён: %v", err)
	}
}

func TestEngineInvalidOrderSnapshotStopsForReconciliation(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 30, 2, 55, 0, 0, time.UTC)
	control := &fakeControl{mode: domain.ModeTrade}
	store := repository.NewMemory()
	engine := newTestEngine(t, control, store, now, nil)
	awaiting := prepareAwaitingSaleSaga(t, engine, now)
	snapshot := engineOrderSnapshot(awaiting, orderStatusActive, now)
	snapshot.AutomaticRepriceAllowed = true
	engine.runner = &snapshotDirectiveRunner{
		err: ErrSalePending, has: true, snapshot: snapshot,
	}

	err := engine.driveSaga(context.Background())
	if !errors.Is(err, ErrReconciliationRequired) {
		t.Fatalf("некорректный снимок вернул ошибку %v", err)
	}
	if control.mode != domain.ModePaused ||
		!engine.Snapshot().RecoveryBlocked {
		t.Fatalf("движок не остановлен fail-closed: control=%+v snapshot=%+v", control, engine.Snapshot())
	}
	if _, err := store.RuntimeRecord(
		context.Background(),
		orderSnapshotKey(awaiting.ID),
	); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("некорректный снимок был сохранён: %v", err)
	}
}

func TestCompletedMismatchPausesAndBlocksNewTrade(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 30, 3, 0, 0, 0, time.UTC)
	control := &fakeControl{mode: domain.ModeTrade}
	store := repository.NewMemory()
	engine := newTestEngine(t, control, store, now, nil)
	value := testDirectOpportunity(now)
	if _, err := engine.service.Begin("execution", value, engine.runtime.Risk.Domain()); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.service.Apply(trading.Event{
		ID:        "buy-event",
		SagaID:    "execution",
		Kind:      trading.EventStepSucceeded,
		StepIndex: 0,
		Outcome: trading.StepOutcome{
			CompletedQuantity: 1,
			PurchaseCost:      10,
			MonetaryActionID:  "buy-action",
			InventoryDeltas: []domain.InventoryDelta{{
				ItemID: "item", QuantityDelta: 1, SlotsDelta: 1,
			}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	awaiting, err := engine.service.Apply(trading.Event{
		ID:        "list-event",
		SagaID:    "execution",
		Kind:      trading.EventStepSucceeded,
		StepIndex: 1,
		Outcome: trading.StepOutcome{
			CompletedQuantity: 1,
			Fees:              1,
			MonetaryActionID:  "list-action",
			MarketOrderID:     "order-1",
			InventoryDeltas: []domain.InventoryDelta{{
				ItemID: "item", QuantityDelta: -1, SlotsDelta: -1,
			}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	engine.activeID = awaiting.ID
	engine.setActiveSaga(&awaiting)
	runner := &eventDirectiveRunner{event: trading.Event{
		ID:     "settled-event",
		SagaID: "execution",
		Kind:   trading.EventSaleSettled,
		Settlement: &domain.TradeResult{
			ExecutionID:    "execution",
			MarketOrderID:  "order-1",
			InputCost:      10,
			Revenue:        29,
			Fees:           3,
			ResultItemID:   "item",
			ResultQuantity: 1,
		},
	}, snapshot: engineOrderSnapshot(awaiting, orderStatusSold, now)}
	engine.runner = runner

	err = engine.tick(context.Background())
	if !errors.Is(err, ErrReconciliationRequired) {
		t.Fatalf("расхождение сверки вернуло ошибку %v", err)
	}
	snapshot := engine.Snapshot()
	if control.mode != domain.ModePaused ||
		!snapshot.RecoveryBlocked ||
		snapshot.ActiveSaga == nil ||
		snapshot.ActiveSaga.Status != trading.SagaCompletedMismatch ||
		engine.activeID != "" {
		t.Fatalf(
			"расхождение не остановило торговлю fail-closed: control=%+v snapshot=%+v active=%q",
			control,
			snapshot,
			engine.activeID,
		)
	}
	execution, err := store.Execution(context.Background(), "execution")
	if err != nil {
		t.Fatal(err)
	}
	if execution.Status != domain.TradeCompletedMismatch {
		t.Fatalf("статус исполнения = %s", execution.Status)
	}
	if runner.calls != 1 {
		t.Fatalf("число вызовов мониторинга продажи = %d", runner.calls)
	}
}

func TestRecoverRestoresPersistedRecoveryBlock(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 30, 4, 0, 0, 0, time.UTC)
	store := repository.NewMemory()
	if err := saveJSON(
		context.Background(),
		store,
		engineSnapshotKey,
		recordKindEngineSnapshot,
		EngineSnapshot{
			SessionID:       "previous-session",
			Mode:            domain.ModePaused,
			RecoveryBlocked: true,
			RecoveryReason:  "операторская сверка обязательна",
			UpdatedAt:       now.Add(-time.Minute),
		},
		now.Add(-time.Minute),
	); err != nil {
		t.Fatal(err)
	}
	control := &fakeControl{mode: domain.ModeTrade}
	engine := newTestEngine(t, control, store, now, nil)

	if err := engine.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	snapshot := engine.Snapshot()
	if !snapshot.RecoveryBlocked ||
		!strings.Contains(snapshot.RecoveryReason, "операторская сверка обязательна") ||
		snapshot.Mode != domain.ModePaused ||
		control.mode != domain.ModePaused {
		t.Fatalf(
			"сохранённая блокировка не восстановлена: control=%+v snapshot=%+v",
			control,
			snapshot,
		)
	}
	if err := engine.tick(context.Background()); err != nil {
		t.Fatalf("PAUSED после восстановления завершился ошибкой: %v", err)
	}
}

func TestStronglyConnectedIgnoresMonetaryEdges(t *testing.T) {
	t.Parallel()

	err := stronglyConnected(
		[]appconfig.Transition{
			{
				From:  domain.StateMainMenu,
				To:    domain.StateMarketHome,
				Class: protocol.ActionPurchase,
			},
			{
				From:  domain.StateMarketHome,
				To:    domain.StateMainMenu,
				Class: protocol.ActionNavigation,
			},
		},
		[]domain.ScreenState{domain.StateMainMenu, domain.StateMarketHome},
	)
	if err == nil || !strings.Contains(err.Error(), "MAIN_MENU") {
		t.Fatalf("денежное ребро ошибочно признано безопасной навигацией: %v", err)
	}
}

func TestValidateRoutesRequiresExactTradeCommitEdges(t *testing.T) {
	t.Parallel()

	engine := &Engine{runtime: appconfig.Runtime{
		Watchlist: []appconfig.WatchItem{{ID: "item", Enabled: true}},
		Navigation: appconfig.Navigation{
			Transitions: completeTradeReadinessTransitions(),
		},
	}}
	if err := engine.validateRoutes(domain.ModeTrade); err != nil {
		t.Fatalf("полный граф отклонён: %v", err)
	}

	for index := range engine.runtime.Navigation.Transitions {
		transition := &engine.runtime.Navigation.Transitions[index]
		if transition.Class == protocol.ActionPurchase {
			transition.From = domain.StateItemCard
		}
	}
	err := engine.validateRoutes(domain.ModeTrade)
	if err == nil ||
		!strings.Contains(err.Error(), "PURCHASE_DIALOG") ||
		!strings.Contains(err.Error(), "PURCHASE") {
		t.Fatalf("неверный источник PURCHASE не отклонён: %v", err)
	}
}

func TestValidateRoutesRequiresItemCardForScan(t *testing.T) {
	t.Parallel()

	transitions := completeTradeReadinessTransitions()
	filtered := transitions[:0]
	for _, transition := range transitions {
		if transition.From == domain.StateItemCard ||
			transition.To == domain.StateItemCard {
			continue
		}
		filtered = append(filtered, transition)
	}
	engine := &Engine{runtime: appconfig.Runtime{
		Watchlist:  []appconfig.WatchItem{{ID: "item", Enabled: true}},
		Navigation: appconfig.Navigation{Transitions: filtered},
	}}
	err := engine.validateRoutes(domain.ModeScan)
	if err == nil || !strings.Contains(err.Error(), "ITEM_CARD") {
		t.Fatalf("SCAN без маршрута карточки предмета не отклонён: %v", err)
	}
}

func completeTradeReadinessTransitions() []appconfig.Transition {
	navigable := []domain.ScreenState{
		domain.StateInventory,
		domain.StateMarketHome,
		domain.StateMarketResults,
		domain.StateItemCard,
		domain.StatePurchaseDialog,
		domain.StateSaleDialog,
	}
	transitions := make([]appconfig.Transition, 0, len(navigable)*2+3)
	for _, state := range navigable {
		transitions = append(
			transitions,
			appconfig.Transition{
				From: domain.StateMainMenu, To: state,
				Class: protocol.ActionNavigation,
			},
			appconfig.Transition{
				From: state, To: domain.StateMainMenu,
				Class: protocol.ActionNavigation,
			},
		)
	}
	transitions = append(
		transitions,
		appconfig.Transition{
			From: domain.StateConfirmation, To: domain.StateMainMenu,
			Class: protocol.ActionNavigation,
		},
		appconfig.Transition{
			From: domain.StatePurchaseDialog, To: domain.StateConfirmation,
			Class: protocol.ActionPurchase,
		},
		appconfig.Transition{
			From: domain.StateSaleDialog, To: domain.StateConfirmation,
			Class: protocol.ActionListing,
		},
	)
	return transitions
}

func saveSuccessfulMoneyAction(
	t *testing.T,
	store repository.Store,
	id string,
	class protocol.ActionClass,
	requestedAt time.Time,
) {
	t.Helper()
	if err := store.SaveAction(context.Background(), domain.ActionRecord{
		ID: id, Class: string(class), Kind: "CLICK", RequestedAt: requestedAt,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveActionResult(context.Background(), domain.ActionResultRecord{
		ActionID: id, Success: true, CompletedAt: requestedAt, ReceivedAt: requestedAt,
	}); err != nil {
		t.Fatal(err)
	}
}

func testDirectOpportunity(now time.Time) domain.TradeOpportunity {
	return domain.TradeOpportunity{
		ID:              "direct:item",
		Kind:            domain.OpportunityDirectFlip,
		InputCost:       10,
		ExpectedRevenue: 30,
		ExpectedFees:    3,
		Confidence:      1,
		LiquidityScore:  1,
		RequiredSlots:   1,
		QuoteObservedAt: now,
		ResaleKnown:     true,
		ResultItemID:    "item",
		ResultQuantity:  1,
		ExpiresAt:       now.Add(time.Hour),
		Steps: []domain.TradeStep{
			{Kind: domain.TradeStepBuy, ItemID: "item", Quantity: 1, LimitPrice: 10},
			{Kind: domain.TradeStepList, ItemID: "item", Quantity: 1, LimitPrice: 30},
		},
	}
}

func prepareAwaitingSaleSaga(
	t *testing.T,
	engine *Engine,
	now time.Time,
) trading.Saga {
	t.Helper()
	value := testDirectOpportunity(now)
	if _, err := engine.service.Begin(
		"execution",
		value,
		engine.runtime.Risk.Domain(),
	); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.service.Apply(trading.Event{
		ID: "buy-event", SagaID: "execution",
		Kind: trading.EventStepSucceeded, StepIndex: 0, OccurredAt: now,
		Outcome: trading.StepOutcome{
			CompletedQuantity: 1, PurchaseCost: 10, MonetaryActionID: "buy-action",
			InventoryDeltas: []domain.InventoryDelta{{
				ItemID: "item", QuantityDelta: 1, SlotsDelta: 1,
			}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	awaiting, err := engine.service.Apply(trading.Event{
		ID: "list-event", SagaID: "execution",
		Kind: trading.EventStepSucceeded, StepIndex: 1, OccurredAt: now,
		Outcome: trading.StepOutcome{
			CompletedQuantity: 1, Fees: 1, MonetaryActionID: "list-action",
			MarketOrderID: "order-1",
			InventoryDeltas: []domain.InventoryDelta{{
				ItemID: "item", QuantityDelta: -1, SlotsDelta: -1,
			}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	engine.activeID = awaiting.ID
	engine.setActiveSaga(&awaiting)
	return awaiting
}

func engineOrderSnapshot(
	saga trading.Saga,
	status string,
	observedAt time.Time,
) OrderSnapshot {
	slotOccupied, _ := orderSlotOccupied(status)
	return OrderSnapshot{
		SchemaVersion: orderSnapshotSchemaVersion,
		SagaID:        saga.ID, MarketOrderID: saga.MarketOrderID,
		ItemID: saga.Opportunity.ResultItemID, Status: status,
		ListedPrice: 30, CurrentMarketPrice: 30, MinimumAllowedPrice: 30,
		ObservedFrameID: 10, ObservedAt: observedAt,
		ListedAt: observedAt.Add(-time.Minute), AgeSeconds: 60,
		SlotOccupied: slotOccupied, RecommendationOnly: true,
		RecommendationReason: orderRepriceRecommendationReason(status),
		Confidence:           1,
	}
}

func orderRepriceRecommendationReason(status string) string {
	_, _, reason := orderRepriceRecommendation(status, 30, 30, 30)
	return reason
}

func newTestEngine(
	t *testing.T,
	control *fakeControl,
	store repository.Store,
	now time.Time,
	accountErr error,
) *Engine {
	t.Helper()
	runtime := testRuntime()
	finder, err := opportunity.NewFinder(opportunity.Config{QuoteTTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	tracker, err := inventory.NewTracker(2)
	if err != nil {
		t.Fatal(err)
	}
	service, err := trading.NewService(finder, tracker, trading.Config{
		ScoreWeights: economy.ScoreWeights{Profit: 1}, ProfitScale: 1,
		Clock: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	engine, err := NewEngine(
		control,
		fakeAccountScanner{
			value: AccountSnapshot{
				Balance: 100, FreeInventorySlots: 2, FreeMarketSlots: 2,
				Confidence: 1, ObservedAt: now,
			},
			err: accountErr,
		},
		fakeMarketScanner{report: ScanReport{FinishedAt: now}},
		fakeContactScanner{report: ScanReport{FinishedAt: now}},
		service,
		fakeDirectiveRunner{},
		store,
		runtime,
		logger,
		EngineConfig{Now: func() time.Time { return now }},
	)
	if err != nil {
		t.Fatal(err)
	}
	return engine
}

func testRuntime() appconfig.Runtime {
	return appconfig.Runtime{
		Version:            appconfig.CurrentVersion,
		DetectorConfigPath: "screens.json",
		Risk: appconfig.Risk{
			MaxBudget: 100, MaxItemPrice: 100, MinProfit: 1,
			MinROI: 0, MinConfidence: .9,
			MaxQuoteAge: appconfig.Duration(time.Minute), AvailableSlots: 2,
		},
		Watchlist: []appconfig.WatchItem{{
			ID: "item", Name: "Предмет", Enabled: true,
			MaxBuyPrice: 100, MinSalePrice: 20,
		}},
		Navigation: appconfig.Navigation{Transitions: []appconfig.Transition{{
			ID: "main-market", From: domain.StateMainMenu, To: domain.StateMarketHome,
			Actions: []appconfig.Action{{
				ID: "open-market", Kind: appconfig.ActionClick,
				Point: &domain.Point{X: .5, Y: .5}, Value: "LEFT",
			}},
			Verify: appconfig.Verification{
				State: domain.StateMarketHome, MinConfidence: .9,
				Timeout: appconfig.Duration(time.Second),
			},
		}}},
		Scanners: appconfig.Scanners{
			Market: appconfig.ScannerTiming{
				Interval: appconfig.Duration(time.Second), Staleness: appconfig.Duration(time.Minute),
			},
			Contacts: appconfig.ScannerTiming{
				Interval: appconfig.Duration(time.Second), Staleness: appconfig.Duration(time.Minute),
			},
			Orders: appconfig.ScannerTiming{
				Interval: appconfig.Duration(time.Second), Staleness: appconfig.Duration(time.Minute),
			},
		},
		Strategy: appconfig.Strategy{
			ProfitWeight: 1, ProfitScale: 1, MaxStepAttempts: 3,
			MaxMultistepDepth: 3, MaxRecipeExpansions: 100,
		},
	}
}

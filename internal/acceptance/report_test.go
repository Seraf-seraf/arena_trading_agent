package acceptance

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/arena-trading-agent/arena-trading-agent/internal/domain"
	"github.com/arena-trading-agent/arena-trading-agent/internal/economy"
	"github.com/arena-trading-agent/arena-trading-agent/internal/inventory"
	"github.com/arena-trading-agent/arena-trading-agent/internal/opportunity"
	"github.com/arena-trading-agent/arena-trading-agent/internal/protocol"
	"github.com/arena-trading-agent/arena-trading-agent/internal/repository"
	"github.com/arena-trading-agent/arena-trading-agent/internal/trading"
)

func TestGenerateAcceptsCompleteConsecutiveRuns(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := repository.NewMemory()
	base := time.Date(2026, 7, 30, 8, 0, 0, 0, time.UTC)
	seedAcceptedRun(t, ctx, store, "execution-1", "opportunity", base)
	seedAcceptedRun(t, ctx, store, "execution-2", "opportunity", base.Add(time.Hour))

	report, err := Generate(ctx, store, Options{
		Runs:          2,
		OpportunityID: "opportunity",
		Since:         base.Add(-time.Minute),
	})
	if err != nil {
		t.Fatalf("не удалось построить отчёт: %v", err)
	}
	if !report.Accepted {
		t.Fatalf("полный журнал отклонён: %+v", report)
	}
	if report.SelectedRuns != 2 || len(report.Runs) != 2 {
		t.Fatalf("выбрано неверное число прогонов: %+v", report)
	}
	if report.Runs[0].ExecutionID != "execution-1" ||
		report.Runs[1].ExecutionID != "execution-2" {
		t.Fatalf("прогоны не отсортированы хронологически: %+v", report.Runs)
	}
	if report.Criteria.CompletedExecutions != 2 ||
		report.Criteria.StrictlyCompletedSagas != 2 ||
		report.Criteria.MatchedReconciliations != 2 {
		t.Fatalf("неверные агрегаты завершения: %+v", report.Criteria)
	}
	if !report.Criteria.ZeroErroneousMoneyActions ||
		!report.Criteria.ZeroLostActions ||
		!report.Criteria.ZeroReconciliationMismatch ||
		!report.Criteria.AllActionsBoundToFrames ||
		!report.Criteria.AllMonetaryKindsPresent ||
		!report.Criteria.AllMoneyBoundToCheckpoints ||
		!report.Criteria.AllFixedRoutesValid ||
		!report.Criteria.RecoverySuccessRate100 ||
		!report.Criteria.NoEmergencyOrCriticalEvents {
		t.Fatalf("критерии полного прогона не выполнены: %+v", report.Criteria)
	}
}

func TestGenerateRejectsUnsafeOrIncompleteRun(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := repository.NewMemory()
	startedAt := time.Date(2026, 7, 30, 9, 0, 0, 0, time.UTC)
	updatedAt := startedAt.Add(10 * time.Minute)
	execution := domain.TradeExecution{
		ID:            "execution-failed",
		OpportunityID: "opportunity",
		Status:        domain.TradeCompleted,
		StartedAt:     startedAt,
		UpdatedAt:     updatedAt,
	}
	if err := store.SaveExecution(ctx, execution); err != nil {
		t.Fatalf("не удалось сохранить исполнение: %v", err)
	}
	checkpoint := trading.Checkpoint{
		SchemaVersion: 1,
		Saga: trading.Saga{
			ID:          execution.ID,
			Opportunity: domain.TradeOpportunity{ID: execution.OpportunityID},
			Status:      trading.SagaCompletedMismatch,
			Reconciliation: &domain.ReconciliationReport{
				ExecutionID:   execution.ID,
				OpportunityID: execution.OpportunityID,
				Matched:       false,
				Mismatches: []domain.ReconciliationMismatch{{
					Field: "profit", Expected: "10", Actual: "9",
				}},
			},
		},
	}
	saveCheckpoint(t, ctx, store, execution, checkpoint)

	saveAction(t, ctx, store, domain.ActionRecord{
		ID: "purchase-lost", Class: string(protocol.ActionPurchase),
		RequestedAt: startedAt.Add(time.Minute),
	})
	saveActionWithResult(t, ctx, store, domain.ActionRecord{
		ID: "barter-failed", Class: string(protocol.ActionBarter),
		BasedOnFrame: 11, RequestedAt: startedAt.Add(2 * time.Minute),
	}, domain.ActionResultRecord{
		ActionID: "barter-failed", Success: false, ResultFrame: 12,
		Error: "контрольный отказ", ReceivedAt: startedAt.Add(2*time.Minute + time.Second),
	})
	if err := store.SaveEvent(ctx, domain.AgentEventRecord{
		ID: "stop", Kind: "EMERGENCY_STOP_APPLIED", Severity: "critical",
		CreatedAt: startedAt.Add(3 * time.Minute),
	}); err != nil {
		t.Fatalf("не удалось сохранить аварийное событие: %v", err)
	}

	report, err := Generate(ctx, store, Options{Runs: 1})
	if err != nil {
		t.Fatalf("не удалось построить отчёт: %v", err)
	}
	if report.Accepted {
		t.Fatal("опасный и неполный журнал ошибочно принят")
	}
	if len(report.Runs) != 1 || report.Runs[0].Accepted {
		t.Fatalf("неверный результат прогона: %+v", report.Runs)
	}
	run := report.Runs[0]
	if run.LostActions != 1 || run.ErroneousMonetaryActions != 2 {
		t.Fatalf("неверно посчитаны потерянные/ошибочные действия: %+v", run)
	}
	if run.InvalidBasedOnFrames != 2 {
		t.Fatalf("неполные основания кадров не обнаружены: %+v", run)
	}
	if len(run.MissingMonetaryKinds) != 3 {
		t.Fatalf("отсутствующий LISTING не обнаружен: %+v", run.MissingMonetaryKinds)
	}
	if run.EmergencyEvents != 1 || run.CriticalEvents != 1 {
		t.Fatalf("аварийное событие не обнаружено: %+v", run)
	}
	if report.Criteria.ReconciliationFailures != 1 ||
		report.Criteria.ReconciliationMismatchItems != 1 ||
		report.Criteria.ZeroReconciliationMismatch {
		t.Fatalf("расхождение сверки не отражено в агрегате: %+v", report.Criteria)
	}
	if !containsReason(run.Reasons, "строгого") ||
		!containsReason(run.Reasons, "отсутствует ActionResult") ||
		!containsReason(run.Reasons, "аварийное событие") {
		t.Fatalf("причины отказа неполны: %+v", run.Reasons)
	}
}

func TestGenerateRejectsInsufficientLatestRuns(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := repository.NewMemory()
	seedAcceptedRun(
		t,
		ctx,
		store,
		"only-run",
		"opportunity",
		time.Date(2026, 7, 30, 10, 0, 0, 0, time.UTC),
	)

	report, err := Generate(ctx, store, Options{Runs: 2})
	if err != nil {
		t.Fatalf("не удалось построить отчёт: %v", err)
	}
	if report.Accepted {
		t.Fatal("один прогон ошибочно принят вместо двух")
	}
	if report.SelectedRuns != 1 || len(report.Reasons) == 0 {
		t.Fatalf("не отражён недостаток прогонов: %+v", report)
	}
}

func TestGenerateDoesNotHideInterleavedDifferentOpportunity(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := repository.NewMemory()
	base := time.Date(2026, 7, 30, 11, 0, 0, 0, time.UTC)
	seedAcceptedRun(t, ctx, store, "fixed-run", "fixed-opportunity", base)
	seedAcceptedRun(
		t,
		ctx,
		store,
		"foreign-run",
		"другая-возможность",
		base.Add(time.Hour),
	)

	report, err := Generate(ctx, store, Options{
		Runs:          2,
		OpportunityID: "fixed-opportunity",
		Since:         base.Add(-time.Minute),
	})
	if err != nil {
		t.Fatalf("не удалось построить отчёт: %v", err)
	}
	if report.Accepted || report.SelectedRuns != 2 ||
		!containsReason(report.Runs[1].Reasons, "вместо фиксированной") {
		t.Fatalf("чужой прогон был скрыт фильтром возможности: %+v", report)
	}
}

func TestGenerateRejectsSuccessfulMoneyActionOutsideCheckpoint(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := repository.NewMemory()
	startedAt := time.Date(2026, 7, 30, 13, 0, 0, 0, time.UTC)
	seedAcceptedRun(t, ctx, store, "execution", "opportunity", startedAt)

	requestedAt := startedAt.Add(10 * time.Minute)
	capturedAt := requestedAt.Add(-time.Second)
	saveActionWithResult(t, ctx, store, domain.ActionRecord{
		ID:                 "unbound-purchase",
		SessionID:          "session",
		Class:              string(protocol.ActionPurchase),
		Kind:               "CLICK",
		BasedOnFrame:       500,
		BasedOnCapturedAt:  &capturedAt,
		BasedOnFrameDigest: protocol.ComputeFrameDigest([]byte("unbound")),
		FrameBasisPayload:  testFrameBasisPayload(t, "unbound"),
		BasedOnState:       domain.StatePurchaseDialog,
		ExpectedState:      domain.StateConfirmation,
		ExpectedWidth:      1280,
		ExpectedHeight:     1024,
		ExpectedDPIPercent: 100,
		Deadline:           requestedAt.Add(time.Minute),
		RequestedAt:        requestedAt,
	}, domain.ActionResultRecord{
		ActionID:               "unbound-purchase",
		Success:                true,
		ResultFrame:            501,
		ResultState:            domain.StateConfirmation,
		VerificationConfidence: 1,
		CompletedAt:            requestedAt.Add(time.Second),
		ReceivedAt:             requestedAt.Add(2 * time.Second),
	})

	report, err := Generate(ctx, store, Options{Runs: 1})
	if err != nil {
		t.Fatalf("не удалось построить отчёт: %v", err)
	}
	if report.Accepted ||
		report.Criteria.UnboundMonetaryActions != 1 ||
		report.Criteria.AllMoneyBoundToCheckpoints {
		t.Fatalf("постороннее денежное действие не обнаружено: %+v", report)
	}
}

func TestGenerateRejectsSemanticallyTamperedCheckpoint(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := repository.NewMemory()
	startedAt := time.Date(2026, 7, 30, 14, 0, 0, 0, time.UTC)
	seedAcceptedRun(t, ctx, store, "execution", "opportunity", startedAt)
	record, err := store.RuntimeRecord(ctx, "saga/execution")
	if err != nil {
		t.Fatalf("не удалось прочитать контрольную точку: %v", err)
	}
	var checkpoint trading.Checkpoint
	if err := json.Unmarshal(record.Payload, &checkpoint); err != nil {
		t.Fatalf("не удалось декодировать контрольную точку: %v", err)
	}
	checkpoint.Processed[0].Snapshot.Failure = "сфабрикованный промежуточный снимок"
	payload, err := json.Marshal(checkpoint)
	if err != nil {
		t.Fatalf("не удалось сериализовать изменённую контрольную точку: %v", err)
	}
	record.Payload = payload
	if err := store.SaveRuntimeRecord(ctx, record); err != nil {
		t.Fatalf("не удалось сохранить изменённую контрольную точку: %v", err)
	}

	report, err := Generate(ctx, store, Options{Runs: 1})
	if err != nil {
		t.Fatalf("не удалось построить отчёт: %v", err)
	}
	if report.Accepted || len(report.Runs) != 1 || report.Runs[0].Accepted {
		t.Fatalf("семантически невозможная контрольная точка принята: %+v", report)
	}
	if !containsReason(report.Runs[0].Reasons, "строгую проверку") {
		t.Fatalf("причина строгого отказа отсутствует: %+v", report.Runs[0].Reasons)
	}
}

func seedAcceptedRun(
	t *testing.T,
	ctx context.Context,
	store repository.Store,
	executionID string,
	opportunityID string,
	startedAt time.Time,
) {
	t.Helper()
	updatedAt := startedAt.Add(20 * time.Minute)
	execution := domain.TradeExecution{
		ID:            executionID,
		OpportunityID: opportunityID,
		Status:        domain.TradeCompleted,
		StartedAt:     startedAt,
		UpdatedAt:     updatedAt,
	}
	if err := store.SaveExecution(ctx, execution); err != nil {
		t.Fatalf("не удалось сохранить исполнение %q: %v", executionID, err)
	}
	actionIDs := []string{
		executionID + "-purchase",
		executionID + "-barter",
		executionID + "-listing",
	}
	finder, err := opportunity.NewFinder(opportunity.Config{QuoteTTL: time.Hour})
	if err != nil {
		t.Fatalf("не удалось создать поиск возможностей: %v", err)
	}
	tracker, err := inventory.NewTracker(4)
	if err != nil {
		t.Fatalf("не удалось создать инвентарь: %v", err)
	}
	service, err := trading.NewService(finder, tracker, trading.Config{
		ScoreWeights: economy.ScoreWeights{Profit: 1},
		ProfitScale:  1,
		Clock:        func() time.Time { return startedAt },
	})
	if err != nil {
		t.Fatalf("не удалось создать торговый сервис: %v", err)
	}
	value := domain.TradeOpportunity{
		ID:              opportunityID,
		Kind:            domain.OpportunityContactBarter,
		InputCost:       10,
		ExpectedRevenue: 30,
		ExpectedFees:    2,
		Confidence:      1,
		LiquidityScore:  1,
		RequiredSlots:   1,
		QuoteObservedAt: startedAt,
		ResaleKnown:     true,
		ResultItemID:    "result",
		ResultQuantity:  1,
		ExpiresAt:       startedAt.Add(time.Hour),
		Steps: []domain.TradeStep{
			{
				Kind: domain.TradeStepBuy, ItemID: "ingredient",
				Quantity: 1, LimitPrice: 10,
			},
			{
				Kind: domain.TradeStepBarter, ItemID: "result",
				RecipeID: "known-recipe", Quantity: 1,
			},
			{
				Kind: domain.TradeStepList, ItemID: "result",
				Quantity: 1, LimitPrice: 30,
			},
		},
	}
	if _, err := service.Begin(executionID, value, domain.RiskLimits{
		MaxBudget: 100, MaxItemPrice: 100, MinProfit: 1,
		MinConfidence: .5, MaxQuoteAge: time.Hour, AvailableSlots: 4,
	}); err != nil {
		t.Fatalf("не удалось начать реальную торговую сагу: %v", err)
	}
	events := []trading.Event{
		{
			ID: "event-" + actionIDs[0], SagaID: executionID,
			Kind: trading.EventStepSucceeded, StepIndex: 0,
			Outcome: trading.StepOutcome{
				CompletedQuantity: 1, PurchaseCost: 10,
				MonetaryActionID: actionIDs[0],
				InventoryDeltas: []domain.InventoryDelta{{
					ItemID: "ingredient", QuantityDelta: 1, SlotsDelta: 1,
				}},
			},
			OccurredAt: startedAt.Add(2 * time.Minute),
		},
		{
			ID: "event-" + actionIDs[1], SagaID: executionID,
			Kind: trading.EventStepSucceeded, StepIndex: 1,
			Outcome: trading.StepOutcome{
				CompletedQuantity: 1, MonetaryActionID: actionIDs[1],
				InventoryDeltas: []domain.InventoryDelta{
					{ItemID: "ingredient", QuantityDelta: -1, SlotsDelta: -1},
					{ItemID: "result", QuantityDelta: 1, SlotsDelta: 1},
				},
			},
			OccurredAt: startedAt.Add(4 * time.Minute),
		},
		{
			ID: "event-" + actionIDs[2], SagaID: executionID,
			Kind: trading.EventStepSucceeded, StepIndex: 2,
			Outcome: trading.StepOutcome{
				CompletedQuantity: 1,
				Fees:              2,
				MonetaryActionID:  actionIDs[2],
				MarketOrderID:     "order-" + executionID,
				InventoryDeltas: []domain.InventoryDelta{{
					ItemID: "result", QuantityDelta: -1, SlotsDelta: -1,
				}},
			},
			OccurredAt: startedAt.Add(6 * time.Minute),
		},
		{
			ID: "event-settlement-" + executionID, SagaID: executionID,
			Kind: trading.EventSaleSettled,
			Settlement: &domain.TradeResult{
				ExecutionID:    executionID,
				MarketOrderID:  "order-" + executionID,
				InputCost:      10,
				Revenue:        30,
				Fees:           2,
				ResultItemID:   "result",
				ResultQuantity: 1,
				CompletedAt:    startedAt.Add(8 * time.Minute),
			},
			OccurredAt: startedAt.Add(8 * time.Minute),
		},
	}
	for _, event := range events {
		if _, err := service.Apply(event); err != nil {
			t.Fatalf("не удалось применить реальное событие %q: %v", event.ID, err)
		}
	}
	checkpoint, err := service.Checkpoint(executionID)
	if err != nil {
		t.Fatalf("не удалось получить реальную контрольную точку: %v", err)
	}
	if err := trading.ValidateCheckpointStrict(checkpoint, 3); err != nil {
		t.Fatalf("реальная контрольная точка не прошла строгую проверку: %v", err)
	}
	saveCheckpoint(t, ctx, store, execution, checkpoint)
	for index, class := range monetaryClasses() {
		actionID := actionIDs[index]
		requestedAt := startedAt.Add(time.Duration(index+1) * time.Minute)
		capturedAt := requestedAt.Add(-time.Second)
		saveActionWithResult(t, ctx, store, domain.ActionRecord{
			ID: actionID, SessionID: "session", Class: class, Kind: "CLICK",
			BasedOnFrame:       uint64(index + 1),
			BasedOnCapturedAt:  &capturedAt,
			BasedOnFrameDigest: protocol.ComputeFrameDigest([]byte(actionID)),
			FrameBasisPayload:  testFrameBasisPayload(t, actionID),
			BasedOnState:       domain.StatePurchaseDialog,
			ExpectedState:      domain.StateConfirmation,
			ExpectedWidth:      1280,
			ExpectedHeight:     1024,
			ExpectedDPIPercent: 100,
			Deadline:           requestedAt.Add(time.Minute),
			RequestedAt:        requestedAt,
		}, domain.ActionResultRecord{
			ActionID: actionID, Success: true, ResultFrame: uint64(index + 101),
			ResultState:            domain.StateConfirmation,
			VerificationConfidence: 1,
			CompletedAt:            requestedAt.Add(time.Second),
			ReceivedAt:             requestedAt.Add(2 * time.Second),
		})
	}
}

func saveCheckpoint(
	t *testing.T,
	ctx context.Context,
	store repository.Store,
	execution domain.TradeExecution,
	checkpoint trading.Checkpoint,
) {
	t.Helper()
	payload, err := json.Marshal(checkpoint)
	if err != nil {
		t.Fatalf("не удалось сериализовать контрольную точку: %v", err)
	}
	if err := store.SaveRuntimeRecord(ctx, domain.RuntimeRecord{
		Key:       "saga/" + execution.ID,
		Kind:      checkpointKind,
		Payload:   payload,
		UpdatedAt: execution.UpdatedAt,
	}); err != nil {
		t.Fatalf("не удалось сохранить контрольную точку: %v", err)
	}
}

func saveAction(
	t *testing.T,
	ctx context.Context,
	store repository.Store,
	action domain.ActionRecord,
) {
	t.Helper()
	if err := store.SaveAction(ctx, action); err != nil {
		t.Fatalf("не удалось сохранить действие %q: %v", action.ID, err)
	}
}

func testFrameBasisPayload(t *testing.T, seed string) json.RawMessage {
	t.Helper()
	payload, err := json.Marshal([]protocol.FrameRegionDigest{{
		Region: domain.Rectangle{X: 0.1, Y: 0.1, Width: 0.2, Height: 0.1},
		Digest: protocol.ComputeFrameDigest([]byte(seed + "-roi")),
	}})
	if err != nil {
		t.Fatalf("не удалось сериализовать ROI-основание: %v", err)
	}
	return payload
}

func saveActionWithResult(
	t *testing.T,
	ctx context.Context,
	store repository.Store,
	action domain.ActionRecord,
	result domain.ActionResultRecord,
) {
	t.Helper()
	saveAction(t, ctx, store, action)
	if err := store.SaveActionResult(ctx, result); err != nil {
		t.Fatalf("не удалось сохранить результат действия %q: %v", action.ID, err)
	}
}

func containsReason(reasons []string, fragment string) bool {
	for _, reason := range reasons {
		if strings.Contains(reason, fragment) {
			return true
		}
	}
	return false
}

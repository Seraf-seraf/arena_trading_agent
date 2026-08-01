package trading_test

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/arena-trading-agent/arena-trading-agent/internal/domain"
	"github.com/arena-trading-agent/arena-trading-agent/internal/economy"
	"github.com/arena-trading-agent/arena-trading-agent/internal/inventory"
	"github.com/arena-trading-agent/arena-trading-agent/internal/opportunity"
	"github.com/arena-trading-agent/arena-trading-agent/internal/trading"
)

var fixedNow = time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)

func TestEvaluateConnectsFinderEconomyRiskAndInventory(t *testing.T) {
	t.Parallel()
	service, _ := newService(t, 1)
	input := opportunity.Input{
		AsOf: fixedNow,
		Quotes: []domain.TradeQuote{
			quote("result", 100, 50),
			quote("b", 5, 5),
			quote("a", 10, 30),
		},
		Recipes: []domain.BarterRecipe{{
			ID:          "recipe",
			ContactID:   "contact",
			Ingredients: []domain.BarterIngredient{{ItemID: "b", Quantity: 1}, {ItemID: "a", Quantity: 1}},
			ResultItem:  "result",
			ResultCount: 1,
		}},
	}
	evaluation, err := service.Evaluate(input, limits())
	if err != nil {
		t.Fatal(err)
	}
	if len(evaluation.Candidates) != 1 ||
		evaluation.Candidates[0].Opportunity.ID != "direct:a" ||
		evaluation.Candidates[0].Opportunity.ExpectedProfit != 18 {
		t.Fatalf("unexpected accepted opportunities: %+v", evaluation.Candidates)
	}
	var rejected []string
	for _, value := range evaluation.Rejected {
		rejected = append(rejected, value.OpportunityID)
	}
	wantRejected := []string{"barter:recipe", "direct:b", "direct:result"}
	if !reflect.DeepEqual(rejected, wantRejected) {
		t.Fatalf("rejected IDs = %v, want %v", rejected, wantRejected)
	}
	if !strings.Contains(evaluation.Rejected[0].Reason, "слотов") {
		t.Fatalf("barter was not rejected by verified inventory capacity: %+v", evaluation.Rejected[0])
	}
}

func TestBeginIsIdempotentAndAllowsOnlyOneActiveSaga(t *testing.T) {
	t.Parallel()
	service, _ := newService(t, 4)
	value := directOpportunity("direct:item", "item", 1, 10, 30, 3)

	first, err := service.Begin("execution", value, limits())
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.Begin("execution", value, limits())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("idempotent Begin changed snapshot:\nfirst=%+v\nsecond=%+v", first, second)
	}
	if _, err := service.Begin("other", value, limits()); !errors.Is(err, trading.ErrActiveSaga) {
		t.Fatalf("second active saga error = %v", err)
	}
	changed := value
	changed.Confidence = .8
	if _, err := service.Begin("execution", changed, limits()); !errors.Is(
		err,
		trading.ErrIdempotencyConflict,
	) {
		t.Fatalf("changed Begin error = %v", err)
	}

	// Returned snapshots are defensive copies.
	first.Opportunity.Steps[0].ItemID = "mutated"
	stored, err := service.Get("execution")
	if err != nil {
		t.Fatal(err)
	}
	if stored.Opportunity.Steps[0].ItemID != "item" {
		t.Fatal("caller mutated stored saga through returned slice")
	}
}

func TestSynchronizeInventoryUsesCompleteSnapshotAndRejectsActiveSaga(t *testing.T) {
	t.Parallel()
	service, tracker := newService(t, 2)
	snapshot := domain.InventorySnapshot{
		CapacitySlots: 5,
		UsedSlots:     2,
		Items: []domain.InventoryItem{{
			ItemID: "existing", Quantity: 4, Slots: 2,
		}},
	}
	if err := service.SynchronizeInventory(snapshot); err != nil {
		t.Fatal(err)
	}
	got := service.InventorySnapshot()
	if got.CapacitySlots != 5 || got.UsedSlots != 2 ||
		tracker.Available("existing") != 4 || tracker.FreeSlots() != 3 {
		t.Fatalf("синхронизированный инвентарь = %+v", got)
	}

	value := directOpportunity("direct:item", "item", 1, 10, 30, 3)
	if _, err := service.Begin("execution", value, limits()); err != nil {
		t.Fatal(err)
	}
	before := service.InventorySnapshot()
	if err := service.SynchronizeInventory(domain.InventorySnapshot{
		CapacitySlots: 9,
	}); !errors.Is(err, trading.ErrActiveSaga) {
		t.Fatalf("ошибка синхронизации при активной саге = %v", err)
	}
	if after := service.InventorySnapshot(); !reflect.DeepEqual(after, before) {
		t.Fatalf("отклонённая синхронизация изменила инвентарь: до=%+v после=%+v", before, after)
	}
}

func TestCheckpointRestorePreservesInventoryAndEventIdempotency(t *testing.T) {
	t.Parallel()
	service, _ := newService(t, 4)
	value := directOpportunity("direct:item", "item", 1, 10, 30, 3)
	if _, err := service.Begin("execution", value, limits()); err != nil {
		t.Fatal(err)
	}
	buy := trading.Event{
		ID:        "buy-result",
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
	}
	afterBuy, err := service.Apply(buy)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, err := service.Checkpoint("execution")
	if err != nil {
		t.Fatal(err)
	}
	actionIDs, err := trading.MonetaryActionIDs(checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(actionIDs, []string{"buy-action"}) {
		t.Fatalf("идентификаторы денежных действий = %v", actionIDs)
	}
	checkpoint.Processed[0].Event.Outcome.MonetaryActionID = "изменён-снаружи"
	freshCheckpoint, err := service.Checkpoint("execution")
	if err != nil {
		t.Fatal(err)
	}
	if freshCheckpoint.Processed[0].Event.Outcome.MonetaryActionID != "buy-action" {
		t.Fatalf("внешнее изменение затронуло checkpoint сервиса: %+v", freshCheckpoint)
	}
	checkpoint = freshCheckpoint

	restored, tracker := newService(t, 4)
	got, err := restored.Restore(checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, afterBuy) {
		t.Fatalf("restored saga differs:\ngot=%+v\nwant=%+v", got, afterBuy)
	}
	if tracker.Available("item") != 1 || tracker.FreeSlots() != 3 {
		t.Fatalf("restored inventory is inconsistent: %+v", tracker.Snapshot())
	}
	replayed, err := restored.Apply(buy)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(replayed, afterBuy) {
		t.Fatalf("event replay changed restored saga: %+v", replayed)
	}
	directive, err := restored.Next("execution")
	if err != nil {
		t.Fatal(err)
	}
	if directive.Step == nil || directive.Step.Kind != domain.TradeStepList {
		t.Fatalf("unexpected restored directive: %+v", directive)
	}
}

func TestRestoreRejectsCorruptCheckpoint(t *testing.T) {
	t.Parallel()
	service, _ := newService(t, 4)
	value := directOpportunity("direct:item", "item", 1, 10, 30, 3)
	if _, err := service.Begin("execution", value, limits()); err != nil {
		t.Fatal(err)
	}
	checkpoint, err := service.Checkpoint("execution")
	if err != nil {
		t.Fatal(err)
	}
	checkpoint.Saga.Version++

	restored, _ := newService(t, 4)
	if _, err := restored.Restore(checkpoint); !errors.Is(err, trading.ErrInvalidCheckpoint) {
		t.Fatalf("Restore error = %v", err)
	}
}

func TestValidateCheckpointStrictRejectsSemanticTampering(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		tamper func(*trading.Checkpoint)
	}{
		{
			name: "canonical opportunity",
			tamper: func(checkpoint *trading.Checkpoint) {
				checkpoint.Saga.Opportunity.ExpectedProfit++
				for index := range checkpoint.Processed {
					checkpoint.Processed[index].Snapshot.Opportunity.ExpectedProfit++
				}
			},
		},
		{
			name: "intermediate snapshot",
			tamper: func(checkpoint *trading.Checkpoint) {
				checkpoint.Processed[0].Snapshot.Actual.InputCost--
				checkpoint.Processed[0].Snapshot.Actual.Profit++
			},
		},
		{
			name: "event outcome",
			tamper: func(checkpoint *trading.Checkpoint) {
				checkpoint.Processed[0].Event.Outcome.PurchaseCost--
			},
		},
		{
			name: "inventory delta",
			tamper: func(checkpoint *trading.Checkpoint) {
				checkpoint.Processed[0].Event.Outcome.InventoryDeltas[0].QuantityDelta++
			},
		},
		{
			name: "permuted snapshot versions",
			tamper: func(checkpoint *trading.Checkpoint) {
				checkpoint.Processed[0].Snapshot.Version, checkpoint.Processed[1].Snapshot.Version =
					checkpoint.Processed[1].Snapshot.Version, checkpoint.Processed[0].Snapshot.Version
			},
		},
		{
			name: "version gap",
			tamper: func(checkpoint *trading.Checkpoint) {
				checkpoint.Processed[0].Snapshot.Version++
			},
		},
		{
			name: "zero occurred at",
			tamper: func(checkpoint *trading.Checkpoint) {
				checkpoint.Processed[0].Event.OccurredAt = time.Time{}
			},
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			checkpoint := completedDirectCheckpoint(t)
			test.tamper(&checkpoint)
			if err := trading.ValidateCheckpointStrict(checkpoint, 3); err == nil {
				t.Fatal("семантически невозможная контрольная точка принята")
			}
			if _, err := trading.MonetaryActionIDs(checkpoint); err == nil {
				t.Fatal("денежные действия извлечены из невозможной контрольной точки")
			}
			restored, _ := newService(t, 4)
			if _, err := restored.Restore(checkpoint); !errors.Is(err, trading.ErrInvalidCheckpoint) {
				t.Fatalf("Restore error = %v", err)
			}
		})
	}
}

func TestValidateCheckpointStrictUsesRecoveryAttemptLimit(t *testing.T) {
	t.Parallel()

	service, _ := newServiceWithMaxAttempts(t, 4, 5)
	value := directOpportunity("direct:item", "item", 2, 10, 30, 3)
	if _, err := service.Begin("execution", value, limits()); err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		outcome := trading.StepOutcome{}
		if attempt == 0 {
			outcome = trading.StepOutcome{
				CompletedQuantity: 1,
				PurchaseCost:      10,
				MonetaryActionID:  "failure-action-0",
				InventoryDeltas: []domain.InventoryDelta{{
					ItemID: "item", QuantityDelta: 1, SlotsDelta: 1,
				}},
			}
		}
		if _, err := service.Apply(trading.Event{
			ID:        fmt.Sprintf("failure-%d", attempt),
			SagaID:    "execution",
			Kind:      trading.EventStepFailed,
			StepIndex: 0,
			Reason:    trading.FailureTransient,
			Outcome:   outcome,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := service.Apply(trading.Event{
			ID:         fmt.Sprintf("retry-%d", attempt),
			SagaID:     "execution",
			Kind:       trading.EventRecoveryResolved,
			StepIndex:  0,
			Resolution: trading.RecoveryRetry,
		}); err != nil {
			t.Fatal(err)
		}
	}
	checkpoint, err := service.Checkpoint("execution")
	if err != nil {
		t.Fatal(err)
	}
	if err := trading.ValidateCheckpointStrict(checkpoint, 2); err == nil {
		t.Fatal("replay сверх лимита попыток принят")
	}
	if err := trading.ValidateCheckpointStrict(checkpoint, 5); err != nil {
		t.Fatalf("replay в пределах лимита отклонён: %v", err)
	}
}

func TestDirectSagaIsIdempotentAndReconciles(t *testing.T) {
	t.Parallel()
	service, tracker := newService(t, 4)
	value := directOpportunity("direct:item", "item", 1, 10, 30, 3)
	started, err := service.Begin("execution", value, limits())
	if err != nil {
		t.Fatal(err)
	}
	if started.Status != trading.SagaRunning || started.ReservedBudget != 10 || started.Version != 1 {
		t.Fatalf("unexpected initial saga: %+v", started)
	}
	directive, err := service.Next("execution")
	if err != nil {
		t.Fatal(err)
	}
	if directive.Kind != trading.DirectiveExecuteStep ||
		directive.IdempotencyKey != "execution:0:0" ||
		directive.Step == nil || directive.Step.Kind != domain.TradeStepBuy {
		t.Fatalf("unexpected first directive: %+v", directive)
	}

	buy := trading.Event{
		ID:        "buy-result",
		SagaID:    "execution",
		Kind:      trading.EventStepSucceeded,
		StepIndex: 0,
		Outcome: trading.StepOutcome{
			CompletedQuantity: 1,
			PurchaseCost:      10,
			MonetaryActionID:  "buy-action",
			InventoryDeltas: []domain.InventoryDelta{{
				ItemID:        "item",
				QuantityDelta: 1,
				SlotsDelta:    1,
			}},
		},
	}
	afterBuy, err := service.Apply(buy)
	if err != nil {
		t.Fatal(err)
	}
	if afterBuy.CurrentStep != 1 || afterBuy.Version != 2 ||
		afterBuy.Actual.InputCost != 10 || afterBuy.Actual.Profit != -10 {
		t.Fatalf("unexpected post-buy saga: %+v", afterBuy)
	}

	list := trading.Event{
		ID:        "list-result",
		SagaID:    "execution",
		Kind:      trading.EventStepSucceeded,
		StepIndex: 1,
		Outcome: trading.StepOutcome{
			CompletedQuantity: 1,
			Fees:              1,
			MonetaryActionID:  "list-action",
			MarketOrderID:     "order-1",
			InventoryDeltas: []domain.InventoryDelta{{
				ItemID:        "item",
				QuantityDelta: -1,
				SlotsDelta:    -1,
			}},
		},
	}
	afterList, err := service.Apply(list)
	if err != nil {
		t.Fatal(err)
	}
	if afterList.Status != trading.SagaAwaitingSale ||
		len(afterList.Holdings) != 0 {
		t.Fatalf("unexpected listed saga: %+v", afterList)
	}
	if directive, err = service.Next("execution"); err != nil ||
		directive.Kind != trading.DirectiveMonitorSale {
		t.Fatalf("sale directive = %+v, err=%v", directive, err)
	}

	// A late retry of the first result returns its original response and does
	// not apply the inventory delta again.
	replayed, err := service.Apply(buy)
	if err != nil {
		t.Fatal(err)
	}
	if replayed.Version != 2 || replayed.CurrentStep != 1 {
		t.Fatalf("duplicate returned unexpected original response: %+v", replayed)
	}
	if snapshot := tracker.Snapshot(); snapshot.Revision != 2 || len(snapshot.Items) != 0 {
		t.Fatalf("duplicate event changed inventory: %+v", snapshot)
	}
	conflict := buy
	conflict.Outcome.PurchaseCost = 9
	if _, err := service.Apply(conflict); !errors.Is(err, trading.ErrIdempotencyConflict) {
		t.Fatalf("conflicting duplicate error = %v", err)
	}
	if _, err := service.Apply(trading.Event{
		ID:     "foreign-sale-result",
		SagaID: "execution",
		Kind:   trading.EventSaleSettled,
		Settlement: &domain.TradeResult{
			ExecutionID:    "execution",
			MarketOrderID:  "чужой-ордер",
			InputCost:      10,
			Revenue:        30,
			Fees:           3,
			ResultItemID:   "item",
			ResultQuantity: 1,
		},
	}); !errors.Is(err, trading.ErrInvalidTransition) {
		t.Fatalf("продажа другого ордера не была отклонена: %v", err)
	}

	completed, err := service.Apply(trading.Event{
		ID:     "sale-result",
		SagaID: "execution",
		Kind:   trading.EventSaleSettled,
		Settlement: &domain.TradeResult{
			ExecutionID:    "execution",
			MarketOrderID:  "order-1",
			InputCost:      10,
			Revenue:        30,
			Fees:           3,
			ResultItemID:   "item",
			ResultQuantity: 1,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if completed.Status != trading.SagaCompleted ||
		completed.ReservedBudget != 0 ||
		completed.Reconciliation == nil ||
		!completed.Reconciliation.Matched ||
		completed.Actual.Profit != 17 {
		t.Fatalf("unexpected completed saga: %+v", completed)
	}
	if directive, err = service.Next("execution"); err != nil ||
		directive.Kind != trading.DirectiveDone {
		t.Fatalf("terminal directive = %+v, err=%v", directive, err)
	}
	if _, err := service.Begin("next", value, limits()); err != nil {
		t.Fatalf("terminal saga did not release single-transaction gate: %v", err)
	}
}

func TestListingRequiresVerifiedMarketOrderIdentity(t *testing.T) {
	t.Parallel()

	service, _ := newService(t, 2)
	value := directOpportunity("direct:item", "item", 1, 10, 30, 3)
	if _, err := service.Begin("execution", value, limits()); err != nil {
		t.Fatal(err)
	}
	mustBuy(t, service, "buy", 0, "item", 10, 1)

	_, err := service.Apply(trading.Event{
		ID:        "list-without-order",
		SagaID:    "execution",
		Kind:      trading.EventStepSucceeded,
		StepIndex: 1,
		Outcome: trading.StepOutcome{
			CompletedQuantity: 1,
			Fees:              1,
			MonetaryActionID:  "list-without-order-action",
			InventoryDeltas: []domain.InventoryDelta{{
				ItemID: "item", QuantityDelta: -1, SlotsDelta: -1,
			}},
		},
	})
	if !errors.Is(err, trading.ErrInvalidTransition) {
		t.Fatalf("выставление без идентификатора ордера не было отклонено: %v", err)
	}
	saga, getErr := service.Get("execution")
	if getErr != nil {
		t.Fatal(getErr)
	}
	if saga.Status != trading.SagaRunning || saga.CurrentStep != 1 ||
		saga.MarketOrderID != "" {
		t.Fatalf("отклонённое выставление изменило сагу: %+v", saga)
	}
}

func TestSuccessfulStepRequiresUniqueMonetaryActionIdentity(t *testing.T) {
	t.Parallel()

	service, _ := newService(t, 2)
	value := directOpportunity("direct:item", "item", 1, 10, 30, 3)
	if _, err := service.Begin("execution", value, limits()); err != nil {
		t.Fatal(err)
	}
	_, err := service.Apply(trading.Event{
		ID:        "buy-without-action",
		SagaID:    "execution",
		Kind:      trading.EventStepSucceeded,
		StepIndex: 0,
		Outcome: trading.StepOutcome{
			CompletedQuantity: 1,
			PurchaseCost:      10,
			InventoryDeltas: []domain.InventoryDelta{{
				ItemID: "item", QuantityDelta: 1, SlotsDelta: 1,
			}},
		},
	})
	if !errors.Is(err, trading.ErrInvalidTransition) {
		t.Fatalf("успешный шаг без UI action ID не отклонён: %v", err)
	}

	mustBuy(t, service, "buy", 0, "item", 10, 1)
	_, err = service.Apply(trading.Event{
		ID:        "list-reusing-action",
		SagaID:    "execution",
		Kind:      trading.EventStepSucceeded,
		StepIndex: 1,
		Outcome: trading.StepOutcome{
			CompletedQuantity: 1,
			Fees:              1,
			MonetaryActionID:  "buy-action",
			MarketOrderID:     "order-1",
			InventoryDeltas: []domain.InventoryDelta{{
				ItemID: "item", QuantityDelta: -1, SlotsDelta: -1,
			}},
		},
	})
	if !errors.Is(err, trading.ErrIdempotencyConflict) {
		t.Fatalf("повторное использование UI action ID не отклонено: %v", err)
	}
}

func TestPurchaseCeilingRejectsEventAtomically(t *testing.T) {
	t.Parallel()
	service, tracker := newService(t, 2)
	value := directOpportunity("direct:item", "item", 1, 10, 30, 3)
	if _, err := service.Begin("execution", value, limits()); err != nil {
		t.Fatal(err)
	}
	_, err := service.Apply(trading.Event{
		ID:        "over-limit",
		SagaID:    "execution",
		Kind:      trading.EventStepSucceeded,
		StepIndex: 0,
		Outcome: trading.StepOutcome{
			CompletedQuantity: 1,
			PurchaseCost:      11,
			MonetaryActionID:  "over-limit-action",
			InventoryDeltas: []domain.InventoryDelta{{
				ItemID:        "item",
				QuantityDelta: 1,
				SlotsDelta:    1,
			}},
		},
	})
	if !errors.Is(err, trading.ErrInvalidTransition) {
		t.Fatalf("over-limit event error = %v", err)
	}
	stored, _ := service.Get("execution")
	if stored.Version != 1 || stored.CurrentStep != 0 || tracker.Snapshot().Revision != 0 {
		t.Fatalf("rejected event changed saga or inventory: saga=%+v inventory=%+v", stored, tracker.Snapshot())
	}
}

func TestPartialBuyCanRetryOnlyRemainingQuantity(t *testing.T) {
	t.Parallel()
	service, _ := newService(t, 4)
	value := directOpportunity("direct:stack", "stack", 2, 10, 30, 4)
	if _, err := service.Begin("execution", value, limits()); err != nil {
		t.Fatal(err)
	}
	failed, err := service.Apply(trading.Event{
		ID:        "partial",
		SagaID:    "execution",
		Kind:      trading.EventStepFailed,
		StepIndex: 0,
		Reason:    trading.FailureItemUnavailable,
		Outcome: trading.StepOutcome{
			CompletedQuantity: 1,
			PurchaseCost:      10,
			MonetaryActionID:  "partial-action",
			InventoryDeltas: []domain.InventoryDelta{{
				ItemID:        "stack",
				QuantityDelta: 1,
				SlotsDelta:    1,
			}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if failed.Status != trading.SagaRecovering ||
		failed.StepProgress != 1 ||
		failed.Compensation == nil ||
		failed.Compensation.Kind != trading.CompensationSellOrWait {
		t.Fatalf("unexpected partial-buy recovery: %+v", failed)
	}
	if _, err := service.Apply(trading.Event{
		ID:         "retry",
		SagaID:     "execution",
		Kind:       trading.EventRecoveryResolved,
		StepIndex:  0,
		Resolution: trading.RecoveryRetry,
	}); err != nil {
		t.Fatal(err)
	}
	next, err := service.Next("execution")
	if err != nil {
		t.Fatal(err)
	}
	if next.Step == nil || next.Step.Quantity != 1 ||
		next.IdempotencyKey != "execution:0:1" {
		t.Fatalf("retry did not reduce quantity or change attempt key: %+v", next)
	}
	if _, err := service.Apply(trading.Event{
		ID:        "failed-again",
		SagaID:    "execution",
		Kind:      trading.EventStepFailed,
		StepIndex: 0,
		Reason:    trading.FailureTransient,
	}); err != nil {
		t.Fatal(err)
	}
	mustRetry(t, service, "retry-last", 0)
	if _, err := service.Apply(trading.Event{
		ID:        "failed-third-time",
		SagaID:    "execution",
		Kind:      trading.EventStepFailed,
		StepIndex: 0,
		Reason:    trading.FailureTransient,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Apply(trading.Event{
		ID:         "retry-too-many",
		SagaID:     "execution",
		Kind:       trading.EventRecoveryResolved,
		StepIndex:  0,
		Resolution: trading.RecoveryRetry,
	}); !errors.Is(err, trading.ErrInvalidTransition) {
		t.Fatalf("attempt limit error = %v", err)
	}
}

func TestFailedSecondIngredientCanBeCompensated(t *testing.T) {
	t.Parallel()
	service, tracker := newService(t, 4)
	value := contactOpportunity()
	if _, err := service.Begin("execution", value, limits()); err != nil {
		t.Fatal(err)
	}
	mustBuy(t, service, "buy-a", 0, "a", 10, 1)

	failed, err := service.Apply(trading.Event{
		ID:        "buy-b-failed",
		SagaID:    "execution",
		Kind:      trading.EventStepFailed,
		StepIndex: 1,
		Reason:    trading.FailureItemUnavailable,
		Message:   "ingredient b disappeared",
	})
	if err != nil {
		t.Fatal(err)
	}
	if failed.Status != trading.SagaRecovering ||
		failed.Compensation == nil ||
		failed.Compensation.Kind != trading.CompensationSellOrWait ||
		!reflect.DeepEqual(failed.Compensation.Holdings, []trading.Holding{{ItemID: "a", Quantity: 1}}) {
		t.Fatalf("unexpected ingredient recovery: %+v", failed)
	}
	compensated, err := service.Apply(trading.Event{
		ID:         "sold-a",
		SagaID:     "execution",
		Kind:       trading.EventRecoveryResolved,
		StepIndex:  1,
		Resolution: trading.RecoveryCompensated,
		Outcome: trading.StepOutcome{
			Revenue:          8,
			Fees:             1,
			MonetaryActionID: "compensation-action",
			InventoryDeltas: []domain.InventoryDelta{{
				ItemID:        "a",
				QuantityDelta: -1,
				SlotsDelta:    -1,
			}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if compensated.Status != trading.SagaCompensated ||
		compensated.ReservedBudget != 0 ||
		compensated.Actual.Profit != -3 ||
		len(compensated.Holdings) != 0 ||
		len(tracker.Snapshot().Items) != 0 {
		t.Fatalf("unexpected compensation result: saga=%+v inventory=%+v", compensated, tracker.Snapshot())
	}
}

func TestBarterCooldownAndMissingMarketSlotAreRetryable(t *testing.T) {
	t.Parallel()
	service, tracker := newService(t, 4)
	if _, err := service.Begin("execution", contactOpportunity(), limits()); err != nil {
		t.Fatal(err)
	}
	mustBuy(t, service, "buy-a", 0, "a", 10, 1)
	mustBuy(t, service, "buy-b", 1, "b", 5, 1)

	waiting, err := service.Apply(trading.Event{
		ID:        "barter-unavailable",
		SagaID:    "execution",
		Kind:      trading.EventStepFailed,
		StepIndex: 2,
		Reason:    trading.FailureBarterUnavailable,
	})
	if err != nil {
		t.Fatal(err)
	}
	if waiting.Status != trading.SagaWaitingCooldown ||
		waiting.Compensation == nil ||
		waiting.Compensation.Kind != trading.CompensationWaitForCooldown {
		t.Fatalf("unexpected cooldown recovery: %+v", waiting)
	}
	mustRetry(t, service, "cooldown-over", 2)

	bartered, err := service.Apply(trading.Event{
		ID:        "bartered",
		SagaID:    "execution",
		Kind:      trading.EventStepSucceeded,
		StepIndex: 2,
		Outcome: trading.StepOutcome{
			CompletedQuantity: 1,
			MonetaryActionID:  "barter-action",
			InventoryDeltas: []domain.InventoryDelta{
				{ItemID: "a", QuantityDelta: -1, SlotsDelta: -1},
				{ItemID: "b", QuantityDelta: -1, SlotsDelta: -1},
				{ItemID: "result", QuantityDelta: 1, SlotsDelta: 1},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(bartered.Holdings, []trading.Holding{{ItemID: "result", Quantity: 1}}) {
		t.Fatalf("unexpected barter holdings: %+v", bartered)
	}

	queued, err := service.Apply(trading.Event{
		ID:        "no-slot",
		SagaID:    "execution",
		Kind:      trading.EventStepFailed,
		StepIndex: 3,
		Reason:    trading.FailureMarketSlotUnavailable,
	})
	if err != nil {
		t.Fatal(err)
	}
	if queued.Status != trading.SagaWaitingMarketSlot ||
		queued.Compensation == nil ||
		queued.Compensation.Kind != trading.CompensationQueueForSlot {
		t.Fatalf("unexpected listing queue state: %+v", queued)
	}
	mustRetry(t, service, "slot-free", 3)
	listed, err := service.Apply(trading.Event{
		ID:        "listed",
		SagaID:    "execution",
		Kind:      trading.EventStepSucceeded,
		StepIndex: 3,
		Outcome: trading.StepOutcome{
			CompletedQuantity: 1,
			Fees:              1,
			MonetaryActionID:  "listing-action",
			MarketOrderID:     "order-queued",
			InventoryDeltas: []domain.InventoryDelta{{
				ItemID:        "result",
				QuantityDelta: -1,
				SlotsDelta:    -1,
			}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if listed.Status != trading.SagaAwaitingSale ||
		len(listed.Holdings) != 0 ||
		tracker.Snapshot().UsedSlots != 0 {
		t.Fatalf("unexpected state after queued listing: saga=%+v inventory=%+v", listed, tracker.Snapshot())
	}
}

func TestUnavailableSaleCanHoldVerifiedResult(t *testing.T) {
	t.Parallel()
	service, tracker := newService(t, 2)
	value := directOpportunity("direct:item", "item", 1, 10, 30, 3)
	if _, err := service.Begin("execution", value, limits()); err != nil {
		t.Fatal(err)
	}
	mustBuy(t, service, "buy", 0, "item", 10, 1)
	recovering, err := service.Apply(trading.Event{
		ID:        "sale-unavailable",
		SagaID:    "execution",
		Kind:      trading.EventStepFailed,
		StepIndex: 1,
		Reason:    trading.FailureTransient,
	})
	if err != nil {
		t.Fatal(err)
	}
	if recovering.Status != trading.SagaRecovering ||
		recovering.Compensation == nil ||
		recovering.Compensation.Kind != trading.CompensationHoldResult {
		t.Fatalf("unexpected hold-result recovery: %+v", recovering)
	}
	held, err := service.Apply(trading.Event{
		ID:         "hold",
		SagaID:     "execution",
		Kind:       trading.EventRecoveryResolved,
		StepIndex:  1,
		Resolution: trading.RecoveryHold,
	})
	if err != nil {
		t.Fatal(err)
	}
	if held.Status != trading.SagaHeld ||
		held.ReservedBudget != 0 ||
		!reflect.DeepEqual(held.Holdings, []trading.Holding{{ItemID: "item", Quantity: 1}}) ||
		tracker.Available("item") != 1 {
		t.Fatalf("verified result was not safely held: saga=%+v inventory=%+v", held, tracker.Snapshot())
	}
}

func TestNoPurchaseNeedsNoCompensationAndMismatchIsExplicit(t *testing.T) {
	t.Parallel()
	t.Run("no purchase", func(t *testing.T) {
		service, _ := newService(t, 2)
		value := directOpportunity("direct:item", "item", 1, 10, 30, 3)
		if _, err := service.Begin("execution", value, limits()); err != nil {
			t.Fatal(err)
		}
		failed, err := service.Apply(trading.Event{
			ID:        "not-bought",
			SagaID:    "execution",
			Kind:      trading.EventStepFailed,
			StepIndex: 0,
			Reason:    trading.FailureItemUnavailable,
		})
		if err != nil {
			t.Fatal(err)
		}
		if failed.Status != trading.SagaFailed ||
			failed.Compensation == nil ||
			failed.Compensation.Kind != trading.CompensationNone ||
			failed.ReservedBudget != 0 {
			t.Fatalf("unexpected empty compensation: %+v", failed)
		}
	})

	t.Run("reconciliation mismatch", func(t *testing.T) {
		service, _ := newService(t, 2)
		value := directOpportunity("direct:item", "item", 1, 10, 30, 3)
		if _, err := service.Begin("execution", value, limits()); err != nil {
			t.Fatal(err)
		}
		mustBuy(t, service, "buy", 0, "item", 10, 1)
		if _, err := service.Apply(trading.Event{
			ID:        "list",
			SagaID:    "execution",
			Kind:      trading.EventStepSucceeded,
			StepIndex: 1,
			Outcome: trading.StepOutcome{
				CompletedQuantity: 1,
				Fees:              1,
				MonetaryActionID:  "mismatch-listing-action",
				MarketOrderID:     "order-mismatch",
				InventoryDeltas: []domain.InventoryDelta{{
					ItemID: "item", QuantityDelta: -1, SlotsDelta: -1,
				}},
			},
		}); err != nil {
			t.Fatal(err)
		}
		result, err := service.Apply(trading.Event{
			ID:     "settled",
			SagaID: "execution",
			Kind:   trading.EventSaleSettled,
			Settlement: &domain.TradeResult{
				ExecutionID:    "execution",
				MarketOrderID:  "order-mismatch",
				InputCost:      10,
				Revenue:        29,
				Fees:           3,
				ResultItemID:   "item",
				ResultQuantity: 1,
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		if result.Status != trading.SagaCompletedMismatch ||
			result.Reconciliation == nil ||
			result.Reconciliation.Matched {
			t.Fatalf("reconciliation mismatch was hidden: %+v", result)
		}
	})
}

func newService(
	t *testing.T,
	capacity int,
) (*trading.Service, *inventory.Tracker) {
	return newServiceWithMaxAttempts(t, capacity, 0)
}

func newServiceWithMaxAttempts(
	t *testing.T,
	capacity int,
	maxStepAttempts int,
) (*trading.Service, *inventory.Tracker) {
	t.Helper()
	finder, err := opportunity.NewFinder(opportunity.Config{QuoteTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	tracker, err := inventory.NewTracker(capacity)
	if err != nil {
		t.Fatal(err)
	}
	service, err := trading.NewService(finder, tracker, trading.Config{
		ScoreWeights:    economy.ScoreWeights{Profit: 1},
		ProfitScale:     1,
		MaxStepAttempts: maxStepAttempts,
		Clock:           func() time.Time { return fixedNow },
	})
	if err != nil {
		t.Fatal(err)
	}
	return service, tracker
}

func completedDirectCheckpoint(t *testing.T) trading.Checkpoint {
	t.Helper()
	service, _ := newService(t, 4)
	value := directOpportunity("direct:item", "item", 1, 10, 30, 3)
	if _, err := service.Begin("execution", value, limits()); err != nil {
		t.Fatal(err)
	}
	events := []trading.Event{
		{
			ID: "buy", SagaID: "execution", Kind: trading.EventStepSucceeded,
			StepIndex: 0, OccurredAt: fixedNow.Add(time.Minute),
			Outcome: trading.StepOutcome{
				CompletedQuantity: 1, PurchaseCost: 10, MonetaryActionID: "buy-action",
				InventoryDeltas: []domain.InventoryDelta{{
					ItemID: "item", QuantityDelta: 1, SlotsDelta: 1,
				}},
			},
		},
		{
			ID: "list", SagaID: "execution", Kind: trading.EventStepSucceeded,
			StepIndex: 1, OccurredAt: fixedNow.Add(2 * time.Minute),
			Outcome: trading.StepOutcome{
				CompletedQuantity: 1, Fees: 3, MonetaryActionID: "list-action",
				MarketOrderID: "order",
				InventoryDeltas: []domain.InventoryDelta{{
					ItemID: "item", QuantityDelta: -1, SlotsDelta: -1,
				}},
			},
		},
		{
			ID: "settlement", SagaID: "execution", Kind: trading.EventSaleSettled,
			OccurredAt: fixedNow.Add(3 * time.Minute),
			Settlement: &domain.TradeResult{
				ExecutionID: "execution", MarketOrderID: "order", InputCost: 10,
				Revenue: 30, Fees: 3, ResultItemID: "item", ResultQuantity: 1,
				CompletedAt: fixedNow.Add(3 * time.Minute),
			},
		},
	}
	for _, event := range events {
		if _, err := service.Apply(event); err != nil {
			t.Fatal(err)
		}
	}
	checkpoint, err := service.Checkpoint("execution")
	if err != nil {
		t.Fatal(err)
	}
	if err := trading.ValidateCheckpointStrict(checkpoint, 3); err != nil {
		t.Fatalf("реальная контрольная точка отклонена: %v", err)
	}
	return checkpoint
}

func limits() domain.RiskLimits {
	return domain.RiskLimits{
		MaxBudget:          1_000,
		MaxItemPrice:       1_000,
		MinProfit:          5,
		MinROI:             0,
		MinConfidence:      .5,
		MaxQuoteAge:        time.Hour,
		AvailableSlots:     99,
		AllowUnknownResale: false,
	}
}

func quote(itemID string, purchase, sale int64) domain.TradeQuote {
	return domain.TradeQuote{
		ItemID:          itemID,
		PurchasePrice:   purchase,
		SalePrice:       sale,
		SaleCommission:  1,
		ListingFee:      1,
		ObservedAt:      fixedNow,
		Confidence:      .9,
		LiquidityScore:  .8,
		PriceVolatility: .1,
		ResaleKnown:     true,
	}
}

func directOpportunity(
	id, itemID string,
	quantity, buyPrice, salePrice, fees int64,
) domain.TradeOpportunity {
	inputCost := buyPrice * quantity
	revenue := salePrice * quantity
	return domain.TradeOpportunity{
		ID:              id,
		Kind:            domain.OpportunityDirectFlip,
		InputCost:       inputCost,
		ExpectedRevenue: revenue,
		ExpectedFees:    fees,
		Confidence:      1,
		LiquidityScore:  1,
		RequiredSlots:   1,
		QuoteObservedAt: fixedNow,
		ResaleKnown:     true,
		ResultItemID:    itemID,
		ResultQuantity:  quantity,
		ExpiresAt:       fixedNow.Add(time.Hour),
		Steps: []domain.TradeStep{
			{Kind: domain.TradeStepBuy, ItemID: itemID, Quantity: quantity, LimitPrice: buyPrice},
			{Kind: domain.TradeStepList, ItemID: itemID, Quantity: quantity, LimitPrice: salePrice},
		},
	}
}

func contactOpportunity() domain.TradeOpportunity {
	return domain.TradeOpportunity{
		ID:              "barter:recipe",
		Kind:            domain.OpportunityContactBarter,
		InputCost:       15,
		ExpectedRevenue: 50,
		ExpectedFees:    5,
		Confidence:      1,
		LiquidityScore:  1,
		RequiredSlots:   2,
		QuoteObservedAt: fixedNow,
		ResaleKnown:     true,
		ResultItemID:    "result",
		ResultQuantity:  1,
		ExpiresAt:       fixedNow.Add(time.Hour),
		Steps: []domain.TradeStep{
			{Kind: domain.TradeStepBuy, ItemID: "a", Quantity: 1, LimitPrice: 10},
			{Kind: domain.TradeStepBuy, ItemID: "b", Quantity: 1, LimitPrice: 5},
			{Kind: domain.TradeStepBarter, ItemID: "result", RecipeID: "recipe", Quantity: 1},
			{Kind: domain.TradeStepList, ItemID: "result", Quantity: 1, LimitPrice: 50},
		},
	}
}

func mustBuy(
	t *testing.T,
	service *trading.Service,
	eventID string,
	stepIndex int,
	itemID string,
	cost, quantity int64,
) {
	t.Helper()
	_, err := service.Apply(trading.Event{
		ID:        eventID,
		SagaID:    "execution",
		Kind:      trading.EventStepSucceeded,
		StepIndex: stepIndex,
		Outcome: trading.StepOutcome{
			CompletedQuantity: quantity,
			PurchaseCost:      cost,
			MonetaryActionID:  eventID + "-action",
			InventoryDeltas: []domain.InventoryDelta{{
				ItemID:        itemID,
				QuantityDelta: quantity,
				SlotsDelta:    1,
			}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
}

func mustRetry(
	t *testing.T,
	service *trading.Service,
	eventID string,
	stepIndex int,
) {
	t.Helper()
	if _, err := service.Apply(trading.Event{
		ID:         eventID,
		SagaID:     "execution",
		Kind:       trading.EventRecoveryResolved,
		StepIndex:  stepIndex,
		Resolution: trading.RecoveryRetry,
	}); err != nil {
		t.Fatal(err)
	}
}

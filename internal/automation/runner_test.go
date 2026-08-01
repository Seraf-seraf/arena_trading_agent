package automation

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	appconfig "github.com/arena-trading-agent/arena-trading-agent/internal/config"
	"github.com/arena-trading-agent/arena-trading-agent/internal/domain"
	"github.com/arena-trading-agent/arena-trading-agent/internal/navigation"
	"github.com/arena-trading-agent/arena-trading-agent/internal/protocol"
	"github.com/arena-trading-agent/arena-trading-agent/internal/trading"
)

type routeReply struct {
	target domain.ScreenState
	result navigation.Result
	err    error
}

type scriptedRouter struct {
	t       *testing.T
	replies []routeReply
	calls   []domain.ScreenState
	last    domain.Observation
	mutate  func(domain.Observation) domain.Observation
	commits int
}

func (r *scriptedRouter) Navigate(
	_ context.Context,
	_ string,
	_ string,
	target domain.ScreenState,
	_ map[string]string,
) (navigation.Result, error) {
	r.t.Helper()
	r.calls = append(r.calls, target)
	if len(r.replies) == 0 {
		r.t.Fatalf("unexpected Navigate(%s)", target)
	}
	reply := r.replies[0]
	r.replies = r.replies[1:]
	if reply.target != target {
		r.t.Fatalf("Navigate target=%s, want %s", target, reply.target)
	}
	r.last = reply.result.Observation
	return reply.result, reply.err
}

func (r *scriptedRouter) Commit(
	_ context.Context,
	_ string,
	_ string,
	target domain.ScreenState,
	_ protocol.ActionClass,
	_ map[string]string,
	validate CommitValidator,
) (navigation.Result, error) {
	observation := r.last
	if r.mutate != nil {
		observation = r.mutate(observation)
	}
	if err := validate(observation); err != nil {
		return navigation.Result{}, fmt.Errorf("%w: %v", ErrCommitValidation, err)
	}
	r.commits++
	return r.next(target)
}

func (r *scriptedRouter) next(target domain.ScreenState) (navigation.Result, error) {
	r.t.Helper()
	r.calls = append(r.calls, target)
	if len(r.replies) == 0 {
		r.t.Fatalf("unexpected route(%s)", target)
	}
	reply := r.replies[0]
	r.replies = r.replies[1:]
	if reply.target != target {
		r.t.Fatalf("route target=%s, want %s", target, reply.target)
	}
	return reply.result, reply.err
}

func TestTradeRunnerBuyUsesVerifiedBalanceAndInventoryDeltas(t *testing.T) {
	t.Parallel()
	itemID := "item"
	router := &scriptedRouter{t: t, replies: []routeReply{
		{target: domain.StateItemCard, result: navigation.Result{
			Observation: observationWith(map[string]string{
				valueItemName: "Предмет", valuePurchasePrice: "10",
			}),
		}},
		{target: domain.StatePurchaseDialog, result: navigation.Result{
			Observation: observationWith(map[string]string{
				valueItemName:                "Предмет",
				valuePurchasePrice:           "10",
				valueBalance:                 "100",
				inventoryQuantityKey(itemID): "0",
				inventorySlotsKey(itemID):    "0",
				valueFreeInventorySlots:      "4",
			}),
		}},
		{target: domain.StateConfirmation, result: navigation.Result{
			Observation: observationWith(map[string]string{
				valueBalance:                 "90",
				inventoryQuantityKey(itemID): "1",
				inventorySlotsKey(itemID):    "1",
				valueFreeInventorySlots:      "3",
			}),
			Attempts: []navigation.Attempt{{
				Request: protocol.ActionRequest{
					ID: "purchase-action", Class: protocol.ActionPurchase,
				},
				Result: protocol.ActionResult{
					ID: "purchase-action", Success: true,
				},
			}},
		}},
	}}
	runner := newTestRunner(t, router, nil)
	saga, directive := testDirective(domain.TradeStep{
		Kind: domain.TradeStepBuy, ItemID: itemID, Quantity: 1, LimitPrice: 10,
	})

	event, err := runner.Execute(context.Background(), "agent", "session", saga, directive)
	if err != nil {
		t.Fatal(err)
	}
	if event.Kind != trading.EventStepSucceeded || event.Outcome.PurchaseCost != 10 ||
		event.Outcome.CompletedQuantity != 1 ||
		event.Outcome.MonetaryActionID != "purchase-action" {
		t.Fatalf("unexpected event: %+v", event)
	}
	wantDelta := []domain.InventoryDelta{{
		ItemID: itemID, QuantityDelta: 1, SlotsDelta: 1,
	}}
	if !reflect.DeepEqual(event.Outcome.InventoryDeltas, wantDelta) {
		t.Fatalf("deltas=%+v, want %+v", event.Outcome.InventoryDeltas, wantDelta)
	}
}

func TestTradeRunnerRejectsWrongVisibleItemBeforeBuy(t *testing.T) {
	t.Parallel()

	router := &scriptedRouter{t: t, replies: []routeReply{{
		target: domain.StateItemCard,
		result: navigation.Result{Observation: observationWith(map[string]string{
			valueItemName: "Чужой предмет", valuePurchasePrice: "10",
		})},
	}}}
	runner := newTestRunner(t, router, nil)
	saga, directive := testDirective(domain.TradeStep{
		Kind: domain.TradeStepBuy, ItemID: "item", Quantity: 1, LimitPrice: 10,
	})

	_, err := runner.Execute(
		context.Background(),
		"agent",
		"session",
		saga,
		directive,
	)
	if err == nil || !strings.Contains(err.Error(), "Чужой предмет") &&
		!strings.Contains(err.Error(), "чужой предмет") {
		t.Fatalf("чужой предмет перед покупкой не был отклонён: %v", err)
	}
	if router.commits != 0 {
		t.Fatalf("после несовпадения предмета выполнено денежных commit: %d", router.commits)
	}
}

func TestTradeRunnerRejectsLowConfidenceVisibleItemBeforeBuy(t *testing.T) {
	t.Parallel()

	observation := observationWith(map[string]string{
		valueItemName: "Предмет", valuePurchasePrice: "10",
	})
	identity := observation.Values[valueItemName]
	identity.Confidence = .5
	observation.Values[valueItemName] = identity
	router := &scriptedRouter{t: t, replies: []routeReply{{
		target: domain.StateItemCard,
		result: navigation.Result{Observation: observation},
	}}}
	runner := newTestRunner(t, router, nil)
	saga, directive := testDirective(domain.TradeStep{
		Kind: domain.TradeStepBuy, ItemID: "item", Quantity: 1, LimitPrice: 10,
	})

	_, err := runner.Execute(
		context.Background(),
		"agent",
		"session",
		saga,
		directive,
	)
	if err == nil || !strings.Contains(err.Error(), "недостаточную уверенность") {
		t.Fatalf("ненадёжная идентичность предмета не была отклонена: %v", err)
	}
	if router.commits != 0 {
		t.Fatalf("после ненадёжной идентичности выполнено денежных commit: %d", router.commits)
	}
}

func TestTradeRunnerRejectsMissingVisibleItemBeforeBuy(t *testing.T) {
	t.Parallel()

	router := &scriptedRouter{t: t, replies: []routeReply{{
		target: domain.StateItemCard,
		result: navigation.Result{Observation: observationWith(map[string]string{
			valuePurchasePrice: "10",
		})},
	}}}
	runner := newTestRunner(t, router, nil)
	saga, directive := testDirective(domain.TradeStep{
		Kind: domain.TradeStepBuy, ItemID: "item", Quantity: 1, LimitPrice: 10,
	})

	_, err := runner.Execute(
		context.Background(),
		"agent",
		"session",
		saga,
		directive,
	)
	if err == nil || !strings.Contains(err.Error(), "не содержит обязательную") {
		t.Fatalf("отсутствующая идентичность предмета не была отклонена: %v", err)
	}
	if router.commits != 0 {
		t.Fatalf("без идентичности предмета выполнено денежных commit: %d", router.commits)
	}
}

func TestTradeRunnerRejectsChangedPriceBeforeMoneyAction(t *testing.T) {
	t.Parallel()
	itemID := "item"
	router := &scriptedRouter{t: t, replies: []routeReply{
		{target: domain.StateItemCard, result: navigation.Result{
			Observation: observationWith(map[string]string{
				valueItemName: "Предмет", valuePurchasePrice: "10",
			}),
		}},
		{target: domain.StatePurchaseDialog, result: navigation.Result{
			Observation: observationWith(map[string]string{
				valueItemName: "Предмет", valuePurchasePrice: "11",
			}),
		}},
	}}
	runner := newTestRunner(t, router, nil)
	saga, directive := testDirective(domain.TradeStep{
		Kind: domain.TradeStepBuy, ItemID: itemID, Quantity: 1, LimitPrice: 10,
	})

	event, err := runner.Execute(context.Background(), "agent", "session", saga, directive)
	if err != nil {
		t.Fatal(err)
	}
	if event.Kind != trading.EventStepFailed || event.Reason != trading.FailurePriceChanged {
		t.Fatalf("unexpected event: %+v", event)
	}
	if len(router.calls) != 2 {
		t.Fatalf("money confirmation was unexpectedly attempted: %v", router.calls)
	}
}

func TestTradeRunnerRejectsTOCTOUChangeOnCommitFrame(t *testing.T) {
	t.Parallel()
	itemID := "item"
	router := &scriptedRouter{t: t, replies: []routeReply{
		{target: domain.StateItemCard, result: navigation.Result{
			Observation: observationWith(map[string]string{
				valueItemName: "Предмет", valuePurchasePrice: "10",
			}),
		}},
		{target: domain.StatePurchaseDialog, result: navigation.Result{
			Observation: observationWith(map[string]string{
				valueItemName:                "Предмет",
				valuePurchasePrice:           "10",
				valueBalance:                 "100",
				inventoryQuantityKey(itemID): "0",
				inventorySlotsKey(itemID):    "0",
				valueFreeInventorySlots:      "4",
			}),
		}},
	}}
	router.mutate = func(observation domain.Observation) domain.Observation {
		observation.Values = cloneObservationValues(observation.Values)
		value := observation.Values[valuePurchasePrice]
		value.Raw = "11"
		value.Normalized = "11"
		observation.Values[valuePurchasePrice] = value
		return observation
	}
	runner := newTestRunner(t, router, nil)
	saga, directive := testDirective(domain.TradeStep{
		Kind: domain.TradeStepBuy, ItemID: itemID, Quantity: 1, LimitPrice: 20,
	})

	event, err := runner.Execute(context.Background(), "agent", "session", saga, directive)
	if err != nil {
		t.Fatal(err)
	}
	if event.Kind != trading.EventStepFailed ||
		event.Reason != trading.FailureTransient ||
		router.commits != 0 {
		t.Fatalf("изменённый commit-frame не был безопасно отклонён: event=%+v commits=%d", event, router.commits)
	}
}

func TestTradeRunnerRejectsIdentityChangeOnCommitFrame(t *testing.T) {
	t.Parallel()

	itemID := "item"
	router := &scriptedRouter{t: t, replies: []routeReply{
		{target: domain.StateItemCard, result: navigation.Result{
			Observation: observationWith(map[string]string{
				valueItemName: "Предмет", valuePurchasePrice: "10",
			}),
		}},
		{target: domain.StatePurchaseDialog, result: navigation.Result{
			Observation: observationWith(map[string]string{
				valueItemName:                "Предмет",
				valuePurchasePrice:           "10",
				valueBalance:                 "100",
				inventoryQuantityKey(itemID): "0",
				inventorySlotsKey(itemID):    "0",
				valueFreeInventorySlots:      "4",
			}),
		}},
	}}
	router.mutate = func(observation domain.Observation) domain.Observation {
		observation.Values = cloneObservationValues(observation.Values)
		identity := observation.Values[valueItemName]
		identity.Raw = "Чужой предмет"
		identity.Normalized = "Чужой предмет"
		observation.Values[valueItemName] = identity
		return observation
	}
	runner := newTestRunner(t, router, nil)
	saga, directive := testDirective(domain.TradeStep{
		Kind: domain.TradeStepBuy, ItemID: itemID, Quantity: 1, LimitPrice: 20,
	})

	event, err := runner.Execute(context.Background(), "agent", "session", saga, directive)
	if err != nil {
		t.Fatal(err)
	}
	if event.Kind != trading.EventStepFailed ||
		event.Reason != trading.FailureTransient ||
		router.commits != 0 {
		t.Fatalf(
			"подмена предмета на commit-кадре не заблокирована: event=%+v commits=%d",
			event,
			router.commits,
		)
	}
}

func TestTradeRunnerRejectsVLMSourceOnCommitFrame(t *testing.T) {
	t.Parallel()
	itemID := "item"
	router := &scriptedRouter{t: t, replies: []routeReply{
		{target: domain.StateItemCard, result: navigation.Result{
			Observation: observationWith(map[string]string{
				valueItemName: "Предмет", valuePurchasePrice: "10",
			}),
		}},
		{target: domain.StatePurchaseDialog, result: navigation.Result{
			Observation: observationWith(map[string]string{
				valueItemName:                "Предмет",
				valuePurchasePrice:           "10",
				valueBalance:                 "100",
				inventoryQuantityKey(itemID): "0",
				inventorySlotsKey(itemID):    "0",
				valueFreeInventorySlots:      "4",
			}),
		}},
	}}
	router.mutate = func(observation domain.Observation) domain.Observation {
		observation.Values = cloneObservationValues(observation.Values)
		value := observation.Values[valuePurchasePrice]
		value.Source = "VLM"
		observation.Values[valuePurchasePrice] = value
		return observation
	}
	runner := newTestRunner(t, router, nil)
	saga, directive := testDirective(domain.TradeStep{
		Kind: domain.TradeStepBuy, ItemID: itemID, Quantity: 1, LimitPrice: 20,
	})

	event, err := runner.Execute(context.Background(), "agent", "session", saga, directive)
	if err != nil {
		t.Fatal(err)
	}
	if event.Kind != trading.EventStepFailed ||
		event.Reason != trading.FailureTransient ||
		router.commits != 0 {
		t.Fatalf(
			"VLM-источник commit-кадра не был безопасно отклонён: event=%+v commits=%d",
			event,
			router.commits,
		)
	}
}

func TestTradeRunnerBarterRequiresExactRecipeDeltas(t *testing.T) {
	t.Parallel()
	recipe := appconfig.Recipe{
		ID: "recipe", ContactID: "contact", ContactName: "Контакт",
		ResultItemID: "result", ResultItemName: "Результат", ResultCount: 1,
		Ingredients: []appconfig.RecipeIngredient{{
			ItemID: "a", Name: "A", Quantity: 2,
		}},
	}
	before := map[string]string{
		valueContactName:               "Контакт",
		valueResultItemName:            "Результат",
		valueCooldownSeconds:           "0",
		valueResultQuantity:            "1",
		ingredientNameKey("a"):         "A",
		"ingredient.a.quantity":        "2",
		inventoryQuantityKey("a"):      "2",
		inventorySlotsKey("a"):         "1",
		inventoryQuantityKey("result"): "0",
		inventorySlotsKey("result"):    "0",
		valueFreeInventorySlots:        "3",
	}
	after := map[string]string{
		inventoryQuantityKey("a"):      "0",
		inventorySlotsKey("a"):         "0",
		inventoryQuantityKey("result"): "1",
		inventorySlotsKey("result"):    "1",
		valueFreeInventorySlots:        "3",
	}
	router := &scriptedRouter{t: t, replies: []routeReply{
		{target: domain.StateBarterCard, result: navigation.Result{Observation: observationWith(before)}},
		{target: domain.StateConfirmation, result: navigation.Result{
			Observation: observationWith(after),
			Attempts: []navigation.Attempt{{
				Request: protocol.ActionRequest{
					ID: "barter-action", Class: protocol.ActionBarter,
				},
				Result: protocol.ActionResult{
					ID: "barter-action", Success: true,
				},
			}},
		}},
	}}
	runner := newTestRunner(t, router, []appconfig.Recipe{recipe})
	saga, directive := testDirective(domain.TradeStep{
		Kind: domain.TradeStepBarter, ItemID: "result", RecipeID: "recipe", Quantity: 1,
	})

	event, err := runner.Execute(context.Background(), "agent", "session", saga, directive)
	if err != nil {
		t.Fatal(err)
	}
	if event.Kind != trading.EventStepSucceeded ||
		event.Outcome.MonetaryActionID != "barter-action" ||
		len(event.Outcome.InventoryDeltas) != 2 {
		t.Fatalf("unexpected barter event: %+v", event)
	}
}

func TestTradeRunnerRejectsWrongVisibleBarterIdentity(t *testing.T) {
	t.Parallel()

	recipe := appconfig.Recipe{
		ID: "recipe", ContactID: "contact", ContactName: "Контакт",
		ResultItemID: "result", ResultItemName: "Результат", ResultCount: 1,
		Ingredients: []appconfig.RecipeIngredient{{
			ItemID: "a", Name: "Ингредиент", Quantity: 2,
		}},
	}
	tests := []struct {
		name  string
		key   string
		value string
	}{
		{name: "неверный контакт", key: valueContactName, value: "Другой контакт"},
		{name: "неверный рецепт", key: ingredientNameKey("a"), value: "Другой ингредиент"},
		{name: "неверный результат", key: valueResultItemName, value: "Другой результат"},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			values := map[string]string{
				valueContactName:               recipe.ContactName,
				valueResultItemName:            recipe.ResultItemName,
				ingredientNameKey("a"):         recipe.Ingredients[0].Name,
				valueCooldownSeconds:           "0",
				valueResultQuantity:            "1",
				"ingredient.a.quantity":        "2",
				valueFreeInventorySlots:        "3",
				inventoryQuantityKey("a"):      "2",
				inventorySlotsKey("a"):         "1",
				inventoryQuantityKey("result"): "0",
				inventorySlotsKey("result"):    "0",
			}
			values[test.key] = test.value
			router := &scriptedRouter{t: t, replies: []routeReply{{
				target: domain.StateBarterCard,
				result: navigation.Result{Observation: observationWith(values)},
			}}}
			runner := newTestRunner(t, router, []appconfig.Recipe{recipe})
			saga, directive := testDirective(domain.TradeStep{
				Kind: domain.TradeStepBarter, ItemID: "result",
				RecipeID: recipe.ID, Quantity: 1,
			})

			_, err := runner.Execute(
				context.Background(),
				"agent",
				"session",
				saga,
				directive,
			)
			if err == nil {
				t.Fatalf("%s ошибочно принята", test.name)
			}
			if router.commits != 0 {
				t.Fatalf("%s дошла до денежного commit", test.name)
			}
		})
	}
}

func TestTradeRunnerListingPersistsVerifiedMarketOrderIdentity(t *testing.T) {
	t.Parallel()

	itemID := "item"
	router := &scriptedRouter{t: t, replies: []routeReply{
		{target: domain.StateSaleDialog, result: navigation.Result{
			Observation: observationWith(map[string]string{
				valueItemName:                "Предмет",
				valueSalePrice:               "30",
				valueListingFee:              "2",
				valueBalance:                 "100",
				inventoryQuantityKey(itemID): "1",
				inventorySlotsKey(itemID):    "1",
				valueFreeMarketSlots:         "3",
			}),
		}},
		{target: domain.StateConfirmation, result: navigation.Result{
			Observation: observationWith(map[string]string{
				valueBalance:                 "98",
				inventoryQuantityKey(itemID): "0",
				inventorySlotsKey(itemID):    "0",
				valueFreeMarketSlots:         "2",
				valueMarketOrderID:           "order-42",
			}),
			Attempts: []navigation.Attempt{{
				Request: protocol.ActionRequest{
					ID: "listing-action", Class: protocol.ActionListing,
				},
				Result: protocol.ActionResult{
					ID: "listing-action", Success: true,
				},
			}},
		}},
	}}
	runner := newTestRunner(t, router, nil)
	saga, directive := testDirective(domain.TradeStep{
		Kind: domain.TradeStepList, ItemID: itemID, Quantity: 1, LimitPrice: 30,
	})

	event, err := runner.Execute(context.Background(), "agent", "session", saga, directive)
	if err != nil {
		t.Fatal(err)
	}
	if event.Kind != trading.EventStepSucceeded ||
		event.Outcome.MonetaryActionID != "listing-action" ||
		event.Outcome.MarketOrderID != "order-42" ||
		event.Outcome.Fees != 2 {
		t.Fatalf("выставление не сохранило идентичность ордера: %+v", event)
	}
}

func TestTradeRunnerRejectsWrongVisibleItemBeforeListing(t *testing.T) {
	t.Parallel()

	router := &scriptedRouter{t: t, replies: []routeReply{{
		target: domain.StateSaleDialog,
		result: navigation.Result{Observation: observationWith(map[string]string{
			valueItemName: "Чужой предмет",
		})},
	}}}
	runner := newTestRunner(t, router, nil)
	saga, directive := testDirective(domain.TradeStep{
		Kind: domain.TradeStepList, ItemID: "item", Quantity: 1, LimitPrice: 30,
	})

	_, err := runner.Execute(
		context.Background(),
		"agent",
		"session",
		saga,
		directive,
	)
	if err == nil || router.commits != 0 {
		t.Fatalf("чужой предмет перед выставлением не заблокирован: err=%v commits=%d", err, router.commits)
	}
}

func TestTradeRunnerSaleMonitorRejectsDifferentMarketOrder(t *testing.T) {
	t.Parallel()

	router := &scriptedRouter{t: t, replies: []routeReply{{
		target: domain.StateMarketHome,
		result: navigation.Result{Observation: observationWith(map[string]string{
			valueMarketOrderID: "order-other",
			valueOrderStatus:   "SOLD",
		})},
	}}}
	runner := newTestRunner(t, router, nil)
	saga := trading.Saga{
		ID:            "execution",
		Status:        trading.SagaAwaitingSale,
		MarketOrderID: "order-expected",
		Opportunity: domain.TradeOpportunity{
			ResultItemID: "item",
		},
	}
	directive := trading.Directive{
		Kind:           trading.DirectiveMonitorSale,
		SagaID:         saga.ID,
		IdempotencyKey: "execution:monitor",
	}

	_, err := runner.Execute(context.Background(), "agent", "session", saga, directive)
	if err == nil || !strings.Contains(err.Error(), "order-other") {
		t.Fatalf("другой рыночный ордер не был отклонён: %v", err)
	}
}

func TestTradeRunnerSaleMonitorReturnsActiveOrderSnapshot(t *testing.T) {
	t.Parallel()

	observation := testOrderObservation(orderStatusActive, "30", "25")
	router := &scriptedRouter{t: t, replies: []routeReply{{
		target: domain.StateMarketHome,
		result: navigation.Result{Observation: observation},
	}}}
	runner := newTestRunner(t, router, nil)
	saga, directive := saleMonitorSagaDirective()

	event, err := runner.Execute(
		context.Background(),
		"agent",
		"session",
		saga,
		directive,
	)
	if !errors.Is(err, ErrSalePending) || event.Kind != "" {
		t.Fatalf("активный ордер вернул event=%+v err=%v", event, err)
	}
	snapshot, ok := runner.takeOrderSnapshot(saga.ID)
	if !ok || snapshot.Status != orderStatusActive ||
		!snapshot.SlotOccupied ||
		!snapshot.RepriceRecommended ||
		snapshot.RecommendedPrice != 25 {
		t.Fatalf("неверный снимок активного ордера: %+v exists=%v", snapshot, ok)
	}
}

func TestTradeRunnerSaleMonitorReturnsSoldOrderSnapshotAndSettlement(t *testing.T) {
	t.Parallel()

	observation := testOrderObservation(orderStatusSold, "30", "25")
	observation.Values[valueSettledRevenue] = testOCRValue("30")
	observation.Values[valueSettledFees] = testOCRValue("3")
	observation.Values[valueSoldQuantity] = testOCRValue("1")
	observation.Values[valueSoldItemID] = testOCRValue("item")
	router := &scriptedRouter{t: t, replies: []routeReply{{
		target: domain.StateMarketHome,
		result: navigation.Result{Observation: observation},
	}}}
	runner := newTestRunner(t, router, nil)
	saga, directive := saleMonitorSagaDirective()
	saga.Actual.InputCost = 10

	event, err := runner.Execute(
		context.Background(),
		"agent",
		"session",
		saga,
		directive,
	)
	if err != nil || event.Kind != trading.EventSaleSettled ||
		event.Settlement == nil ||
		event.Settlement.MarketOrderID != saga.MarketOrderID {
		t.Fatalf("проданный ордер вернул event=%+v err=%v", event, err)
	}
	snapshot, ok := runner.takeOrderSnapshot(saga.ID)
	if !ok || snapshot.Status != orderStatusSold ||
		snapshot.SlotOccupied ||
		snapshot.RepriceRecommended ||
		snapshot.AutomaticRepriceAllowed {
		t.Fatalf("неверный снимок проданного ордера: %+v exists=%v", snapshot, ok)
	}
}

func TestRequireMonetaryTransitionRequiresOneSuccessfulActionIdentity(t *testing.T) {
	t.Parallel()

	attempt := navigation.Attempt{
		Request: protocol.ActionRequest{
			ID: "purchase-action", Class: protocol.ActionPurchase,
		},
		Result: protocol.ActionResult{
			ID: "purchase-action", Success: true,
		},
	}
	actionID, err := requireMonetaryTransition(
		navigation.Result{Attempts: []navigation.Attempt{attempt}},
		protocol.ActionPurchase,
	)
	if err != nil || actionID != "purchase-action" {
		t.Fatalf("корректная денежная попытка отклонена: id=%q err=%v", actionID, err)
	}

	if _, err := requireMonetaryTransition(
		navigation.Result{Attempts: []navigation.Attempt{attempt, attempt}},
		protocol.ActionPurchase,
	); err == nil {
		t.Fatal("две денежные попытки ошибочно приняты как один результат")
	}

	conflicting := attempt
	conflicting.Result.ID = "другой-action"
	if _, err := requireMonetaryTransition(
		navigation.Result{Attempts: []navigation.Attempt{conflicting}},
		protocol.ActionPurchase,
	); err == nil {
		t.Fatal("несогласованные request/result ID ошибочно приняты")
	}
}

func newTestRunner(
	t *testing.T,
	router TradeRouteNavigator,
	recipes []appconfig.Recipe,
) *TradeRunner {
	t.Helper()
	runner, err := NewTradeRunner(router, []appconfig.WatchItem{{
		ID: "item", Name: "Предмет", Enabled: true,
	}}, recipes, .9)
	if err != nil {
		t.Fatal(err)
	}
	runner.now = func() time.Time {
		return time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	}
	return runner
}

func testDirective(step domain.TradeStep) (trading.Saga, trading.Directive) {
	saga := trading.Saga{
		ID: "execution", Status: trading.SagaRunning, CurrentStep: 0,
		Opportunity: domain.TradeOpportunity{Steps: []domain.TradeStep{step}},
	}
	directive := trading.Directive{
		Kind: trading.DirectiveExecuteStep, SagaID: saga.ID,
		IdempotencyKey: "execution:0:0", StepIndex: 0, Step: &step,
	}
	return saga, directive
}

func saleMonitorSagaDirective() (trading.Saga, trading.Directive) {
	saga := testOrderSaga()
	directive := trading.Directive{
		Kind: trading.DirectiveMonitorSale, SagaID: saga.ID,
		IdempotencyKey: "execution:monitor",
	}
	return saga, directive
}

func observationWith(values map[string]string) domain.Observation {
	result := domain.Observation{
		State: domain.StateConfirmation, Confidence: .99, FrameID: 1,
		CreatedAt: time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC),
		Values:    make(map[string]domain.Value, len(values)),
	}
	for key, value := range values {
		result.Values[key] = domain.Value{
			Raw: value, Normalized: value, Source: "OCR", Confidence: .99,
		}
	}
	return result
}

func cloneObservationValues(values map[string]domain.Value) map[string]domain.Value {
	result := make(map[string]domain.Value, len(values))
	for key, value := range values {
		result[key] = value
	}
	return result
}

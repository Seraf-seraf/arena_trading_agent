package automation

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	appconfig "github.com/arena-trading-agent/arena-trading-agent/internal/config"
	"github.com/arena-trading-agent/arena-trading-agent/internal/domain"
	"github.com/arena-trading-agent/arena-trading-agent/internal/money"
	"github.com/arena-trading-agent/arena-trading-agent/internal/navigation"
	"github.com/arena-trading-agent/arena-trading-agent/internal/protocol"
	"github.com/arena-trading-agent/arena-trading-agent/internal/trading"
)

var (
	// ErrSalePending is a normal monitor outcome: an order is still active.
	ErrSalePending = errors.New("продажа ещё не завершена")
	// ErrRecoveryPending is a normal recovery outcome: cooldown or capacity
	// has not become available yet.
	ErrRecoveryPending = errors.New("условие восстановления ещё не выполнено")
)

const (
	valueSoldItemID = "sold_item_id"
)

// DirectiveRunner is the engine-facing execution contract.
type DirectiveRunner interface {
	Execute(
		context.Context,
		string,
		string,
		trading.Saga,
		trading.Directive,
	) (trading.Event, error)
}

// TradeRunner translates declarative saga directives into verified UI routes.
// It never calls an InputDriver and cannot bypass navigation.Executor.
type TradeRunner struct {
	router           TradeRouteNavigator
	recipes          map[string]appconfig.Recipe
	itemNames        map[string]string
	minConfidence    float64
	now              func() time.Time
	orderSnapshotsMu sync.Mutex
	orderSnapshots   map[string]OrderSnapshot
}

func NewTradeRunner(
	router TradeRouteNavigator,
	watchlist []appconfig.WatchItem,
	recipes []appconfig.Recipe,
	minConfidence float64,
) (*TradeRunner, error) {
	const methodCtx = "automation.NewTradeRunner"

	if router == nil {
		return nil, fmt.Errorf("%s: исполнителю сделок требуется маршрутизатор", methodCtx)
	}
	if !finiteConfidence(minConfidence) || minConfidence <= 0 {
		return nil, fmt.Errorf("%s: минимальная уверенность должна быть в диапазоне (0, 1]", methodCtx)
	}
	result := &TradeRunner{
		router:         router,
		recipes:        make(map[string]appconfig.Recipe, len(recipes)),
		itemNames:      make(map[string]string),
		minConfidence:  minConfidence,
		now:            func() time.Time { return time.Now().UTC() },
		orderSnapshots: make(map[string]OrderSnapshot),
	}
	for _, item := range watchlist {
		if err := registerItemName(result.itemNames, item.ID, item.Name); err != nil {
			return nil, fmt.Errorf(
				"%s: некорректная идентичность предмета списка наблюдения: %w",
				methodCtx,
				err,
			)
		}
	}
	for _, recipe := range recipes {
		if _, duplicate := result.recipes[recipe.ID]; duplicate {
			return nil, fmt.Errorf("%s: рецепт %q продублирован", methodCtx, recipe.ID)
		}
		result.recipes[recipe.ID] = recipe
		if err := registerItemName(
			result.itemNames,
			recipe.ResultItemID,
			recipe.ResultItemName,
		); err != nil {
			return nil, fmt.Errorf(
				"%s: некорректная идентичность результата рецепта %q: %w",
				methodCtx,
				recipe.ID,
				err,
			)
		}
		for _, ingredient := range recipe.Ingredients {
			if err := registerItemName(
				result.itemNames,
				ingredient.ItemID,
				ingredient.Name,
			); err != nil {
				return nil, fmt.Errorf(
					"%s: некорректная идентичность ингредиента рецепта %q: %w",
					methodCtx,
					recipe.ID,
					err,
				)
			}
		}
	}
	return result, nil
}

func (r *TradeRunner) Execute(
	ctx context.Context,
	agentID string,
	sessionID string,
	saga trading.Saga,
	directive trading.Directive,
) (trading.Event, error) {
	const methodCtx = "automation.TradeRunner.Execute"

	if ctx == nil {
		return trading.Event{}, fmt.Errorf("%s: контекст не задан", methodCtx)
	}
	if directive.SagaID == "" || directive.SagaID != saga.ID ||
		strings.TrimSpace(directive.IdempotencyKey) == "" {
		return trading.Event{}, fmt.Errorf("%s: директива не соответствует саге", methodCtx)
	}
	switch directive.Kind {
	case trading.DirectiveExecuteStep:
		if directive.Step == nil || directive.StepIndex != saga.CurrentStep {
			return trading.Event{}, fmt.Errorf("%s: некорректная директива шага", methodCtx)
		}
		switch directive.Step.Kind {
		case domain.TradeStepBuy:
			return r.executeBuy(ctx, agentID, sessionID, saga, directive)
		case domain.TradeStepBarter:
			return r.executeBarter(ctx, agentID, sessionID, saga, directive)
		case domain.TradeStepList:
			return r.executeList(ctx, agentID, sessionID, saga, directive)
		default:
			return trading.Event{}, fmt.Errorf("%s: неизвестный тип шага %q", methodCtx, directive.Step.Kind)
		}
	case trading.DirectiveRecover:
		return r.executeRecovery(ctx, agentID, sessionID, saga, directive)
	case trading.DirectiveMonitorSale:
		return r.monitorSale(ctx, agentID, sessionID, saga, directive)
	case trading.DirectiveDone:
		return trading.Event{}, fmt.Errorf("%s: завершающая директива не исполняется", methodCtx)
	default:
		return trading.Event{}, fmt.Errorf("%s: неизвестная директива %q", methodCtx, directive.Kind)
	}
}

func (r *TradeRunner) executeBuy(
	ctx context.Context,
	agentID string,
	sessionID string,
	saga trading.Saga,
	directive trading.Directive,
) (trading.Event, error) {
	const methodCtx = "automation.TradeRunner.executeBuy"

	step := *directive.Step
	itemName, err := r.configuredItemName(step.ItemID)
	if err != nil {
		return trading.Event{}, fmt.Errorf(
			"%s: покупка заблокирована без настроенной идентичности предмета: %w",
			methodCtx,
			err,
		)
	}
	variables := r.itemVariables(step)
	card, err := r.router.Navigate(ctx, agentID, sessionID, domain.StateItemCard, variables)
	if err != nil {
		return trading.Event{}, fmt.Errorf("%s: не удалось открыть карточку предмета %s: %w", methodCtx, step.ItemID, err)
	}
	cardIdentityConfidence, err := r.requireVisibleIdentity(
		card.Observation,
		valueItemName,
		itemName,
	)
	if err != nil {
		return trading.Event{}, fmt.Errorf(
			"%s: карточка покупки не подтверждает предмет %q: %w",
			methodCtx,
			step.ItemID,
			err,
		)
	}
	cardPrice, cardConfidence, err := requiredInt(
		card.Observation,
		valuePurchasePrice,
		1,
		maximumObservedMoney,
	)
	if err != nil {
		return trading.Event{}, fmt.Errorf("%s: не удалось прочитать цену карточки: %w", methodCtx, err)
	}
	if cardPrice > step.LimitPrice {
		return r.failedEvent(directive, trading.FailurePriceChanged, "цена карточки превышает лимит"), nil
	}
	dialog, err := r.router.Navigate(ctx, agentID, sessionID, domain.StatePurchaseDialog, variables)
	if err != nil {
		return trading.Event{}, fmt.Errorf("%s: не удалось открыть подтверждение покупки %s: %w", methodCtx, step.ItemID, err)
	}
	dialogIdentityConfidence, err := r.requireVisibleIdentity(
		dialog.Observation,
		valueItemName,
		itemName,
	)
	if err != nil {
		return trading.Event{}, fmt.Errorf(
			"%s: подтверждение покупки не относится к предмету %q: %w",
			methodCtx,
			step.ItemID,
			err,
		)
	}
	price, priceConfidence, err := requiredInt(
		dialog.Observation,
		valuePurchasePrice,
		1,
		maximumObservedMoney,
	)
	if err != nil {
		return trading.Event{}, fmt.Errorf("%s: не удалось прочитать цену подтверждения: %w", methodCtx, err)
	}
	if price != cardPrice || price > step.LimitPrice {
		return r.failedEvent(directive, trading.FailurePriceChanged, "цена изменилась перед покупкой"), nil
	}
	balance, balanceConfidence, err := requiredInt(
		dialog.Observation,
		valueBalance,
		0,
		maximumObservedMoney,
	)
	if err != nil {
		return trading.Event{}, fmt.Errorf("%s: не удалось прочитать баланс до покупки: %w", methodCtx, err)
	}
	before, err := readObservedItem(dialog.Observation, step.ItemID)
	if err != nil {
		return trading.Event{}, fmt.Errorf("%s: не удалось прочитать предмет до покупки: %w", methodCtx, err)
	}
	freeBefore, freeConfidence, err := requiredInt(
		dialog.Observation,
		valueFreeInventorySlots,
		0,
		1_000_000,
	)
	if err != nil {
		return trading.Event{}, fmt.Errorf("%s: не удалось прочитать свободные слоты до покупки: %w", methodCtx, err)
	}
	if err := r.requireConfidence(
		card.Observation.Confidence,
		dialog.Observation.Confidence,
		cardIdentityConfidence,
		dialogIdentityConfidence,
		cardConfidence,
		priceConfidence,
		balanceConfidence,
		before.Confidence,
		freeConfidence,
	); err != nil {
		return trading.Event{}, fmt.Errorf("%s: значения до покупки имеют недостаточную уверенность: %w", methodCtx, err)
	}
	expectedCost, err := money.Multiply(price, step.Quantity)
	if err != nil {
		return trading.Event{}, fmt.Errorf("%s: не удалось рассчитать ожидаемую стоимость: %w", methodCtx, err)
	}
	if balance < expectedCost {
		return r.failedEvent(directive, trading.FailureInsufficientFunds, "баланс ниже подтверждённой стоимости"), nil
	}

	confirmed, err := r.router.Commit(
		ctx,
		agentID,
		sessionID,
		domain.StateConfirmation,
		protocol.ActionPurchase,
		variables,
		r.criticalValuesUnchanged(dialog.Observation, []string{
			valueItemName,
			valuePurchasePrice,
			valueBalance,
			inventoryQuantityKey(step.ItemID),
			inventorySlotsKey(step.ItemID),
			valueFreeInventorySlots,
		}),
	)
	if err != nil {
		if errors.Is(err, ErrCommitValidation) {
			return r.failedEvent(
				directive,
				trading.FailureTransient,
				"критичные значения покупки изменились до подтверждения",
			), nil
		}
		return trading.Event{}, fmt.Errorf("%s: денежный переход покупки %s неоднозначен: %w", methodCtx, step.ItemID, err)
	}
	monetaryActionID, err := requireMonetaryTransition(
		confirmed,
		protocol.ActionPurchase,
	)
	if err != nil {
		return trading.Event{}, fmt.Errorf("%s: переход покупки не подтверждён: %w", methodCtx, err)
	}
	afterBalance, afterBalanceConfidence, err := requiredInt(
		confirmed.Observation,
		valueBalance,
		0,
		maximumObservedMoney,
	)
	if err != nil {
		return trading.Event{}, fmt.Errorf("%s: не удалось прочитать баланс после покупки: %w", methodCtx, err)
	}
	after, err := readObservedItem(confirmed.Observation, step.ItemID)
	if err != nil {
		return trading.Event{}, fmt.Errorf("%s: не удалось прочитать предмет после покупки: %w", methodCtx, err)
	}
	freeAfter, afterFreeConfidence, err := requiredInt(
		confirmed.Observation,
		valueFreeInventorySlots,
		0,
		1_000_000,
	)
	if err != nil {
		return trading.Event{}, fmt.Errorf("%s: не удалось прочитать свободные слоты после покупки: %w", methodCtx, err)
	}
	if err := r.requireConfidence(
		confirmed.Observation.Confidence,
		afterBalanceConfidence,
		after.Confidence,
		afterFreeConfidence,
	); err != nil {
		return trading.Event{}, fmt.Errorf("%s: значения после покупки имеют недостаточную уверенность: %w", methodCtx, err)
	}
	delta, err := inventoryDelta(step.ItemID, before, after)
	if err != nil {
		return trading.Event{}, fmt.Errorf("%s: не удалось вычислить изменение инвентаря: %w", methodCtx, err)
	}
	actualCost, err := checkedSubtract(balance, afterBalance)
	if err != nil {
		return trading.Event{}, fmt.Errorf(
			"%s: не удалось вычислить фактическую стоимость покупки %s: %w",
			methodCtx,
			step.ItemID,
			err,
		)
	}
	if delta.QuantityDelta < 0 || delta.QuantityDelta > step.Quantity ||
		int64(delta.SlotsDelta) != freeBefore-freeAfter {
		return trading.Event{}, fmt.Errorf(
			"%s: для покупки %s получено некорректное изменение инвентаря %+v, свободные слоты %d→%d",
			methodCtx,
			step.ItemID,
			delta,
			freeBefore,
			freeAfter,
		)
	}
	completedCost, err := money.Multiply(price, delta.QuantityDelta)
	if err != nil {
		return trading.Event{}, fmt.Errorf("%s: не удалось рассчитать стоимость полученного количества: %w", methodCtx, err)
	}
	if actualCost != completedCost {
		return trading.Event{}, fmt.Errorf(
			"%s: для покупки %s изменение баланса равно %d, стоимость подтверждённого количества %d",
			methodCtx,
			step.ItemID,
			actualCost,
			completedCost,
		)
	}
	if delta.QuantityDelta < step.Quantity {
		event := r.failedEvent(
			directive,
			trading.FailureItemUnavailable,
			fmt.Sprintf(
				"подтверждена частичная покупка %d из %d",
				delta.QuantityDelta,
				step.Quantity,
			),
		)
		event.Outcome.MonetaryActionID = monetaryActionID
		if delta.QuantityDelta > 0 {
			event.Outcome = trading.StepOutcome{
				CompletedQuantity: delta.QuantityDelta,
				PurchaseCost:      actualCost,
				MonetaryActionID:  monetaryActionID,
				InventoryDeltas:   []domain.InventoryDelta{delta},
			}
		}
		return event, nil
	}
	if actualCost != expectedCost {
		return trading.Event{}, fmt.Errorf(
			"%s: для полной покупки %s изменение баланса равно %d, ожидалось %d",
			methodCtx,
			step.ItemID,
			actualCost,
			expectedCost,
		)
	}
	return r.successEvent(directive, trading.StepOutcome{
		CompletedQuantity: step.Quantity,
		PurchaseCost:      actualCost,
		MonetaryActionID:  monetaryActionID,
		InventoryDeltas:   []domain.InventoryDelta{delta},
	}), nil
}

func (r *TradeRunner) executeBarter(
	ctx context.Context,
	agentID string,
	sessionID string,
	saga trading.Saga,
	directive trading.Directive,
) (trading.Event, error) {
	const methodCtx = "automation.TradeRunner.executeBarter"

	step := *directive.Step
	recipe, exists := r.recipes[step.RecipeID]
	if !exists {
		return trading.Event{}, fmt.Errorf("%s: рецепт обмена %q не настроен", methodCtx, step.RecipeID)
	}
	if step.ItemID != recipe.ResultItemID {
		return trading.Event{}, fmt.Errorf(
			"%s: результат шага %q не соответствует результату рецепта %q",
			methodCtx,
			step.ItemID,
			recipe.ResultItemID,
		)
	}
	variables, err := r.recipeVariables(recipe, step.Quantity)
	if err != nil {
		return trading.Event{}, fmt.Errorf("%s: не удалось подготовить параметры рецепта: %w", methodCtx, err)
	}
	card, err := r.router.Navigate(ctx, agentID, sessionID, domain.StateBarterCard, variables)
	if err != nil {
		return trading.Event{}, fmt.Errorf("%s: не удалось открыть рецепт %s: %w", methodCtx, recipe.ID, err)
	}
	identityConfidence, err := r.requireRecipeIdentity(card.Observation, recipe)
	if err != nil {
		return trading.Event{}, fmt.Errorf(
			"%s: видимая идентичность рецепта %q не подтверждена: %w",
			methodCtx,
			recipe.ID,
			err,
		)
	}
	cooldown, cooldownConfidence, err := requiredInt(
		card.Observation,
		valueCooldownSeconds,
		0,
		int64((30*24*time.Hour)/time.Second),
	)
	if err != nil {
		return trading.Event{}, fmt.Errorf("%s: не удалось прочитать время ожидания обмена: %w", methodCtx, err)
	}
	if cooldown > 0 {
		return r.failedEvent(directive, trading.FailureBarterCooldown, "обмен находится в периоде ожидания"), nil
	}
	resultCount, resultConfidence, err := requiredInt(
		card.Observation,
		valueResultQuantity,
		1,
		1_000_000,
	)
	if err != nil {
		return trading.Event{}, fmt.Errorf("%s: не удалось прочитать количество результата: %w", methodCtx, err)
	}
	if resultCount != recipe.ResultCount {
		return trading.Event{}, fmt.Errorf(
			"%s: для обмена %s количество результата равно %d, настроено %d",
			methodCtx,
			recipe.ID,
			resultCount,
			recipe.ResultCount,
		)
	}
	ingredientConfidence := 1.0
	for _, ingredient := range recipe.Ingredients {
		key := "ingredient." + ingredient.ItemID + ".quantity"
		observed, confidence, err := requiredInt(
			card.Observation,
			key,
			1,
			1_000_000_000,
		)
		if err != nil {
			return trading.Event{}, fmt.Errorf(
				"%s: не удалось прочитать требование ингредиента %q: %w",
				methodCtx,
				ingredient.ItemID,
				err,
			)
		}
		expected, err := money.Multiply(ingredient.Quantity, step.Quantity)
		if err != nil {
			return trading.Event{}, fmt.Errorf(
				"%s: не удалось рассчитать требование ингредиента %q: %w",
				methodCtx,
				ingredient.ItemID,
				err,
			)
		}
		if observed != expected {
			return trading.Event{}, fmt.Errorf(
				"%s: рецепт %s требует %d предмета %s, ожидалось %d",
				methodCtx,
				recipe.ID,
				observed,
				ingredient.ItemID,
				expected,
			)
		}
		ingredientConfidence = min(ingredientConfidence, confidence)
	}
	ids := recipeItemIDs(recipe)
	before, confidence, err := readItems(card.Observation, ids)
	if err != nil {
		return trading.Event{}, fmt.Errorf("%s: не удалось прочитать предметы до обмена: %w", methodCtx, err)
	}
	freeBefore, freeConfidence, err := requiredInt(
		card.Observation,
		valueFreeInventorySlots,
		0,
		1_000_000,
	)
	if err != nil {
		return trading.Event{}, fmt.Errorf("%s: не удалось прочитать свободные слоты до обмена: %w", methodCtx, err)
	}
	if err := r.requireConfidence(
		card.Observation.Confidence,
		cooldownConfidence,
		resultConfidence,
		ingredientConfidence,
		identityConfidence,
		confidence,
		freeConfidence,
	); err != nil {
		return trading.Event{}, fmt.Errorf("%s: значения до обмена имеют недостаточную уверенность: %w", methodCtx, err)
	}
	criticalKeys := []string{
		valueContactName,
		valueResultItemName,
		valueCooldownSeconds,
		valueResultQuantity,
		valueFreeInventorySlots,
	}
	for _, itemID := range ids {
		criticalKeys = append(
			criticalKeys,
			inventoryQuantityKey(itemID),
			inventorySlotsKey(itemID),
		)
	}
	for _, ingredient := range recipe.Ingredients {
		criticalKeys = append(
			criticalKeys,
			ingredientNameKey(ingredient.ItemID),
			"ingredient."+ingredient.ItemID+".quantity",
		)
	}
	confirmed, err := r.router.Commit(
		ctx,
		agentID,
		sessionID,
		domain.StateConfirmation,
		protocol.ActionBarter,
		variables,
		r.criticalValuesUnchanged(card.Observation, criticalKeys),
	)
	if err != nil {
		if errors.Is(err, ErrCommitValidation) {
			return r.failedEvent(
				directive,
				trading.FailureTransient,
				"критичные значения обмена изменились до подтверждения",
			), nil
		}
		return trading.Event{}, fmt.Errorf("%s: переход обмена %s неоднозначен: %w", methodCtx, recipe.ID, err)
	}
	monetaryActionID, err := requireMonetaryTransition(
		confirmed,
		protocol.ActionBarter,
	)
	if err != nil {
		return trading.Event{}, fmt.Errorf("%s: переход обмена не подтверждён: %w", methodCtx, err)
	}
	after, afterConfidence, err := readItems(confirmed.Observation, ids)
	if err != nil {
		return trading.Event{}, fmt.Errorf("%s: не удалось прочитать предметы после обмена: %w", methodCtx, err)
	}
	freeAfter, afterFreeConfidence, err := requiredInt(
		confirmed.Observation,
		valueFreeInventorySlots,
		0,
		1_000_000,
	)
	if err != nil {
		return trading.Event{}, fmt.Errorf("%s: не удалось прочитать свободные слоты после обмена: %w", methodCtx, err)
	}
	if err := r.requireConfidence(
		confirmed.Observation.Confidence,
		afterConfidence,
		afterFreeConfidence,
	); err != nil {
		return trading.Event{}, fmt.Errorf("%s: значения после обмена имеют недостаточную уверенность: %w", methodCtx, err)
	}
	deltas := make([]domain.InventoryDelta, 0, len(ids))
	slotDelta := 0
	for _, itemID := range ids {
		delta, err := inventoryDelta(itemID, before[itemID], after[itemID])
		if err != nil {
			return trading.Event{}, fmt.Errorf("%s: не удалось вычислить изменение предмета %q: %w", methodCtx, itemID, err)
		}
		if delta.QuantityDelta != 0 || delta.SlotsDelta != 0 {
			deltas = append(deltas, delta)
			slotDelta += delta.SlotsDelta
		}
	}
	for _, ingredient := range recipe.Ingredients {
		expected, err := money.Multiply(ingredient.Quantity, step.Quantity)
		if err != nil {
			return trading.Event{}, fmt.Errorf("%s: не удалось рассчитать расход ингредиента %q: %w", methodCtx, ingredient.ItemID, err)
		}
		if deltaQuantity(deltas, ingredient.ItemID) != -expected {
			return trading.Event{}, fmt.Errorf(
				"%s: для обмена %s расход предмета %s не подтверждён",
				methodCtx,
				recipe.ID,
				ingredient.ItemID,
			)
		}
	}
	expectedResult, err := money.Multiply(recipe.ResultCount, step.Quantity)
	if err != nil {
		return trading.Event{}, fmt.Errorf("%s: не удалось рассчитать количество результата: %w", methodCtx, err)
	}
	if deltaQuantity(deltas, recipe.ResultItemID) != expectedResult ||
		int64(slotDelta) != freeBefore-freeAfter {
		return trading.Event{}, fmt.Errorf(
			"%s: для обмена %s результат или слоты не подтверждены",
			methodCtx,
			recipe.ID,
		)
	}
	return r.successEvent(directive, trading.StepOutcome{
		CompletedQuantity: step.Quantity,
		MonetaryActionID:  monetaryActionID,
		InventoryDeltas:   deltas,
	}), nil
}

func (r *TradeRunner) executeList(
	ctx context.Context,
	agentID string,
	sessionID string,
	saga trading.Saga,
	directive trading.Directive,
) (trading.Event, error) {
	const methodCtx = "automation.TradeRunner.executeList"

	step := *directive.Step
	itemName, err := r.configuredItemName(step.ItemID)
	if err != nil {
		return trading.Event{}, fmt.Errorf(
			"%s: выставление заблокировано без настроенной идентичности предмета: %w",
			methodCtx,
			err,
		)
	}
	variables := r.itemVariables(step)
	dialog, err := r.router.Navigate(ctx, agentID, sessionID, domain.StateSaleDialog, variables)
	if err != nil {
		return trading.Event{}, fmt.Errorf("%s: не удалось открыть форму продажи предмета %s: %w", methodCtx, step.ItemID, err)
	}
	identityConfidence, err := r.requireVisibleIdentity(
		dialog.Observation,
		valueItemName,
		itemName,
	)
	if err != nil {
		return trading.Event{}, fmt.Errorf(
			"%s: форма продажи не подтверждает предмет %q: %w",
			methodCtx,
			step.ItemID,
			err,
		)
	}
	price, priceConfidence, err := requiredInt(
		dialog.Observation,
		valueSalePrice,
		1,
		maximumObservedMoney,
	)
	if err != nil {
		return trading.Event{}, fmt.Errorf("%s: не удалось прочитать цену продажи: %w", methodCtx, err)
	}
	if price < step.LimitPrice {
		return r.failedEvent(directive, trading.FailurePriceChanged, "цена продажи ниже лимита"), nil
	}
	listingFee, feeConfidence, err := requiredInt(
		dialog.Observation,
		valueListingFee,
		0,
		maximumObservedMoney,
	)
	if err != nil {
		return trading.Event{}, fmt.Errorf("%s: не удалось прочитать плату за выставление: %w", methodCtx, err)
	}
	balance, balanceConfidence, err := requiredInt(
		dialog.Observation,
		valueBalance,
		0,
		maximumObservedMoney,
	)
	if err != nil {
		return trading.Event{}, fmt.Errorf("%s: не удалось прочитать баланс до выставления: %w", methodCtx, err)
	}
	before, err := readObservedItem(dialog.Observation, step.ItemID)
	if err != nil {
		return trading.Event{}, fmt.Errorf("%s: не удалось прочитать предмет до выставления: %w", methodCtx, err)
	}
	freeBefore, slotsConfidence, err := requiredInt(
		dialog.Observation,
		valueFreeMarketSlots,
		0,
		1_000_000,
	)
	if err != nil {
		return trading.Event{}, fmt.Errorf("%s: не удалось прочитать рыночные слоты до выставления: %w", methodCtx, err)
	}
	if freeBefore == 0 {
		return r.failedEvent(
			directive,
			trading.FailureMarketSlotUnavailable,
			"нет свободного рыночного слота",
		), nil
	}
	if before.Quantity < step.Quantity {
		return r.failedEvent(directive, trading.FailureItemUnavailable, "предмет отсутствует"), nil
	}
	if err := r.requireConfidence(
		dialog.Observation.Confidence,
		priceConfidence,
		feeConfidence,
		balanceConfidence,
		identityConfidence,
		before.Confidence,
		slotsConfidence,
	); err != nil {
		return trading.Event{}, fmt.Errorf("%s: значения до выставления имеют недостаточную уверенность: %w", methodCtx, err)
	}
	confirmed, err := r.router.Commit(
		ctx,
		agentID,
		sessionID,
		domain.StateConfirmation,
		protocol.ActionListing,
		variables,
		r.criticalValuesUnchanged(dialog.Observation, []string{
			valueItemName,
			valueSalePrice,
			valueListingFee,
			valueBalance,
			inventoryQuantityKey(step.ItemID),
			inventorySlotsKey(step.ItemID),
			valueFreeMarketSlots,
		}),
	)
	if err != nil {
		if errors.Is(err, ErrCommitValidation) {
			return r.failedEvent(
				directive,
				trading.FailureTransient,
				"критичные значения выставления изменились до подтверждения",
			), nil
		}
		return trading.Event{}, fmt.Errorf("%s: денежный переход выставления предмета %s неоднозначен: %w", methodCtx, step.ItemID, err)
	}
	monetaryActionID, err := requireMonetaryTransition(
		confirmed,
		protocol.ActionListing,
	)
	if err != nil {
		return trading.Event{}, fmt.Errorf("%s: переход выставления не подтверждён: %w", methodCtx, err)
	}
	afterBalance, afterBalanceConfidence, err := requiredInt(
		confirmed.Observation,
		valueBalance,
		0,
		maximumObservedMoney,
	)
	if err != nil {
		return trading.Event{}, fmt.Errorf("%s: не удалось прочитать баланс после выставления: %w", methodCtx, err)
	}
	after, err := readObservedItem(confirmed.Observation, step.ItemID)
	if err != nil {
		return trading.Event{}, fmt.Errorf("%s: не удалось прочитать предмет после выставления: %w", methodCtx, err)
	}
	freeAfter, afterSlotsConfidence, err := requiredInt(
		confirmed.Observation,
		valueFreeMarketSlots,
		0,
		1_000_000,
	)
	if err != nil {
		return trading.Event{}, fmt.Errorf("%s: не удалось прочитать рыночные слоты после выставления: %w", methodCtx, err)
	}
	orderID, orderIDConfidence, err := requiredText(
		confirmed.Observation,
		valueMarketOrderID,
	)
	if err != nil {
		return trading.Event{}, fmt.Errorf(
			"%s: не удалось прочитать идентификатор созданного рыночного ордера: %w",
			methodCtx,
			err,
		)
	}
	if err := r.requireConfidence(
		confirmed.Observation.Confidence,
		afterBalanceConfidence,
		after.Confidence,
		afterSlotsConfidence,
		orderIDConfidence,
	); err != nil {
		return trading.Event{}, fmt.Errorf("%s: значения после выставления имеют недостаточную уверенность: %w", methodCtx, err)
	}
	delta, err := inventoryDelta(step.ItemID, before, after)
	if err != nil {
		return trading.Event{}, fmt.Errorf("%s: не удалось вычислить изменение инвентаря: %w", methodCtx, err)
	}
	paidFee, err := checkedSubtract(balance, afterBalance)
	if err != nil || paidFee != listingFee {
		return trading.Event{}, fmt.Errorf(
			"%s: для выставления %s изменение баланса равно %d, плата за выставление %d",
			methodCtx,
			step.ItemID,
			paidFee,
			listingFee,
		)
	}
	if delta.QuantityDelta != -step.Quantity || freeAfter != freeBefore-1 {
		return trading.Event{}, fmt.Errorf(
			"%s: для выставления %s предмет или рыночный слот не подтверждён",
			methodCtx,
			step.ItemID,
		)
	}
	return r.successEvent(directive, trading.StepOutcome{
		CompletedQuantity: step.Quantity,
		Fees:              listingFee,
		MonetaryActionID:  monetaryActionID,
		MarketOrderID:     orderID,
		InventoryDeltas:   []domain.InventoryDelta{delta},
	}), nil
}

func (r *TradeRunner) executeRecovery(
	ctx context.Context,
	agentID string,
	sessionID string,
	saga trading.Saga,
	directive trading.Directive,
) (trading.Event, error) {
	const methodCtx = "automation.TradeRunner.executeRecovery"

	if directive.Compensation == nil {
		return trading.Event{}, fmt.Errorf("%s: директива восстановления не содержит плана", methodCtx)
	}
	switch directive.Compensation.Kind {
	case trading.CompensationWaitForCooldown:
		if directive.StepIndex < 0 || directive.StepIndex >= len(saga.Opportunity.Steps) {
			return trading.Event{}, fmt.Errorf("%s: шаг восстановления находится вне плана", methodCtx)
		}
		step := saga.Opportunity.Steps[directive.StepIndex]
		recipe, ok := r.recipes[step.RecipeID]
		if !ok {
			return trading.Event{}, fmt.Errorf("%s: рецепт восстановления %q не настроен", methodCtx, step.RecipeID)
		}
		variables, err := r.recipeVariables(recipe, step.Quantity)
		if err != nil {
			return trading.Event{}, fmt.Errorf("%s: не удалось подготовить параметры рецепта: %w", methodCtx, err)
		}
		result, err := r.router.Navigate(
			ctx,
			agentID,
			sessionID,
			domain.StateBarterCard,
			variables,
		)
		if err != nil {
			return trading.Event{}, fmt.Errorf("%s: не удалось открыть рецепт восстановления: %w", methodCtx, err)
		}
		cooldown, confidence, err := requiredInt(
			result.Observation,
			valueCooldownSeconds,
			0,
			int64((30*24*time.Hour)/time.Second),
		)
		if err != nil {
			return trading.Event{}, fmt.Errorf("%s: не удалось прочитать время ожидания: %w", methodCtx, err)
		}
		if err := r.requireConfidence(result.Observation.Confidence, confidence); err != nil {
			return trading.Event{}, fmt.Errorf("%s: значение времени ожидания имеет недостаточную уверенность: %w", methodCtx, err)
		}
		if cooldown > 0 {
			return trading.Event{}, fmt.Errorf("%s: обмен всё ещё недоступен: %w", methodCtx, ErrRecoveryPending)
		}
		return r.recoveryEvent(directive, trading.RecoveryRetry), nil
	case trading.CompensationQueueForSlot:
		result, err := r.router.Navigate(ctx, agentID, sessionID, domain.StateMarketHome, nil)
		if err != nil {
			return trading.Event{}, fmt.Errorf("%s: не удалось открыть рынок: %w", methodCtx, err)
		}
		slots, confidence, err := requiredInt(
			result.Observation,
			valueFreeMarketSlots,
			0,
			1_000_000,
		)
		if err != nil {
			return trading.Event{}, fmt.Errorf("%s: не удалось прочитать свободные рыночные слоты: %w", methodCtx, err)
		}
		if err := r.requireConfidence(result.Observation.Confidence, confidence); err != nil {
			return trading.Event{}, fmt.Errorf("%s: число рыночных слотов имеет недостаточную уверенность: %w", methodCtx, err)
		}
		if slots == 0 {
			return trading.Event{}, fmt.Errorf("%s: рыночный слот всё ещё недоступен: %w", methodCtx, ErrRecoveryPending)
		}
		return r.recoveryEvent(directive, trading.RecoveryRetry), nil
	case trading.CompensationNone:
		return r.recoveryEvent(directive, trading.RecoveryHold), nil
	case trading.CompensationSellOrWait, trading.CompensationHoldResult:
		// Automatically disposing a partial holding would create another
		// order saga. HOLD is the deterministic safe compensation until that
		// separate listing is explicitly calibrated.
		return r.recoveryEvent(directive, trading.RecoveryHold), nil
	default:
		return trading.Event{}, fmt.Errorf(
			"%s: неизвестная компенсация %q",
			methodCtx,
			directive.Compensation.Kind,
		)
	}
}

func (r *TradeRunner) monitorSale(
	ctx context.Context,
	agentID string,
	sessionID string,
	saga trading.Saga,
	directive trading.Directive,
) (trading.Event, error) {
	const methodCtx = "automation.TradeRunner.monitorSale"

	r.clearOrderSnapshot(saga.ID)
	if strings.TrimSpace(saga.MarketOrderID) == "" {
		return trading.Event{}, fmt.Errorf(
			"%s: сага %q не содержит идентификатор выставленного ордера",
			methodCtx,
			saga.ID,
		)
	}
	result, err := r.router.Navigate(
		ctx,
		agentID,
		sessionID,
		domain.StateMarketHome,
		map[string]string{
			"execution.id": saga.ID,
			"item.id":      saga.Opportunity.ResultItemID,
			"order.id":     saga.MarketOrderID,
		},
	)
	if err != nil {
		return trading.Event{}, fmt.Errorf("%s: не удалось открыть мониторинг продажи: %w", methodCtx, err)
	}
	snapshot, err := r.orderSnapshotFromObservation(saga, result.Observation)
	if err != nil {
		return trading.Event{}, fmt.Errorf("%s: снимок контролируемого ордера отклонён: %w", methodCtx, err)
	}
	switch snapshot.Status {
	case orderStatusActive, orderStatusListed, orderStatusPending:
		r.storeOrderSnapshot(snapshot)
		return trading.Event{}, fmt.Errorf("%s: ордер остаётся активным: %w", methodCtx, ErrSalePending)
	case orderStatusSold, orderStatusSettled:
	default:
		return trading.Event{}, fmt.Errorf("%s: неизвестное состояние ордера %q", methodCtx, snapshot.Status)
	}
	revenue, revenueConfidence, err := requiredInt(
		result.Observation,
		valueSettledRevenue,
		0,
		maximumObservedMoney,
	)
	if err != nil {
		return trading.Event{}, fmt.Errorf("%s: не удалось прочитать итоговую выручку: %w", methodCtx, err)
	}
	fees, feesConfidence, err := requiredInt(
		result.Observation,
		valueSettledFees,
		0,
		maximumObservedMoney,
	)
	if err != nil {
		return trading.Event{}, fmt.Errorf("%s: не удалось прочитать итоговые комиссии: %w", methodCtx, err)
	}
	quantity, quantityConfidence, err := requiredInt(
		result.Observation,
		valueSoldQuantity,
		1,
		1_000_000_000,
	)
	if err != nil {
		return trading.Event{}, fmt.Errorf("%s: не удалось прочитать проданное количество: %w", methodCtx, err)
	}
	itemConfidence, err := requiredExactText(
		result.Observation,
		valueSoldItemID,
		saga.Opportunity.ResultItemID,
	)
	if err != nil {
		return trading.Event{}, fmt.Errorf("%s: не удалось прочитать проданный предмет: %w", methodCtx, err)
	}
	if err := r.requireConfidence(
		revenueConfidence,
		feesConfidence,
		quantityConfidence,
		itemConfidence,
	); err != nil {
		return trading.Event{}, fmt.Errorf("%s: итоги продажи имеют недостаточную уверенность: %w", methodCtx, err)
	}
	r.storeOrderSnapshot(snapshot)
	return trading.Event{
		ID:     directive.IdempotencyKey + ":settled",
		SagaID: saga.ID,
		Kind:   trading.EventSaleSettled,
		Settlement: &domain.TradeResult{
			ExecutionID:    saga.ID,
			MarketOrderID:  saga.MarketOrderID,
			InputCost:      saga.Actual.InputCost,
			Revenue:        revenue,
			Fees:           fees,
			ResultItemID:   saga.Opportunity.ResultItemID,
			ResultQuantity: quantity,
			CompletedAt:    r.now(),
		},
		OccurredAt: r.now(),
	}, nil
}

func (r *TradeRunner) storeOrderSnapshot(value OrderSnapshot) {
	r.orderSnapshotsMu.Lock()
	defer r.orderSnapshotsMu.Unlock()
	r.orderSnapshots[value.SagaID] = value
}

func (r *TradeRunner) clearOrderSnapshot(sagaID string) {
	r.orderSnapshotsMu.Lock()
	defer r.orderSnapshotsMu.Unlock()
	delete(r.orderSnapshots, sagaID)
}

func (r *TradeRunner) takeOrderSnapshot(sagaID string) (OrderSnapshot, bool) {
	r.orderSnapshotsMu.Lock()
	defer r.orderSnapshotsMu.Unlock()
	value, exists := r.orderSnapshots[sagaID]
	delete(r.orderSnapshots, sagaID)
	return value, exists
}

func (r *TradeRunner) itemVariables(step domain.TradeStep) map[string]string {
	return map[string]string{
		"item.id":          step.ItemID,
		"item.name":        r.itemNames[step.ItemID],
		"item.quantity":    strconv.FormatInt(step.Quantity, 10),
		"item.limit_price": strconv.FormatInt(step.LimitPrice, 10),
	}
}

func registerItemName(
	names map[string]string,
	itemID string,
	itemName string,
) error {
	const methodCtx = "automation.registerItemName"

	itemID = strings.TrimSpace(itemID)
	if itemID == "" {
		return fmt.Errorf("%s: идентификатор предмета не задан", methodCtx)
	}
	canonical, err := canonicalIdentity(itemName)
	if err != nil {
		return fmt.Errorf(
			"%s: имя предмета %q некорректно: %w",
			methodCtx,
			itemID,
			err,
		)
	}
	if configured, exists := names[itemID]; exists {
		configuredCanonical, err := canonicalIdentity(configured)
		if err != nil {
			return fmt.Errorf(
				"%s: сохранённое имя предмета %q некорректно: %w",
				methodCtx,
				itemID,
				err,
			)
		}
		if configuredCanonical != canonical {
			return fmt.Errorf(
				"%s: предмет %q имеет конфликтующие имена %q и %q",
				methodCtx,
				itemID,
				configured,
				itemName,
			)
		}
		return nil
	}
	names[itemID] = strings.TrimSpace(itemName)
	return nil
}

func (r *TradeRunner) configuredItemName(itemID string) (string, error) {
	const methodCtx = "automation.TradeRunner.configuredItemName"

	name, exists := r.itemNames[strings.TrimSpace(itemID)]
	if !exists {
		return "", fmt.Errorf(
			"%s: предмет %q отсутствует в списке наблюдения и рецептах",
			methodCtx,
			itemID,
		)
	}
	if _, err := canonicalIdentity(name); err != nil {
		return "", fmt.Errorf(
			"%s: настроенное имя предмета %q некорректно: %w",
			methodCtx,
			itemID,
			err,
		)
	}
	return name, nil
}

func (r *TradeRunner) requireVisibleIdentity(
	observation domain.Observation,
	key string,
	expected string,
) (float64, error) {
	const methodCtx = "automation.TradeRunner.requireVisibleIdentity"

	confidence, err := requiredIdentity(observation, key, expected)
	if err != nil {
		return 0, fmt.Errorf(
			"%s: идентичность %q не прошла проверку: %w",
			methodCtx,
			key,
			err,
		)
	}
	if err := r.requireConfidence(observation.Confidence, confidence); err != nil {
		return 0, fmt.Errorf(
			"%s: идентичность %q имеет недостаточную уверенность: %w",
			methodCtx,
			key,
			err,
		)
	}
	return confidence, nil
}

func (r *TradeRunner) requireRecipeIdentity(
	observation domain.Observation,
	recipe appconfig.Recipe,
) (float64, error) {
	const methodCtx = "automation.TradeRunner.requireRecipeIdentity"

	confidence, err := r.requireVisibleIdentity(
		observation,
		valueContactName,
		recipe.ContactName,
	)
	if err != nil {
		return 0, fmt.Errorf(
			"%s: контакт рецепта %q не подтверждён: %w",
			methodCtx,
			recipe.ID,
			err,
		)
	}
	resultConfidence, err := r.requireVisibleIdentity(
		observation,
		valueResultItemName,
		recipe.ResultItemName,
	)
	if err != nil {
		return 0, fmt.Errorf(
			"%s: результат рецепта %q не подтверждён: %w",
			methodCtx,
			recipe.ID,
			err,
		)
	}
	confidence = min(confidence, resultConfidence)
	for _, ingredient := range recipe.Ingredients {
		ingredientConfidence, err := r.requireVisibleIdentity(
			observation,
			ingredientNameKey(ingredient.ItemID),
			ingredient.Name,
		)
		if err != nil {
			return 0, fmt.Errorf(
				"%s: ингредиент %q рецепта %q не подтверждён: %w",
				methodCtx,
				ingredient.ItemID,
				recipe.ID,
				err,
			)
		}
		confidence = min(confidence, ingredientConfidence)
	}
	return confidence, nil
}

func (r *TradeRunner) recipeVariables(
	recipe appconfig.Recipe,
	runs int64,
) (map[string]string, error) {
	const methodCtx = "automation.TradeRunner.recipeVariables"

	resultQuantity, err := money.Multiply(recipe.ResultCount, runs)
	if err != nil {
		return nil, fmt.Errorf("%s: не удалось рассчитать количество результата: %w", methodCtx, err)
	}
	return map[string]string{
		"recipe.id":       recipe.ID,
		"recipe.runs":     strconv.FormatInt(runs, 10),
		"contact.id":      recipe.ContactID,
		"contact.name":    recipe.ContactName,
		"result.id":       recipe.ResultItemID,
		"result.name":     recipe.ResultItemName,
		"result.quantity": strconv.FormatInt(resultQuantity, 10),
	}, nil
}

func (r *TradeRunner) successEvent(
	directive trading.Directive,
	outcome trading.StepOutcome,
) trading.Event {
	return trading.Event{
		ID:         directive.IdempotencyKey + ":success",
		SagaID:     directive.SagaID,
		Kind:       trading.EventStepSucceeded,
		StepIndex:  directive.StepIndex,
		Outcome:    outcome,
		OccurredAt: r.now(),
	}
}

func (r *TradeRunner) failedEvent(
	directive trading.Directive,
	reason trading.FailureReason,
	message string,
) trading.Event {
	return trading.Event{
		ID:         directive.IdempotencyKey + ":failed",
		SagaID:     directive.SagaID,
		Kind:       trading.EventStepFailed,
		StepIndex:  directive.StepIndex,
		Reason:     reason,
		Message:    message,
		OccurredAt: r.now(),
	}
}

func (r *TradeRunner) recoveryEvent(
	directive trading.Directive,
	resolution trading.RecoveryResolution,
) trading.Event {
	return trading.Event{
		ID:         directive.IdempotencyKey + ":recovery:" + string(resolution),
		SagaID:     directive.SagaID,
		Kind:       trading.EventRecoveryResolved,
		StepIndex:  directive.StepIndex,
		Resolution: resolution,
		OccurredAt: r.now(),
	}
}

func (r *TradeRunner) requireConfidence(values ...float64) error {
	const methodCtx = "automation.TradeRunner.requireConfidence"

	for _, value := range values {
		if !finiteConfidence(value) || value < r.minConfidence {
			return fmt.Errorf(
				"%s: уверенность %.3f ниже %.3f",
				methodCtx,
				value,
				r.minConfidence,
			)
		}
	}
	return nil
}

func (r *TradeRunner) criticalValuesUnchanged(
	baseline domain.Observation,
	keys []string,
) CommitValidator {
	expectedState := baseline.State
	expected := make(map[string]domain.Value, len(keys))
	for _, key := range keys {
		if value, exists := baseline.Values[key]; exists {
			expected[key] = value
		}
	}
	return func(current domain.Observation) error {
		const methodCtx = "automation.TradeRunner.criticalValuesUnchanged"

		if current.State != expectedState {
			return fmt.Errorf(
				"%s: экран изменился с %s на %s",
				methodCtx,
				expectedState,
				current.State,
			)
		}
		confidences := make([]float64, 0, len(keys)+1)
		confidences = append(confidences, current.Confidence)
		for _, key := range keys {
			expectedObserved, expectedExists := expected[key]
			currentValue, currentExists := current.Values[key]
			if !expectedExists || !currentExists {
				return fmt.Errorf(
					"%s: критичное значение %q отсутствует в одном из наблюдений",
					methodCtx,
					key,
				)
			}
			if err := requireOCRSource(key, currentValue); err != nil {
				return fmt.Errorf(
					"%s: критичное значение %q на commit-кадре получено из недопустимого источника: %w",
					methodCtx,
					key,
					err,
				)
			}
			expectedValue, err := comparableObservedValue(key, expectedObserved)
			if err != nil {
				return fmt.Errorf(
					"%s: исходное критичное значение %q не нормализуется: %w",
					methodCtx,
					key,
					err,
				)
			}
			actual, err := comparableObservedValue(key, currentValue)
			if err != nil {
				return fmt.Errorf(
					"%s: критичное значение %q на commit-кадре не нормализуется: %w",
					methodCtx,
					key,
					err,
				)
			}
			if actual != expectedValue {
				return fmt.Errorf(
					"%s: критичное значение %q изменилось с %q на %q",
					methodCtx,
					key,
					expectedValue,
					actual,
				)
			}
			confidences = append(confidences, currentValue.Confidence)
		}
		if err := r.requireConfidence(confidences...); err != nil {
			return fmt.Errorf("%s: повторное наблюдение недостаточно надёжно: %w", methodCtx, err)
		}
		return nil
	}
}

func comparableObservedValue(
	name string,
	value domain.Value,
) (string, error) {
	if isVisibleIdentityKey(name) {
		return canonicalObservedIdentity(name, value)
	}
	result := strings.TrimSpace(value.Normalized)
	if result == "" {
		result = strings.TrimSpace(value.Raw)
	}
	return result, nil
}

func requireMonetaryTransition(
	result navigation.Result,
	expected protocol.ActionClass,
) (string, error) {
	const methodCtx = "automation.requireMonetaryTransition"

	var monetary []navigation.Attempt
	for _, attempt := range result.Attempts {
		class := attempt.Request.Class
		if class == "" {
			class = protocol.ActionNavigation
		}
		if class != protocol.ActionNavigation {
			monetary = append(monetary, attempt)
		}
	}
	if len(monetary) != 1 || monetary[0].Request.Class != expected {
		classes := make([]protocol.ActionClass, 0, len(monetary))
		for _, attempt := range monetary {
			classes = append(classes, attempt.Request.Class)
		}
		return "", fmt.Errorf(
			"%s: ожидался ровно один переход класса %s, получено %v",
			methodCtx,
			expected,
			classes,
		)
	}
	attempt := monetary[0]
	actionID := strings.TrimSpace(attempt.Request.ID)
	if actionID == "" {
		return "", fmt.Errorf(
			"%s: подтверждённый денежный переход класса %s не содержит идентификатор действия",
			methodCtx,
			expected,
		)
	}
	if !attempt.Result.Success || attempt.Result.ID != actionID {
		return "", fmt.Errorf(
			"%s: денежное действие %q класса %s не имеет согласованного успешного результата",
			methodCtx,
			actionID,
			expected,
		)
	}
	return actionID, nil
}

func recipeItemIDs(recipe appconfig.Recipe) []string {
	seen := map[string]struct{}{recipe.ResultItemID: {}}
	for _, ingredient := range recipe.Ingredients {
		seen[ingredient.ItemID] = struct{}{}
	}
	result := make([]string, 0, len(seen))
	for itemID := range seen {
		result = append(result, itemID)
	}
	sort.Strings(result)
	return result
}

func readItems(
	observation domain.Observation,
	itemIDs []string,
) (map[string]observedItem, float64, error) {
	const methodCtx = "automation.readItems"

	result := make(map[string]observedItem, len(itemIDs))
	confidence := 1.0
	for _, itemID := range itemIDs {
		item, err := readObservedItem(observation, itemID)
		if err != nil {
			return nil, 0, fmt.Errorf("%s: не удалось прочитать предмет %q: %w", methodCtx, itemID, err)
		}
		result[itemID] = item
		confidence = min(confidence, item.Confidence)
	}
	return result, confidence, nil
}

func deltaQuantity(values []domain.InventoryDelta, itemID string) int64 {
	for _, value := range values {
		if value.ItemID == itemID {
			return value.QuantityDelta
		}
	}
	return 0
}

func requiredText(
	observation domain.Observation,
	name string,
) (string, float64, error) {
	const methodCtx = "automation.requiredText"

	value, exists := observation.Values[name]
	if !exists {
		return "", 0, fmt.Errorf("%s: наблюдение не содержит обязательное значение %q", methodCtx, name)
	}
	if err := requireOCRSource(name, value); err != nil {
		return "", 0, fmt.Errorf("%s: источник значения не прошёл проверку: %w", methodCtx, err)
	}
	normalized := strings.TrimSpace(value.Normalized)
	if normalized == "" {
		normalized = strings.TrimSpace(value.Raw)
	}
	if normalized == "" || len(normalized) > 512 {
		return "", 0, fmt.Errorf("%s: значение %q пустое или слишком длинное", methodCtx, name)
	}
	if !finiteConfidence(value.Confidence) {
		return "", 0, fmt.Errorf("%s: значение %q имеет некорректную уверенность", methodCtx, name)
	}
	return normalized, value.Confidence, nil
}

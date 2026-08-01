package trading

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/arena-trading-agent/arena-trading-agent/internal/domain"
	"github.com/arena-trading-agent/arena-trading-agent/internal/money"
)

func applyStepEvent(
	state *sagaState,
	event Event,
	succeeded bool,
) ([]domain.InventoryDelta, error) {
	const methodCtx = "trading.applyStepEvent"

	if state.snapshot.Status != SagaRunning {
		return nil, fmt.Errorf(
			"%s: %w: результат шага нельзя применить из состояния %s",
			methodCtx,
			ErrInvalidTransition,
			state.snapshot.Status,
		)
	}
	if event.StepIndex != state.snapshot.CurrentStep {
		return nil, fmt.Errorf(
			"%s: %w: результат относится к шагу %d, текущий шаг %d",
			methodCtx,
			ErrInvalidTransition,
			event.StepIndex,
			state.snapshot.CurrentStep,
		)
	}
	if event.Settlement != nil || event.Resolution != "" {
		return nil, fmt.Errorf("%s: %w: событие шага содержит посторонние поля", methodCtx, ErrInvalidTransition)
	}
	event.Outcome.MonetaryActionID = strings.TrimSpace(event.Outcome.MonetaryActionID)
	if succeeded {
		if event.Reason != "" {
			return nil, fmt.Errorf("%s: %w: успешный шаг содержит причину сбоя", methodCtx, ErrInvalidTransition)
		}
	} else if event.Reason == "" {
		return nil, fmt.Errorf("%s: %w: для неуспешного шага требуется причина сбоя", methodCtx, ErrInvalidTransition)
	}

	step := state.snapshot.Opportunity.Steps[state.snapshot.CurrentStep]
	remaining, err := money.Subtract(step.Quantity, state.snapshot.StepProgress)
	if err != nil || remaining <= 0 {
		return nil, fmt.Errorf("%s: %w: некорректный прогресс шага", methodCtx, ErrInvalidTransition)
	}
	completed := event.Outcome.CompletedQuantity
	if completed < 0 || completed > remaining {
		return nil, fmt.Errorf("%s: %w: выполненное количество выходит за пределы текущего шага", methodCtx, ErrInvalidTransition)
	}
	if succeeded && completed != remaining {
		return nil, fmt.Errorf(
			"%s: %w: успешный шаг должен подтвердить всё оставшееся количество",
			methodCtx,
			ErrInvalidTransition,
		)
	}
	if !succeeded && completed == remaining {
		return nil, fmt.Errorf(
			"%s: %w: неуспешный шаг не может отметить весь остаток выполненным",
			methodCtx,
			ErrInvalidTransition,
		)
	}
	if succeeded && event.Outcome.MonetaryActionID == "" {
		return nil, fmt.Errorf(
			"%s: %w: успешный денежный шаг не содержит идентификатор UI-действия",
			methodCtx,
			ErrInvalidTransition,
		)
	}
	if completed > 0 && event.Outcome.MonetaryActionID == "" {
		return nil, fmt.Errorf(
			"%s: %w: подтверждённый результат шага не связан с денежным UI-действием",
			methodCtx,
			ErrInvalidTransition,
		)
	}
	deltas, err := applyVerifiedStepOutcome(state, step, event.Outcome)
	if err != nil {
		return nil, fmt.Errorf("%s: подтверждённый результат шага отклонён: %w", methodCtx, err)
	}

	if succeeded {
		state.snapshot.CurrentStep++
		state.snapshot.StepProgress = 0
		state.snapshot.Attempt = 0
		state.snapshot.Failure = ""
		state.snapshot.Compensation = nil
		if state.snapshot.CurrentStep == len(state.snapshot.Opportunity.Steps) {
			state.snapshot.Status = SagaAwaitingSale
		}
		return deltas, nil
	}

	state.snapshot.StepProgress += completed
	state.snapshot.Failure = event.Message
	if state.snapshot.Failure == "" {
		state.snapshot.Failure = string(event.Reason)
	}
	holdings := sortedHoldings(state.holdings)
	compensation := &Compensation{
		FailedStep: state.snapshot.CurrentStep,
		Reason:     event.Reason,
		Message:    event.Message,
		Holdings:   holdings,
	}
	switch step.Kind {
	case domain.TradeStepBuy:
		if len(holdings) == 0 {
			compensation.Kind = CompensationNone
			state.snapshot.Status = SagaFailed
		} else {
			compensation.Kind = CompensationSellOrWait
			state.snapshot.Status = SagaRecovering
		}
	case domain.TradeStepBarter:
		compensation.Kind = CompensationWaitForCooldown
		state.snapshot.Status = SagaWaitingCooldown
	case domain.TradeStepList:
		if event.Reason == FailureMarketSlotUnavailable {
			compensation.Kind = CompensationQueueForSlot
			state.snapshot.Status = SagaWaitingMarketSlot
		} else {
			compensation.Kind = CompensationHoldResult
			state.snapshot.Status = SagaRecovering
		}
	default:
		return nil, fmt.Errorf("%s: %w: неподдерживаемый тип шага %q", methodCtx, ErrInvalidTransition, step.Kind)
	}
	state.snapshot.Compensation = compensation
	return deltas, nil
}

func applyRecoveryEvent(
	state *sagaState,
	event Event,
	maxStepAttempts int,
) ([]domain.InventoryDelta, error) {
	const methodCtx = "trading.applyRecoveryEvent"

	switch state.snapshot.Status {
	case SagaRecovering, SagaWaitingCooldown, SagaWaitingMarketSlot:
	default:
		return nil, fmt.Errorf(
			"%s: %w: восстановление нельзя завершить из состояния %s",
			methodCtx,
			ErrInvalidTransition,
			state.snapshot.Status,
		)
	}
	if event.StepIndex != state.snapshot.CurrentStep {
		return nil, fmt.Errorf("%s: %w: шаг восстановления не совпадает с текущим шагом", methodCtx, ErrInvalidTransition)
	}
	if event.Settlement != nil || event.Reason != "" {
		return nil, fmt.Errorf("%s: %w: событие восстановления содержит посторонние поля", methodCtx, ErrInvalidTransition)
	}
	switch event.Resolution {
	case RecoveryRetry:
		if !emptyOutcome(event.Outcome) {
			return nil, fmt.Errorf("%s: %w: повтор не может содержать результат шага", methodCtx, ErrInvalidTransition)
		}
		if state.snapshot.Attempt == math.MaxInt {
			return nil, fmt.Errorf("%s: переполнение счётчика попыток шага", methodCtx)
		}
		if state.snapshot.Attempt+1 >= maxStepAttempts {
			return nil, fmt.Errorf(
				"%s: %w: достигнут лимит попыток шага %d",
				methodCtx,
				ErrInvalidTransition,
				maxStepAttempts,
			)
		}
		state.snapshot.Attempt++
		state.snapshot.Status = SagaRunning
		state.snapshot.Compensation = nil
		state.snapshot.Failure = ""
		return nil, nil
	case RecoveryHold:
		if !emptyOutcome(event.Outcome) {
			return nil, fmt.Errorf("%s: %w: удержание не может содержать результат шага", methodCtx, ErrInvalidTransition)
		}
		state.snapshot.Status = SagaHeld
		return nil, nil
	case RecoveryCompensated:
		deltas, err := applyCompensationOutcome(state, event.Outcome)
		if err != nil {
			return nil, fmt.Errorf("%s: компенсация отклонена: %w", methodCtx, err)
		}
		if len(sortedHoldings(state.holdings)) != 0 {
			return nil, fmt.Errorf(
				"%s: %w: после компенсации остался инвентарь, относимый к саге",
				methodCtx,
				ErrInvalidTransition,
			)
		}
		state.snapshot.Status = SagaCompensated
		state.snapshot.Compensation = nil
		state.snapshot.Failure = ""
		return deltas, nil
	default:
		return nil, fmt.Errorf("%s: %w: неизвестный результат восстановления %q", methodCtx, ErrInvalidTransition, event.Resolution)
	}
}

func applyVerifiedStepOutcome(
	state *sagaState,
	step domain.TradeStep,
	outcome StepOutcome,
) ([]domain.InventoryDelta, error) {
	const methodCtx = "trading.applyVerifiedStepOutcome"

	if err := validateNonNegativeMoney(outcome); err != nil {
		return nil, fmt.Errorf("%s: некорректные денежные значения: %w", methodCtx, err)
	}
	completed := outcome.CompletedQuantity
	quantities, err := aggregateDeltas(outcome.InventoryDeltas)
	if err != nil {
		return nil, fmt.Errorf("%s: не удалось агрегировать изменения инвентаря: %w", methodCtx, err)
	}
	if completed == 0 {
		if outcome.PurchaseCost != 0 ||
			outcome.Revenue != 0 ||
			outcome.Fees != 0 ||
			outcome.MarketOrderID != "" ||
			len(outcome.InventoryDeltas) != 0 {
			return nil, fmt.Errorf(
				"%s: %w: нулевое выполненное количество имеет финансовые или инвентарные последствия",
				methodCtx,
				ErrInvalidTransition,
			)
		}
		return nil, nil
	}

	switch step.Kind {
	case domain.TradeStepBuy:
		if outcome.MarketOrderID != "" {
			return nil, fmt.Errorf(
				"%s: %w: шаг покупки содержит идентификатор рыночного ордера",
				methodCtx,
				ErrInvalidTransition,
			)
		}
		if outcome.Revenue != 0 || outcome.Fees != 0 {
			return nil, fmt.Errorf("%s: %w: шаг покупки содержит денежные значения продажи", methodCtx, ErrInvalidTransition)
		}
		ceiling, err := money.Multiply(step.LimitPrice, completed)
		if err != nil {
			return nil, fmt.Errorf("%s: не удалось рассчитать предел стоимости покупки: %w", methodCtx, err)
		}
		if outcome.PurchaseCost > ceiling {
			return nil, fmt.Errorf(
				"%s: %w: фактическая стоимость покупки %d превышает предел %d",
				methodCtx,
				ErrInvalidTransition,
				outcome.PurchaseCost,
				ceiling,
			)
		}
		if len(quantities) != 1 || quantities[step.ItemID] != completed {
			return nil, fmt.Errorf(
				"%s: %w: изменение инвентаря после покупки не совпадает с выполненным количеством",
				methodCtx,
				ErrInvalidTransition,
			)
		}
	case domain.TradeStepBarter:
		if outcome.MarketOrderID != "" {
			return nil, fmt.Errorf(
				"%s: %w: шаг обмена содержит идентификатор рыночного ордера",
				methodCtx,
				ErrInvalidTransition,
			)
		}
		if outcome.PurchaseCost != 0 || outcome.Revenue != 0 || outcome.Fees != 0 {
			return nil, fmt.Errorf("%s: %w: шаг обмена содержит денежные последствия", methodCtx, ErrInvalidTransition)
		}
		if quantities[step.ItemID] < completed {
			return nil, fmt.Errorf(
				"%s: %w: результат обмена не подтверждён в инвентаре",
				methodCtx,
				ErrInvalidTransition,
			)
		}
	case domain.TradeStepList:
		orderID := strings.TrimSpace(outcome.MarketOrderID)
		if orderID == "" {
			return nil, fmt.Errorf(
				"%s: %w: выставление не содержит подтверждённый идентификатор рыночного ордера",
				methodCtx,
				ErrInvalidTransition,
			)
		}
		if state.snapshot.MarketOrderID != "" &&
			state.snapshot.MarketOrderID != orderID {
			return nil, fmt.Errorf(
				"%s: %w: идентификатор рыночного ордера изменился с %q на %q",
				methodCtx,
				ErrInvalidTransition,
				state.snapshot.MarketOrderID,
				orderID,
			)
		}
		if outcome.PurchaseCost != 0 || outcome.Revenue != 0 {
			return nil, fmt.Errorf("%s: %w: шаг выставления содержит стоимость покупки или выручку", methodCtx, ErrInvalidTransition)
		}
		projectedFees, err := money.Add(state.snapshot.Actual.Fees, outcome.Fees)
		if err != nil {
			return nil, fmt.Errorf("%s: переполнение комиссий выставления: %w", methodCtx, err)
		}
		if projectedFees > state.snapshot.Opportunity.ExpectedFees {
			return nil, fmt.Errorf(
				"%s: %w: подтверждённые комиссии выставления %d превышают общий лимит %d",
				methodCtx,
				ErrInvalidTransition,
				projectedFees,
				state.snapshot.Opportunity.ExpectedFees,
			)
		}
		if len(quantities) != 1 || quantities[step.ItemID] != -completed {
			return nil, fmt.Errorf(
				"%s: %w: выставленное количество не удалено из инвентаря",
				methodCtx,
				ErrInvalidTransition,
			)
		}
		state.snapshot.MarketOrderID = orderID
	default:
		return nil, fmt.Errorf("%s: %w: неизвестный тип шага %q", methodCtx, ErrInvalidTransition, step.Kind)
	}
	nextHoldings, err := holdingsAfter(state.holdings, outcome.InventoryDeltas)
	if err != nil {
		return nil, fmt.Errorf("%s: не удалось обновить удержания: %w", methodCtx, err)
	}
	actual, err := financialsAfter(state.snapshot.Actual, outcome)
	if err != nil {
		return nil, fmt.Errorf("%s: не удалось обновить фактические финансы: %w", methodCtx, err)
	}
	state.holdings = nextHoldings
	state.snapshot.Actual = actual
	return append([]domain.InventoryDelta(nil), outcome.InventoryDeltas...), nil
}

func applyCompensationOutcome(
	state *sagaState,
	outcome StepOutcome,
) ([]domain.InventoryDelta, error) {
	const methodCtx = "trading.applyCompensationOutcome"

	if outcome.CompletedQuantity != 0 ||
		outcome.PurchaseCost != 0 ||
		outcome.MarketOrderID != "" {
		return nil, fmt.Errorf(
			"%s: %w: компенсация не может завершить плановый шаг или выполнить дополнительную покупку",
			methodCtx,
			ErrInvalidTransition,
		)
	}
	if strings.TrimSpace(outcome.MonetaryActionID) == "" {
		return nil, fmt.Errorf(
			"%s: %w: подтверждённая компенсация не связана с денежным UI-действием",
			methodCtx,
			ErrInvalidTransition,
		)
	}
	if outcome.Revenue < 0 || outcome.Fees < 0 {
		return nil, fmt.Errorf("%s: %w: денежные значения компенсации отрицательны", methodCtx, ErrInvalidTransition)
	}
	if len(outcome.InventoryDeltas) == 0 {
		return nil, fmt.Errorf("%s: %w: компенсация не содержит подтверждённого изменения инвентаря", methodCtx, ErrInvalidTransition)
	}
	nextHoldings, err := holdingsAfter(state.holdings, outcome.InventoryDeltas)
	if err != nil {
		return nil, fmt.Errorf("%s: не удалось обновить удержания: %w", methodCtx, err)
	}
	actual, err := financialsAfter(state.snapshot.Actual, outcome)
	if err != nil {
		return nil, fmt.Errorf("%s: не удалось обновить фактические финансы: %w", methodCtx, err)
	}
	state.holdings = nextHoldings
	state.snapshot.Actual = actual
	return append([]domain.InventoryDelta(nil), outcome.InventoryDeltas...), nil
}

func validateNonNegativeMoney(outcome StepOutcome) error {
	const methodCtx = "trading.validateNonNegativeMoney"

	if outcome.PurchaseCost < 0 || outcome.Revenue < 0 || outcome.Fees < 0 {
		return fmt.Errorf("%s: %w: подтверждённые денежные значения не могут быть отрицательными", methodCtx, ErrInvalidTransition)
	}
	return nil
}

func aggregateDeltas(
	deltas []domain.InventoryDelta,
) (map[string]int64, error) {
	const methodCtx = "trading.aggregateDeltas"

	result := make(map[string]int64, len(deltas))
	for _, delta := range deltas {
		if delta.ItemID == "" {
			return nil, fmt.Errorf("%s: %w: идентификатор предмета в изменении инвентаря пуст", methodCtx, ErrInvalidTransition)
		}
		var err error
		result[delta.ItemID], err = money.Add(result[delta.ItemID], delta.QuantityDelta)
		if err != nil {
			return nil, fmt.Errorf("%s: переполнение количества в инвентаре: %w", methodCtx, err)
		}
	}
	for itemID, quantity := range result {
		if quantity == 0 {
			delete(result, itemID)
		}
	}
	return result, nil
}

func holdingsAfter(
	current map[string]int64,
	deltas []domain.InventoryDelta,
) (map[string]int64, error) {
	const methodCtx = "trading.holdingsAfter"

	next := make(map[string]int64, len(current)+len(deltas))
	for itemID, quantity := range current {
		next[itemID] = quantity
	}
	changes, err := aggregateDeltas(deltas)
	if err != nil {
		return nil, fmt.Errorf("%s: не удалось агрегировать изменения инвентаря: %w", methodCtx, err)
	}
	itemIDs := make([]string, 0, len(changes))
	for itemID := range changes {
		itemIDs = append(itemIDs, itemID)
	}
	sort.Strings(itemIDs)
	for _, itemID := range itemIDs {
		quantity, err := money.Add(next[itemID], changes[itemID])
		if err != nil {
			return nil, fmt.Errorf("%s: переполнение количества относимого инвентаря: %w", methodCtx, err)
		}
		if quantity < 0 {
			return nil, fmt.Errorf(
				"%s: %w: шаг расходует не относимый к саге предмет %q",
				methodCtx,
				ErrInvalidTransition,
				itemID,
			)
		}
		if quantity == 0 {
			delete(next, itemID)
		} else {
			next[itemID] = quantity
		}
	}
	return next, nil
}

func financialsAfter(
	current domain.TradeFinancials,
	outcome StepOutcome,
) (domain.TradeFinancials, error) {
	const methodCtx = "trading.financialsAfter"

	var err error
	current.InputCost, err = money.Add(current.InputCost, outcome.PurchaseCost)
	if err != nil {
		return domain.TradeFinancials{}, fmt.Errorf("%s: переполнение фактической входной стоимости: %w", methodCtx, err)
	}
	current.Revenue, err = money.Add(current.Revenue, outcome.Revenue)
	if err != nil {
		return domain.TradeFinancials{}, fmt.Errorf("%s: переполнение фактической выручки: %w", methodCtx, err)
	}
	current.Fees, err = money.Add(current.Fees, outcome.Fees)
	if err != nil {
		return domain.TradeFinancials{}, fmt.Errorf("%s: переполнение фактических комиссий: %w", methodCtx, err)
	}
	current.Profit, err = money.Subtract(current.Revenue, current.Fees, current.InputCost)
	if err != nil {
		return domain.TradeFinancials{}, fmt.Errorf("%s: переполнение фактической прибыли: %w", methodCtx, err)
	}
	return current, nil
}

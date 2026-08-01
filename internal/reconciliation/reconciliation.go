// Package reconciliation compares a planned trade with verified actual facts.
package reconciliation

import (
	"fmt"
	"strconv"

	"github.com/arena-trading-agent/arena-trading-agent/internal/domain"
	"github.com/arena-trading-agent/arena-trading-agent/internal/money"
)

// Reconcile performs an exact integer comparison. It derives both profit
// values and returns an error instead of accepting overflow or negative raw
// money observations.
func Reconcile(
	opportunity domain.TradeOpportunity,
	actual domain.TradeResult,
) (domain.ReconciliationReport, error) {
	const methodCtx = "reconciliation.Reconcile"

	expected, err := financials(
		opportunity.InputCost,
		opportunity.ExpectedRevenue,
		opportunity.ExpectedFees,
	)
	if err != nil {
		return domain.ReconciliationReport{}, fmt.Errorf("%s: некорректные ожидаемые параметры сделки: %w", methodCtx, err)
	}
	actualFinancials, err := financials(actual.InputCost, actual.Revenue, actual.Fees)
	if err != nil {
		return domain.ReconciliationReport{}, fmt.Errorf("%s: некорректные фактические параметры сделки: %w", methodCtx, err)
	}
	if opportunity.ResultQuantity < 0 {
		return domain.ReconciliationReport{}, fmt.Errorf("%s: ожидаемое количество результата не может быть отрицательным", methodCtx)
	}
	if actual.ResultQuantity < 0 {
		return domain.ReconciliationReport{}, fmt.Errorf("%s: фактическое количество результата не может быть отрицательным", methodCtx)
	}

	report := domain.ReconciliationReport{
		OpportunityID:       opportunity.ID,
		ExecutionID:         actual.ExecutionID,
		Expected:            expected,
		Actual:              actualFinancials,
		ExpectedResultItem:  opportunity.ResultItemID,
		ActualResultItem:    actual.ResultItemID,
		ExpectedResultCount: opportunity.ResultQuantity,
		ActualResultCount:   actual.ResultQuantity,
	}
	report.InputCostVariance, err = variance(actual.InputCost, opportunity.InputCost)
	if err != nil {
		return domain.ReconciliationReport{}, fmt.Errorf("%s: не удалось вычислить отклонение входной стоимости: %w", methodCtx, err)
	}
	report.RevenueVariance, err = variance(actual.Revenue, opportunity.ExpectedRevenue)
	if err != nil {
		return domain.ReconciliationReport{}, fmt.Errorf("%s: не удалось вычислить отклонение выручки: %w", methodCtx, err)
	}
	report.FeesVariance, err = variance(actual.Fees, opportunity.ExpectedFees)
	if err != nil {
		return domain.ReconciliationReport{}, fmt.Errorf("%s: не удалось вычислить отклонение комиссий: %w", methodCtx, err)
	}
	report.ProfitVariance, err = variance(actualFinancials.Profit, expected.Profit)
	if err != nil {
		return domain.ReconciliationReport{}, fmt.Errorf("%s: не удалось вычислить отклонение прибыли: %w", methodCtx, err)
	}

	appendMoneyMismatch := func(field string, expectedValue, actualValue int64) {
		if expectedValue == actualValue {
			return
		}
		report.Mismatches = append(report.Mismatches, domain.ReconciliationMismatch{
			Field:    field,
			Expected: strconv.FormatInt(expectedValue, 10),
			Actual:   strconv.FormatInt(actualValue, 10),
		})
	}
	appendMoneyMismatch("input_cost", expected.InputCost, actualFinancials.InputCost)
	appendMoneyMismatch("revenue", expected.Revenue, actualFinancials.Revenue)
	appendMoneyMismatch("fees", expected.Fees, actualFinancials.Fees)
	appendMoneyMismatch("profit", expected.Profit, actualFinancials.Profit)
	if opportunity.ResultItemID != actual.ResultItemID {
		report.Mismatches = append(report.Mismatches, domain.ReconciliationMismatch{
			Field:    "result_item",
			Expected: opportunity.ResultItemID,
			Actual:   actual.ResultItemID,
		})
	}
	if opportunity.ResultQuantity != actual.ResultQuantity {
		report.Mismatches = append(report.Mismatches, domain.ReconciliationMismatch{
			Field:    "result_quantity",
			Expected: strconv.FormatInt(opportunity.ResultQuantity, 10),
			Actual:   strconv.FormatInt(actual.ResultQuantity, 10),
		})
	}
	report.Matched = len(report.Mismatches) == 0
	return report, nil
}

func financials(inputCost, revenue, fees int64) (domain.TradeFinancials, error) {
	const methodCtx = "reconciliation.financials"

	if inputCost < 0 || revenue < 0 || fees < 0 {
		return domain.TradeFinancials{}, fmt.Errorf("%s: денежные значения не могут быть отрицательными", methodCtx)
	}
	profit, err := money.Subtract(revenue, fees, inputCost)
	if err != nil {
		return domain.TradeFinancials{}, fmt.Errorf("%s: не удалось рассчитать прибыль: %w", methodCtx, err)
	}
	return domain.TradeFinancials{
		InputCost: inputCost,
		Revenue:   revenue,
		Fees:      fees,
		Profit:    profit,
	}, nil
}

func variance(actual, expected int64) (int64, error) {
	return money.Subtract(actual, expected)
}

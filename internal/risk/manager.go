// Package risk применяет обязательные ограничения перед исполнением сделки.
package risk

import (
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/arena-trading-agent/arena-trading-agent/internal/domain"
)

// Manager проверяет возможности относительно лимитов сессии.
type Manager struct {
	now func() time.Time
}

// NewManager создаёт менеджер риска с системными часами.
func NewManager() Manager { return Manager{now: time.Now} }

// NewManagerWithClock создаёт менеджер с переданными часами для
// детерминированных проверок сессии и тестов.
func NewManagerWithClock(now func() time.Time) Manager {
	if now == nil {
		now = time.Now
	}
	return Manager{now: now}
}

// Validate возвращает все причины, по которым сделку нельзя исполнять.
func (m Manager) Validate(opportunity domain.TradeOpportunity, limits domain.RiskLimits) error {
	const methodCtx = "risk.Manager.Validate"

	var violations []error
	now := time.Now()
	if m.now != nil {
		now = m.now()
	}
	if limits.MaxBudget < 0 {
		violations = append(violations, fmt.Errorf("%s: максимальный бюджет не может быть отрицательным", methodCtx))
	}
	if limits.MaxItemPrice < 0 {
		violations = append(violations, fmt.Errorf("%s: максимальная цена предмета не может быть отрицательной", methodCtx))
	}
	if limits.MinProfit < 0 {
		violations = append(violations, fmt.Errorf("%s: минимальная прибыль не может быть отрицательной", methodCtx))
	}
	if math.IsNaN(limits.MinROI) || math.IsInf(limits.MinROI, 0) || limits.MinROI < 0 {
		violations = append(violations, fmt.Errorf("%s: минимальный ROI некорректен", methodCtx))
	}
	if math.IsNaN(limits.MinConfidence) || math.IsInf(limits.MinConfidence, 0) ||
		limits.MinConfidence < 0 || limits.MinConfidence > 1 {
		violations = append(violations, fmt.Errorf("%s: минимальная уверенность некорректна", methodCtx))
	}
	if limits.MaxQuoteAge < 0 {
		violations = append(violations, fmt.Errorf("%s: максимальный возраст котировки не может быть отрицательным", methodCtx))
	}
	if limits.AvailableSlots < 0 {
		violations = append(violations, fmt.Errorf("%s: число свободных слотов не может быть отрицательным", methodCtx))
	}
	if opportunity.InputCost < 0 || opportunity.ExpectedRevenue < 0 || opportunity.ExpectedFees < 0 {
		violations = append(violations, fmt.Errorf("%s: денежные значения возможности некорректны", methodCtx))
	}
	if opportunity.InputCost > limits.MaxBudget {
		violations = append(violations, fmt.Errorf("%s: стоимость %d превышает бюджет %d", methodCtx, opportunity.InputCost, limits.MaxBudget))
	}
	if opportunity.ExpectedProfit < limits.MinProfit {
		violations = append(violations, fmt.Errorf("%s: прибыль ниже минимальной", methodCtx))
	}
	if math.IsNaN(opportunity.ROI) || math.IsInf(opportunity.ROI, 0) || opportunity.ROI < limits.MinROI {
		violations = append(violations, fmt.Errorf("%s: ROI ниже минимального", methodCtx))
	}
	if math.IsNaN(opportunity.Confidence) || math.IsInf(opportunity.Confidence, 0) ||
		opportunity.Confidence < 0 || opportunity.Confidence > 1 ||
		opportunity.Confidence < limits.MinConfidence {
		violations = append(violations, fmt.Errorf("%s: уверенность распознавания ниже минимальной", methodCtx))
	}
	if opportunity.RequiredSlots < 0 {
		violations = append(violations, fmt.Errorf("%s: требуемое число слотов некорректно", methodCtx))
	} else if opportunity.RequiredSlots > limits.AvailableSlots {
		violations = append(violations, fmt.Errorf("%s: недостаточно свободных слотов", methodCtx))
	}
	if !opportunity.ExpiresAt.IsZero() && !now.Before(opportunity.ExpiresAt) {
		violations = append(violations, fmt.Errorf("%s: котировка устарела", methodCtx))
	}
	if limits.MaxQuoteAge > 0 {
		switch {
		case opportunity.QuoteObservedAt.IsZero():
			violations = append(violations, fmt.Errorf("%s: время котировки неизвестно", methodCtx))
		case opportunity.QuoteObservedAt.After(now):
			violations = append(violations, fmt.Errorf("%s: время котировки находится в будущем", methodCtx))
		case now.Sub(opportunity.QuoteObservedAt) > limits.MaxQuoteAge:
			violations = append(violations, fmt.Errorf("%s: возраст котировки превышает допустимый", methodCtx))
		}
	}
	if !limits.AllowUnknownResale && !opportunity.ResaleKnown {
		violations = append(violations, fmt.Errorf("%s: возможность перепродажи неизвестна", methodCtx))
	}
	buySteps := 0
	for _, step := range opportunity.Steps {
		switch step.Kind {
		case domain.TradeStepBarter, domain.TradeStepList:
			continue
		case domain.TradeStepBuy:
			buySteps++
		default:
			violations = append(violations, fmt.Errorf("%s: неизвестный шаг торгового плана %q", methodCtx, step.Kind))
			continue
		}
		if step.ItemID == "" || step.LimitPrice < 0 || step.Quantity <= 0 {
			violations = append(violations, fmt.Errorf("%s: шаг покупки предмета %q некорректен", methodCtx, step.ItemID))
			continue
		}
		if step.LimitPrice > limits.MaxItemPrice {
			violations = append(violations, fmt.Errorf("%s: цена предмета %q превышает лимит", methodCtx, step.ItemID))
		}
	}
	if opportunity.InputCost > 0 && buySteps == 0 {
		violations = append(violations, fmt.Errorf("%s: цена покупки не может быть проверена: в плане нет шагов покупки", methodCtx))
	}
	return errors.Join(violations...)
}

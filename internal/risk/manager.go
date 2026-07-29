// Package risk применяет обязательные ограничения перед исполнением сделки.
package risk

import (
	"errors"
	"fmt"
	"time"

	"github.com/arena-trading-agent/arena-trading-agent/internal/domain"
)

// Manager проверяет возможности относительно лимитов сессии.
type Manager struct {
	now func() time.Time
}

// NewManager создаёт менеджер риска с системными часами.
func NewManager() Manager { return Manager{now: time.Now} }

// Validate возвращает все причины, по которым сделку нельзя исполнять.
func (m Manager) Validate(opportunity domain.TradeOpportunity, limits domain.RiskLimits) error {
	var violations []error
	if opportunity.InputCost > limits.MaxBudget {
		violations = append(violations, fmt.Errorf("стоимость %d превышает бюджет %d", opportunity.InputCost, limits.MaxBudget))
	}
	if opportunity.ExpectedProfit < limits.MinProfit {
		violations = append(violations, fmt.Errorf("прибыль ниже минимальной"))
	}
	if opportunity.ROI < limits.MinROI {
		violations = append(violations, fmt.Errorf("ROI ниже минимального"))
	}
	if opportunity.Confidence < limits.MinConfidence {
		violations = append(violations, fmt.Errorf("уверенность распознавания ниже минимальной"))
	}
	if opportunity.RequiredSlots > limits.AvailableSlots {
		violations = append(violations, fmt.Errorf("недостаточно свободных слотов"))
	}
	if !opportunity.ExpiresAt.IsZero() && !m.now().Before(opportunity.ExpiresAt) {
		violations = append(violations, fmt.Errorf("котировка устарела"))
	}
	for _, step := range opportunity.Steps {
		if step.LimitPrice > limits.MaxItemPrice {
			violations = append(violations, fmt.Errorf("цена предмета %q превышает лимит", step.ItemID))
		}
	}
	return errors.Join(violations...)
}

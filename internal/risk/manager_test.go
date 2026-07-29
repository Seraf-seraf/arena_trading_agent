package risk_test

import (
	"strings"
	"testing"
	"time"

	"github.com/arena-trading-agent/arena-trading-agent/internal/domain"
	"github.com/arena-trading-agent/arena-trading-agent/internal/risk"
)

func TestValidateReturnsAllViolations(t *testing.T) {
	opportunity := domain.TradeOpportunity{InputCost: 200, ExpectedProfit: -1, Confidence: .2, RequiredSlots: 3, ExpiresAt: time.Now().Add(-time.Minute)}
	limits := domain.RiskLimits{MaxBudget: 100, MinProfit: 10, MinROI: .1, MinConfidence: .8, AvailableSlots: 1}
	err := risk.NewManager().Validate(opportunity, limits)
	if err == nil {
		t.Fatal("ожидался отказ менеджера риска")
	}
	for _, want := range []string{"бюджет", "прибыль", "ROI", "уверенность", "слотов", "устарела"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("ошибка %q не содержит %q", err, want)
		}
	}
}

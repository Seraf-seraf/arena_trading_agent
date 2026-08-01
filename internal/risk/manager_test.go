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

func TestValidateAppliesQuoteAgeItemPriceAndResaleLimits(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 29, 10, 0, 0, 0, time.UTC)
	opportunity := domain.TradeOpportunity{
		InputCost:       90,
		ExpectedRevenue: 150,
		ExpectedFees:    10,
		ExpectedProfit:  50,
		ROI:             0.5,
		Confidence:      .95,
		RequiredSlots:   1,
		QuoteObservedAt: now.Add(-11 * time.Minute),
		ResaleKnown:     false,
		Steps: []domain.TradeStep{
			{Kind: domain.TradeStepBuy, ItemID: "input", Quantity: 1, LimitPrice: 60},
			// Sale prices are proceeds, not individual purchase exposure.
			{Kind: domain.TradeStepList, ItemID: "result", Quantity: 1, LimitPrice: 1_000},
		},
	}
	limits := domain.RiskLimits{
		MaxBudget:          100,
		MaxItemPrice:       50,
		MinProfit:          10,
		MinROI:             .1,
		MinConfidence:      .8,
		MaxQuoteAge:        10 * time.Minute,
		AvailableSlots:     1,
		AllowUnknownResale: false,
	}
	err := risk.NewManagerWithClock(func() time.Time { return now }).Validate(opportunity, limits)
	if err == nil {
		t.Fatal("expected risk violations")
	}
	for _, want := range []string{"возраст котировки", "перепродажи", "цена предмета"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "result") {
		t.Errorf("sale step was incorrectly checked as purchase exposure: %v", err)
	}
}

func TestValidateAcceptsOpportunityAtEveryBoundary(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 29, 10, 0, 0, 0, time.UTC)
	opportunity := domain.TradeOpportunity{
		InputCost:       100,
		ExpectedRevenue: 130,
		ExpectedFees:    10,
		ExpectedProfit:  20,
		ROI:             .2,
		Confidence:      .8,
		RequiredSlots:   2,
		QuoteObservedAt: now.Add(-10 * time.Minute),
		ResaleKnown:     true,
		ExpiresAt:       now.Add(time.Second),
		Steps: []domain.TradeStep{{
			Kind:       domain.TradeStepBuy,
			ItemID:     "input",
			Quantity:   1,
			LimitPrice: 50,
		}},
	}
	limits := domain.RiskLimits{
		MaxBudget:          100,
		MaxItemPrice:       50,
		MinProfit:          20,
		MinROI:             .2,
		MinConfidence:      .8,
		MaxQuoteAge:        10 * time.Minute,
		AvailableSlots:     2,
		AllowUnknownResale: false,
	}
	if err := risk.NewManagerWithClock(func() time.Time { return now }).Validate(opportunity, limits); err != nil {
		t.Fatalf("boundary opportunity rejected: %v", err)
	}
}

func TestValidateCannotBypassItemLimitWithMissingBuyStep(t *testing.T) {
	t.Parallel()
	opportunity := domain.TradeOpportunity{
		InputCost:       10,
		ExpectedRevenue: 20,
		ExpectedProfit:  10,
		ROI:             1,
		Confidence:      1,
		ResaleKnown:     true,
	}
	limits := domain.RiskLimits{
		MaxBudget:          20,
		MaxItemPrice:       5,
		MinConfidence:      1,
		AvailableSlots:     1,
		AllowUnknownResale: false,
	}
	err := risk.NewManager().Validate(opportunity, limits)
	if err == nil || !strings.Contains(err.Error(), "нет шагов покупки") {
		t.Fatalf("missing purchase step bypassed risk manager: %v", err)
	}
}

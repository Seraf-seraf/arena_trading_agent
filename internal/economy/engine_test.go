package economy_test

import (
	"math"
	"testing"

	"github.com/arena-trading-agent/arena-trading-agent/internal/domain"
	"github.com/arena-trading-agent/arena-trading-agent/internal/economy"
)

func TestDirectProfit(t *testing.T) {
	profit, err := economy.DirectProfit(30_000, 2_000, 500, 9_070)
	if err != nil {
		t.Fatal(err)
	}
	if profit != 18_430 {
		t.Fatalf("profit = %d, ожидалось 18430", profit)
	}
}

func TestBarterCost(t *testing.T) {
	cost, err := economy.BarterCost(map[string]int64{"a": 10, "b": 25}, []domain.BarterIngredient{{ItemID: "a", Quantity: 3}, {ItemID: "b", Quantity: 2}})
	if err != nil {
		t.Fatal(err)
	}
	if cost != 80 {
		t.Fatalf("cost = %d, ожидалось 80", cost)
	}
}

func TestBarterCostRequiresEveryPrice(t *testing.T) {
	_, err := economy.BarterCost(nil, []domain.BarterIngredient{{ItemID: "unknown", Quantity: 1}})
	if err == nil {
		t.Fatal("ожидалась ошибка отсутствующей цены")
	}
}

func TestMoneyCalculationsRejectOverflow(t *testing.T) {
	t.Parallel()
	if _, err := economy.DirectProfit(0, math.MaxInt64, 0, math.MaxInt64); err == nil {
		t.Fatal("DirectProfit accepted int64 underflow")
	}
	if _, err := economy.Complete(domain.TradeOpportunity{
		InputCost:       math.MaxInt64,
		ExpectedRevenue: 0,
		ExpectedFees:    math.MaxInt64,
	}); err == nil {
		t.Fatal("Complete accepted int64 underflow")
	}
}

func TestCompleteResetsROIForZeroCost(t *testing.T) {
	t.Parallel()
	value, err := economy.Complete(domain.TradeOpportunity{
		ExpectedRevenue: 10,
		ROI:             123,
	})
	if err != nil {
		t.Fatal(err)
	}
	if value.ROI != 0 {
		t.Fatalf("zero-cost ROI = %v, want deterministic zero", value.ROI)
	}
}

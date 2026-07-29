package economy_test

import (
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

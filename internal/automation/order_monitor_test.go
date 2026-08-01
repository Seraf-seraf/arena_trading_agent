package automation

import (
	"strings"
	"testing"
	"time"

	"github.com/arena-trading-agent/arena-trading-agent/internal/domain"
	"github.com/arena-trading-agent/arena-trading-agent/internal/trading"
)

func TestOrderSnapshotRecommendationIsSafeAndDeterministic(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name             string
		status           string
		listedPrice      string
		currentPrice     string
		wantRecommended  bool
		wantPrice        int64
		wantSlotOccupied bool
	}{
		{
			name: "active recommend", status: orderStatusActive,
			listedPrice: "30", currentPrice: "25",
			wantRecommended: true, wantPrice: 25, wantSlotOccupied: true,
		},
		{
			name: "below minimum", status: orderStatusActive,
			listedPrice: "30", currentPrice: "19",
			wantSlotOccupied: true,
		},
		{
			name: "equal minimum", status: orderStatusListed,
			listedPrice: "30", currentPrice: "20",
			wantRecommended: true, wantPrice: 20, wantSlotOccupied: true,
		},
		{
			name: "equal listed", status: orderStatusActive,
			listedPrice: "30", currentPrice: "30",
			wantSlotOccupied: true,
		},
		{
			name: "sold", status: orderStatusSold,
			listedPrice: "30", currentPrice: "25",
			wantSlotOccupied: false,
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			runner := &TradeRunner{minConfidence: .9}
			snapshot, err := runner.orderSnapshotFromObservation(
				testOrderSaga(),
				testOrderObservation(test.status, test.listedPrice, test.currentPrice),
			)
			if err != nil {
				t.Fatalf("снимок отклонён: %v", err)
			}
			if snapshot.RepriceRecommended != test.wantRecommended ||
				snapshot.RecommendedPrice != test.wantPrice ||
				snapshot.SlotOccupied != test.wantSlotOccupied {
				t.Fatalf("неверная рекомендация: %+v", snapshot)
			}
			if !snapshot.RecommendationOnly || snapshot.AutomaticRepriceAllowed {
				t.Fatalf("снимок разрешил автоматическое перевыставление: %+v", snapshot)
			}
		})
	}
}

func TestOrderSnapshotRejectsUnverifiedIdentityAndConfidence(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		tamper func(*domain.Observation)
	}{
		{
			name: "market order mismatch",
			tamper: func(value *domain.Observation) {
				value.Values[valueMarketOrderID] = testOCRValue("other-order")
			},
		},
		{
			name: "item mismatch",
			tamper: func(value *domain.Observation) {
				value.Values[valueOrderItemID] = testOCRValue("other-item")
			},
		},
		{
			name: "raw normalized mismatch",
			tamper: func(value *domain.Observation) {
				field := value.Values[valueMarketOrderID]
				field.Normalized = "other-order"
				value.Values[valueMarketOrderID] = field
			},
		},
		{
			name: "non OCR identity",
			tamper: func(value *domain.Observation) {
				field := value.Values[valueOrderItemID]
				field.Source = "MODEL"
				value.Values[valueOrderItemID] = field
			},
		},
		{
			name: "low confidence",
			tamper: func(value *domain.Observation) {
				field := value.Values[valueOrderStatus]
				field.Confidence = .5
				value.Values[valueOrderStatus] = field
			},
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			observation := testOrderObservation(orderStatusActive, "30", "25")
			test.tamper(&observation)
			runner := &TradeRunner{minConfidence: .9}
			if _, err := runner.orderSnapshotFromObservation(
				testOrderSaga(),
				observation,
			); err == nil {
				t.Fatal("непроверенный снимок ошибочно принят")
			}
		})
	}
}

func testOrderSaga() trading.Saga {
	return trading.Saga{
		ID:            "execution",
		Status:        trading.SagaAwaitingSale,
		MarketOrderID: "order-1",
		Opportunity: domain.TradeOpportunity{
			ResultItemID: "item",
			Steps: []domain.TradeStep{{
				Kind: domain.TradeStepList, ItemID: "item",
				Quantity: 1, LimitPrice: 20,
			}},
		},
	}
}

func testOrderObservation(status, listedPrice, currentPrice string) domain.Observation {
	observedAt := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	values := map[string]domain.Value{
		valueMarketOrderID:           testOCRValue("order-1"),
		valueOrderItemID:             testOCRValue("item"),
		valueOrderStatus:             testOCRValue(status),
		valueOrderListedPrice:        testOCRValue(listedPrice),
		valueOrderCurrentMarketPrice: testOCRValue(currentPrice),
		valueOrderAgeSeconds:         testOCRValue("120"),
	}
	return domain.Observation{
		FrameID: 10, CreatedAt: observedAt, Confidence: .99, Values: values,
	}
}

func testOCRValue(value string) domain.Value {
	return domain.Value{
		Raw: value, Normalized: strings.TrimSpace(value),
		Source: "OCR", Confidence: .99,
	}
}

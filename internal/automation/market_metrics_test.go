package automation

import (
	"context"
	"testing"
	"time"

	"github.com/arena-trading-agent/arena-trading-agent/internal/domain"
	"github.com/arena-trading-agent/arena-trading-agent/internal/repository"
)

func TestConservativeMarketMetricsUsesFailSafeDefaultsWithoutHistory(t *testing.T) {
	current := metricTestQuote("item", time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC), 100, 150)
	history := []domain.TradeQuote{
		metricTestQuote("item", current.ObservedAt.Add(-time.Hour), 100, 150),
		metricTestQuote("item", current.ObservedAt.Add(-2*time.Hour), 100, 150),
		// Повтор той же временной точки не является независимым наблюдением.
		metricTestQuote("item", current.ObservedAt.Add(-2*time.Hour), 100, 150),
		// Записи другого предмета и будущие записи не дают свидетельства.
		metricTestQuote("other", current.ObservedAt.Add(-3*time.Hour), 100, 150),
		metricTestQuote("item", current.ObservedAt.Add(time.Hour), 100, 150),
	}

	metrics := conservativeMarketMetrics(current, history)
	if metrics.resaleKnown || metrics.liquidityScore != 0 || metrics.priceVolatility != 1 {
		t.Fatalf("небезопасные метрики при недостатке истории: %#v", metrics)
	}
}

func TestConservativeMarketMetricsDerivesStableMarketConservatively(t *testing.T) {
	current := metricTestQuote("item", time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC), 100, 150)
	history := []domain.TradeQuote{
		metricTestQuote("item", current.ObservedAt.Add(-time.Hour), 100, 150),
		metricTestQuote("item", current.ObservedAt.Add(-2*time.Hour), 100, 150),
		metricTestQuote("item", current.ObservedAt.Add(-3*time.Hour), 100, 150),
	}

	metrics := conservativeMarketMetrics(current, history)
	if !metrics.resaleKnown || metrics.priceVolatility != 0 ||
		metrics.liquidityScore != marketMetricsLiquidityCap {
		t.Fatalf("метрики стабильного рынка некорректны: %#v", metrics)
	}
}

func TestConservativeMarketMetricsUsesWorstAdjacentPriceMovement(t *testing.T) {
	current := metricTestQuote("item", time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC), 100, 200)
	history := []domain.TradeQuote{
		metricTestQuote("item", current.ObservedAt.Add(-time.Hour), 125, 180),
		metricTestQuote("item", current.ObservedAt.Add(-2*time.Hour), 100, 200),
		metricTestQuote("item", current.ObservedAt.Add(-3*time.Hour), 100, 200),
	}

	metrics := conservativeMarketMetrics(current, history)
	if !metrics.resaleKnown || metrics.priceVolatility != 0.25 ||
		metrics.liquidityScore != 0.5625 {
		t.Fatalf("метрики волатильного рынка некорректны: %#v", metrics)
	}
}

func TestConservativeMarketMetricsLimitsEvenDirectHistoryInput(t *testing.T) {
	current := metricTestQuote("item", time.Date(2026, 7, 30, 20, 0, 0, 0, time.UTC), 100, 150)
	history := make([]domain.TradeQuote, 0, marketMetricsHistoryLimit+1)
	for hoursAgo := 1; hoursAgo <= marketMetricsHistoryLimit; hoursAgo++ {
		history = append(
			history,
			metricTestQuote("item", current.ObservedAt.Add(-time.Duration(hoursAgo)*time.Hour), 100, 150),
		)
	}
	// Тринадцатая, самая старая котировка резко отличается. Она не должна
	// влиять на ограниченное окно из двенадцати более свежих записей.
	history = append(
		history,
		metricTestQuote("item", current.ObservedAt.Add(-13*time.Hour), 1, 1),
	)

	metrics := conservativeMarketMetrics(current, history)
	if metrics.priceVolatility != 0 || metrics.liquidityScore != marketMetricsLiquidityCap {
		t.Fatalf("расчёт вышел за ограниченное окно истории: %#v", metrics)
	}
}

type metricHistoryStore struct {
	repository.Store
	filter domain.TradeQuoteFilter
	values []domain.TradeQuote
}

func (s *metricHistoryStore) ListTradeQuotes(
	_ context.Context,
	filter domain.TradeQuoteFilter,
) ([]domain.TradeQuote, error) {
	s.filter = filter
	return append([]domain.TradeQuote(nil), s.values...), nil
}

func TestTradeQuoteWithMarketMetricsRequestsBoundedHistory(t *testing.T) {
	current := metricTestQuote("item", time.Date(2026, 7, 30, 20, 0, 0, 0, time.UTC), 100, 150)
	store := &metricHistoryStore{Store: repository.NewMemory()}

	enriched, err := tradeQuoteWithMarketMetrics(context.Background(), store, current)
	if err != nil {
		t.Fatalf("tradeQuoteWithMarketMetrics() вернул ошибку: %v", err)
	}
	if enriched.ResaleKnown || enriched.LiquidityScore != 0 || enriched.PriceVolatility != 1 {
		t.Fatalf("начальная котировка получила небезопасные метрики: %#v", enriched)
	}
	if store.filter.ItemID != current.ItemID ||
		store.filter.Limit != marketMetricsHistoryLimit ||
		!store.filter.Since.Equal(current.ObservedAt.Add(-marketMetricsLookback)) ||
		!store.filter.Until.Equal(current.ObservedAt.Add(-time.Nanosecond)) {
		t.Fatalf("запрошена неограниченная или неверная история: %#v", store.filter)
	}
}

func metricTestQuote(
	itemID string,
	observedAt time.Time,
	purchasePrice int64,
	salePrice int64,
) domain.TradeQuote {
	return domain.TradeQuote{
		ItemID:         itemID,
		PurchasePrice:  purchasePrice,
		SalePrice:      salePrice,
		SaleCommission: 10,
		ListingFee:     5,
		ObservedAt:     observedAt,
		Confidence:     1,
	}
}

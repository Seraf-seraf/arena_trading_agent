package automation

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/arena-trading-agent/arena-trading-agent/internal/domain"
	"github.com/arena-trading-agent/arena-trading-agent/internal/repository"
)

const (
	marketMetricsHistoryLimit   = 12
	marketMetricsMinimumHistory = 3
	marketMetricsLookback       = 24 * time.Hour
	marketMetricsLiquidityCap   = 0.75
)

type marketMetrics struct {
	liquidityScore  float64
	priceVolatility float64
	resaleKnown     bool
}

// tradeQuoteWithMarketMetrics вычисляет аналитические поля котировки только
// из текущих денежных значений и ограниченной истории. UI и OCR не являются
// источником ликвидности, волатильности или признака возможности перепродажи.
func tradeQuoteWithMarketMetrics(
	ctx context.Context,
	store repository.Store,
	current domain.TradeQuote,
) (domain.TradeQuote, error) {
	const methodCtx = "automation.tradeQuoteWithMarketMetrics"

	if ctx == nil {
		return domain.TradeQuote{}, fmt.Errorf("%s: контекст не задан", methodCtx)
	}
	if err := ctx.Err(); err != nil {
		return domain.TradeQuote{}, fmt.Errorf("%s: контекст завершён: %w", methodCtx, err)
	}
	if store == nil {
		return domain.TradeQuote{}, fmt.Errorf("%s: репозиторий не задан", methodCtx)
	}
	if strings.TrimSpace(current.ItemID) == "" {
		return domain.TradeQuote{}, fmt.Errorf("%s: идентификатор предмета пуст", methodCtx)
	}
	if current.ObservedAt.IsZero() {
		return domain.TradeQuote{}, fmt.Errorf("%s: время текущей котировки не задано", methodCtx)
	}

	history, err := store.ListTradeQuotes(ctx, domain.TradeQuoteFilter{
		ItemID: current.ItemID,
		Since:  current.ObservedAt.Add(-marketMetricsLookback),
		Until:  current.ObservedAt.Add(-time.Nanosecond),
		Limit:  marketMetricsHistoryLimit,
	})
	if err != nil {
		return domain.TradeQuote{}, fmt.Errorf(
			"%s: не удалось получить ограниченную историю предмета %q: %w",
			methodCtx,
			current.ItemID,
			err,
		)
	}
	metrics := conservativeMarketMetrics(current, history)
	current.LiquidityScore = metrics.liquidityScore
	current.PriceVolatility = metrics.priceVolatility
	current.ResaleKnown = metrics.resaleKnown
	return current, nil
}

// conservativeMarketMetrics применяет детерминированную консервативную
// формулу:
//
//   - используются не более 12 предыдущих котировок того же предмета за 24 часа;
//   - при менее чем трёх предыдущих наблюдениях с различными метками времени
//     перепродажа считается неизвестной, ликвидность равна 0, а
//     волатильность — 1;
//   - волатильность равна максимальному соседнему изменению цены покупки или
//     продажи: abs(a-b)/min(a,b), ограниченному диапазоном [0, 1];
//   - при достаточной истории ResaleKnown=true, а LiquidityScore равна
//     0.75*(1-volatility). Ограничение 0.75 не позволяет приравнять стабильные
//     котировки к доказанному факту быстрой продажи.
//
// Повреждённые, будущие, слишком старые и относящиеся к другому предмету
// записи не дают положительного свидетельства и потому отбрасываются.
func conservativeMarketMetrics(
	current domain.TradeQuote,
	history []domain.TradeQuote,
) marketMetrics {
	result := marketMetrics{
		liquidityScore:  0,
		priceVolatility: 1,
		resaleKnown:     false,
	}
	if strings.TrimSpace(current.ItemID) == "" ||
		current.PurchasePrice <= 0 ||
		current.SalePrice <= 0 ||
		current.ObservedAt.IsZero() {
		return result
	}

	oldestAllowed := current.ObservedAt.Add(-marketMetricsLookback)
	samples := make([]domain.TradeQuote, 0, min(len(history), marketMetricsHistoryLimit))
	for _, quote := range history {
		if quote.ItemID != current.ItemID ||
			quote.PurchasePrice <= 0 ||
			quote.SalePrice <= 0 ||
			quote.ObservedAt.IsZero() ||
			!quote.ObservedAt.Before(current.ObservedAt) ||
			quote.ObservedAt.Before(oldestAllowed) {
			continue
		}
		samples = append(samples, quote)
	}
	sort.Slice(samples, func(left, right int) bool {
		if !samples[left].ObservedAt.Equal(samples[right].ObservedAt) {
			return samples[left].ObservedAt.After(samples[right].ObservedAt)
		}
		if samples[left].PurchasePrice != samples[right].PurchasePrice {
			return samples[left].PurchasePrice > samples[right].PurchasePrice
		}
		return samples[left].SalePrice > samples[right].SalePrice
	})
	if len(samples) > marketMetricsHistoryLimit {
		samples = samples[:marketMetricsHistoryLimit]
	}

	distinctTimestamps := 0
	var previousTimestamp time.Time
	for _, quote := range samples {
		if previousTimestamp.IsZero() || !quote.ObservedAt.Equal(previousTimestamp) {
			distinctTimestamps++
			previousTimestamp = quote.ObservedAt
		}
	}
	if distinctTimestamps < marketMetricsMinimumHistory {
		return result
	}

	sort.Slice(samples, func(left, right int) bool {
		if !samples[left].ObservedAt.Equal(samples[right].ObservedAt) {
			return samples[left].ObservedAt.Before(samples[right].ObservedAt)
		}
		if samples[left].PurchasePrice != samples[right].PurchasePrice {
			return samples[left].PurchasePrice < samples[right].PurchasePrice
		}
		return samples[left].SalePrice < samples[right].SalePrice
	})
	samples = append(samples, current)

	volatility := 0.0
	for index := 1; index < len(samples); index++ {
		volatility = max(
			volatility,
			relativePriceChange(
				samples[index-1].PurchasePrice,
				samples[index].PurchasePrice,
			),
			relativePriceChange(
				samples[index-1].SalePrice,
				samples[index].SalePrice,
			),
		)
	}
	volatility = min(1, volatility)
	result.priceVolatility = volatility
	result.liquidityScore = marketMetricsLiquidityCap * (1 - volatility)
	result.resaleKnown = true
	return result
}

func relativePriceChange(left, right int64) float64 {
	if left <= 0 || right <= 0 {
		return 1
	}
	denominator := min(left, right)
	change := math.Abs(float64(left) - float64(right))
	return min(1, change/float64(denominator))
}

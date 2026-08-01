package automation

import (
	"strings"
	"testing"
	"time"

	"github.com/arena-trading-agent/arena-trading-agent/internal/domain"
)

func TestRequiredEconomicValueRejectsVLMProvenance(t *testing.T) {
	observation := domain.Observation{Values: map[string]domain.Value{
		valueBalance: {
			Raw: "100", Normalized: "100", Source: "VLM", Confidence: .99,
		},
	}}
	_, _, err := requiredInt(observation, valueBalance, 0, 1000)
	if err == nil || !strings.Contains(err.Error(), "не из OCR") {
		t.Fatalf("денежное значение VLM не было отклонено: %v", err)
	}
}

func TestRequiredEconomicValueAcceptsOCRConsensus(t *testing.T) {
	observation := domain.Observation{Values: map[string]domain.Value{
		valueBalance: {
			Raw: "100", Normalized: "100", Source: "OCR_CONSENSUS", Confidence: .99,
		},
	}}
	value, _, err := requiredInt(observation, valueBalance, 0, 1000)
	if err != nil || value != 100 {
		t.Fatalf("OCR consensus не был принят: value=%d err=%v", value, err)
	}
}

func TestRequiredIdentityUsesStrictCanonicalOCRText(t *testing.T) {
	t.Parallel()

	observation := domain.Observation{
		Confidence: .99,
		Values: map[string]domain.Value{
			valueItemName: {
				Raw:        "  Полевой   фильтр ",
				Normalized: "Полевой фильтр",
				Source:     "OCR_CONSENSUS",
				Confidence: .97,
			},
		},
	}
	confidence, err := requiredIdentity(
		observation,
		valueItemName,
		"полевой фильтр",
	)
	if err != nil || confidence != .97 {
		t.Fatalf("корректная видимая идентичность отклонена: confidence=%v err=%v", confidence, err)
	}
}

func TestRequiredIdentityRejectsDivergentRawAndNormalizedText(t *testing.T) {
	t.Parallel()

	observation := domain.Observation{Values: map[string]domain.Value{
		valueItemName: {
			Raw:        "Полевой фильтр",
			Normalized: "Военный фильтр",
			Source:     "OCR_CONSENSUS",
			Confidence: .99,
		},
	}}
	_, err := requiredIdentity(observation, valueItemName, "Полевой фильтр")
	if err == nil || !strings.Contains(err.Error(), "расходятся") {
		t.Fatalf("расходящиеся OCR-тексты идентичности не отклонены: %v", err)
	}
}

func TestTradeQuoteFromObservationDoesNotRequireAnalyticalMetricsFromOCR(t *testing.T) {
	observedAt := time.Date(2026, 7, 30, 10, 0, 0, 0, time.UTC)
	observation := domain.Observation{
		Confidence: 0.98,
		CreatedAt:  observedAt,
		Values: map[string]domain.Value{
			valuePurchasePrice:  {Raw: "100", Normalized: "100", Source: "OCR", Confidence: 0.97},
			valueSalePrice:      {Raw: "150", Normalized: "150", Source: "OCR", Confidence: 0.96},
			valueSaleCommission: {Raw: "15", Normalized: "15", Source: "OCR", Confidence: 0.95},
			valueListingFee:     {Raw: "5", Normalized: "5", Source: "OCR", Confidence: 0.94},
			// Эти значения не являются UI-фактами. Даже если адаптер модели
			// прислал их, построение денежной котировки должно их игнорировать.
			"liquidity_bps":        {Raw: "10000", Source: "VLM", Confidence: 1},
			"price_volatility_bps": {Raw: "0", Source: "VLM", Confidence: 1},
			"resale_known":         {Raw: "true", Source: "VLM", Confidence: 1},
		},
	}

	quote, err := tradeQuoteFromObservation("item", observation, 0.90)
	if err != nil {
		t.Fatalf("tradeQuoteFromObservation() вернул ошибку: %v", err)
	}
	if quote.PurchasePrice != 100 || quote.SalePrice != 150 ||
		quote.SaleCommission != 15 || quote.ListingFee != 5 {
		t.Fatalf("денежные поля котировки искажены: %#v", quote)
	}
	if quote.LiquidityScore != 0 || quote.PriceVolatility != 0 || quote.ResaleKnown {
		t.Fatalf("аналитические метрики были приняты из наблюдения: %#v", quote)
	}
	if quote.Confidence != 0.94 || !quote.ObservedAt.Equal(observedAt) {
		t.Fatalf("метаданные котировки некорректны: %#v", quote)
	}
}

package automation

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/arena-trading-agent/arena-trading-agent/internal/domain"
)

const maximumObservedMoney int64 = 1_000_000_000_000

const (
	valueBalance                 = "balance"
	valueFreeInventorySlots      = "free_inventory_slots"
	valueFreeMarketSlots         = "free_market_slots"
	valuePurchasePrice           = "purchase_price"
	valueSalePrice               = "sale_price"
	valueSaleCommission          = "sale_commission"
	valueListingFee              = "listing_fee"
	valueCooldownSeconds         = "cooldown_seconds"
	valueResultQuantity          = "result_quantity"
	valueMarketOrderID           = "market_order_id"
	valueOrderItemID             = "order_item_id"
	valueOrderStatus             = "order_status"
	valueOrderListedPrice        = "order_listed_price"
	valueOrderCurrentMarketPrice = "order_current_market_price"
	valueOrderAgeSeconds         = "order_age_seconds"
	valueSettledRevenue          = "settled_revenue"
	valueSettledFees             = "settled_fees"
	valueSoldQuantity            = "sold_quantity"
	valueItemName                = "item_name"
	valueContactName             = "contact_name"
	valueResultItemName          = "result_item_name"
)

// AccountSnapshot is the minimum synchronized state required before TRADE.
type AccountSnapshot struct {
	Balance            int64                    `json:"balance"`
	FreeInventorySlots int                      `json:"free_inventory_slots"`
	FreeMarketSlots    int                      `json:"free_market_slots"`
	Inventory          domain.InventorySnapshot `json:"inventory"`
	Confidence         float64                  `json:"confidence"`
	ObservedAt         time.Time                `json:"observed_at"`
}

type observedItem struct {
	Quantity   int64
	Slots      int
	Confidence float64
}

func inventoryQuantityKey(itemID string) string {
	return "inventory." + itemID + ".quantity"
}

func inventorySlotsKey(itemID string) string {
	return "inventory." + itemID + ".slots"
}

func ingredientNameKey(itemID string) string {
	return "ingredient." + itemID + ".name"
}

func requiredIdentity(
	observation domain.Observation,
	name string,
	expected string,
) (float64, error) {
	const methodCtx = "automation.requiredIdentity"

	value, exists := observation.Values[name]
	if !exists {
		return 0, fmt.Errorf(
			"%s: наблюдение не содержит обязательную видимую идентичность %q",
			methodCtx,
			name,
		)
	}
	if err := requireOCRSource(name, value); err != nil {
		return 0, fmt.Errorf("%s: источник видимой идентичности не прошёл проверку: %w", methodCtx, err)
	}
	expectedCanonical, err := canonicalIdentity(expected)
	if err != nil {
		return 0, fmt.Errorf(
			"%s: настроенная идентичность %q некорректна: %w",
			methodCtx,
			name,
			err,
		)
	}
	actualCanonical, err := canonicalObservedIdentity(name, value)
	if err != nil {
		return 0, fmt.Errorf(
			"%s: OCR-текст видимой идентичности не прошёл проверку: %w",
			methodCtx,
			err,
		)
	}
	if actualCanonical != expectedCanonical {
		return 0, fmt.Errorf(
			"%s: на экране %q=%q вместо настроенного %q",
			methodCtx,
			name,
			actualCanonical,
			expectedCanonical,
		)
	}
	if !finiteConfidence(value.Confidence) {
		return 0, fmt.Errorf(
			"%s: видимая идентичность %q имеет некорректную уверенность",
			methodCtx,
			name,
		)
	}
	return value.Confidence, nil
}

func canonicalObservedIdentity(name string, value domain.Value) (string, error) {
	const methodCtx = "automation.canonicalObservedIdentity"

	rawCanonical := ""
	if strings.TrimSpace(value.Raw) != "" {
		canonical, err := canonicalIdentity(value.Raw)
		if err != nil {
			return "", fmt.Errorf(
				"%s: исходный OCR-текст идентичности %q некорректен: %w",
				methodCtx,
				name,
				err,
			)
		}
		rawCanonical = canonical
	}
	normalizedCanonical := ""
	if strings.TrimSpace(value.Normalized) != "" {
		canonical, err := canonicalIdentity(value.Normalized)
		if err != nil {
			return "", fmt.Errorf(
				"%s: нормализованный OCR-текст идентичности %q некорректен: %w",
				methodCtx,
				name,
				err,
			)
		}
		normalizedCanonical = canonical
	}
	if rawCanonical == "" && normalizedCanonical == "" {
		return "", fmt.Errorf("%s: видимая идентичность %q пуста", methodCtx, name)
	}
	if rawCanonical != "" && normalizedCanonical != "" &&
		rawCanonical != normalizedCanonical {
		return "", fmt.Errorf(
			"%s: исходный и нормализованный OCR-текст идентичности %q расходятся: %q и %q",
			methodCtx,
			name,
			rawCanonical,
			normalizedCanonical,
		)
	}
	actualCanonical := normalizedCanonical
	if actualCanonical == "" {
		actualCanonical = rawCanonical
	}
	return actualCanonical, nil
}

func isVisibleIdentityKey(name string) bool {
	name = strings.ToLower(strings.TrimSpace(name))
	switch name {
	case valueItemName, valueContactName, valueResultItemName:
		return true
	default:
		return strings.HasPrefix(name, "ingredient.") &&
			strings.HasSuffix(name, ".name")
	}
}

func canonicalIdentity(value string) (string, error) {
	const methodCtx = "automation.canonicalIdentity"

	if !utf8.ValidString(value) {
		return "", fmt.Errorf("%s: текст содержит некорректный UTF-8", methodCtx)
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return "", fmt.Errorf("%s: текст пуст", methodCtx)
	}
	if utf8.RuneCountInString(value) > 256 {
		return "", fmt.Errorf("%s: текст длиннее 256 символов", methodCtx)
	}
	for _, character := range value {
		if character == unicode.ReplacementChar ||
			unicode.IsControl(character) ||
			unicode.Is(unicode.Cf, character) ||
			unicode.Is(unicode.Co, character) {
			return "", fmt.Errorf(
				"%s: текст содержит недопустимый символ U+%04X",
				methodCtx,
				character,
			)
		}
	}
	return strings.ToLower(strings.Join(strings.Fields(value), " ")), nil
}

func readObservedItem(
	observation domain.Observation,
	itemID string,
) (observedItem, error) {
	const methodCtx = "automation.readObservedItem"

	quantity, quantityConfidence, err := requiredInt(
		observation,
		inventoryQuantityKey(itemID),
		0,
		1_000_000_000,
	)
	if err != nil {
		return observedItem{}, fmt.Errorf("%s: не удалось прочитать количество предмета %q: %w", methodCtx, itemID, err)
	}
	slots, slotsConfidence, err := requiredInt(
		observation,
		inventorySlotsKey(itemID),
		0,
		1_000_000,
	)
	if err != nil {
		return observedItem{}, fmt.Errorf("%s: не удалось прочитать слоты предмета %q: %w", methodCtx, itemID, err)
	}
	if (quantity == 0) != (slots == 0) {
		return observedItem{}, fmt.Errorf(
			"%s: предмет инвентаря %q имеет несогласованные количество %d и число слотов %d",
			methodCtx,
			itemID,
			quantity,
			slots,
		)
	}
	return observedItem{
		Quantity: quantity, Slots: int(slots),
		Confidence: min(quantityConfidence, slotsConfidence),
	}, nil
}

func inventoryDelta(
	itemID string,
	before observedItem,
	after observedItem,
) (domain.InventoryDelta, error) {
	const methodCtx = "automation.inventoryDelta"

	quantity, err := checkedSubtract(after.Quantity, before.Quantity)
	if err != nil {
		return domain.InventoryDelta{}, fmt.Errorf("%s: не удалось вычислить изменение количества предмета %q: %w", methodCtx, itemID, err)
	}
	return domain.InventoryDelta{
		ItemID:        itemID,
		QuantityDelta: quantity,
		SlotsDelta:    after.Slots - before.Slots,
	}, nil
}

func tradeQuoteFromObservation(
	itemID string,
	observation domain.Observation,
	minConfidence float64,
) (domain.TradeQuote, error) {
	const methodCtx = "automation.tradeQuoteFromObservation"

	purchase, purchaseConfidence, err := requiredInt(observation, valuePurchasePrice, 0, maximumObservedMoney)
	if err != nil {
		return domain.TradeQuote{}, fmt.Errorf("%s: не удалось прочитать цену покупки: %w", methodCtx, err)
	}
	sale, saleConfidence, err := requiredInt(observation, valueSalePrice, 0, maximumObservedMoney)
	if err != nil {
		return domain.TradeQuote{}, fmt.Errorf("%s: не удалось прочитать цену продажи: %w", methodCtx, err)
	}
	commission, commissionConfidence, err := requiredInt(
		observation,
		valueSaleCommission,
		0,
		maximumObservedMoney,
	)
	if err != nil {
		return domain.TradeQuote{}, fmt.Errorf("%s: не удалось прочитать комиссию продажи: %w", methodCtx, err)
	}
	listingFee, listingConfidence, err := requiredInt(observation, valueListingFee, 0, maximumObservedMoney)
	if err != nil {
		return domain.TradeQuote{}, fmt.Errorf("%s: не удалось прочитать плату за выставление: %w", methodCtx, err)
	}
	confidence := min(
		observation.Confidence,
		purchaseConfidence,
		saleConfidence,
		commissionConfidence,
		listingConfidence,
	)
	if !finiteConfidence(confidence) || confidence < minConfidence {
		return domain.TradeQuote{}, fmt.Errorf(
			"%s: котировка %q имеет уверенность %.3f, требуется %.3f",
			methodCtx,
			itemID,
			confidence,
			minConfidence,
		)
	}
	if purchase <= 0 || sale <= 0 {
		return domain.TradeQuote{}, fmt.Errorf("%s: котировка %q содержит нулевую цену", methodCtx, itemID)
	}
	return domain.TradeQuote{
		ItemID: itemID, PurchasePrice: purchase, SalePrice: sale,
		SaleCommission: commission, ListingFee: listingFee,
		ObservedAt: observation.CreatedAt, Confidence: confidence,
	}, nil
}

func requiredInt(
	observation domain.Observation,
	name string,
	minimum int64,
	maximum int64,
) (int64, float64, error) {
	const methodCtx = "automation.requiredInt"

	value, exists := observation.Values[name]
	if !exists {
		return 0, 0, fmt.Errorf("%s: наблюдение не содержит обязательное значение %q", methodCtx, name)
	}
	if err := requireOCRSource(name, value); err != nil {
		return 0, 0, fmt.Errorf("%s: источник значения не прошёл проверку: %w", methodCtx, err)
	}
	normalized := strings.TrimSpace(value.Normalized)
	if normalized == "" {
		normalized = strings.TrimSpace(value.Raw)
	}
	normalized = strings.Map(func(character rune) rune {
		switch {
		case unicode.IsSpace(character), character == '_', character == ',':
			return -1
		default:
			return character
		}
	}, normalized)
	number, err := strconv.ParseInt(normalized, 10, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("%s: значение %q=%q не является целым числом", methodCtx, name, normalized)
	}
	if number < minimum || number > maximum {
		return 0, 0, fmt.Errorf(
			"%s: значение %q=%d вне диапазона [%d, %d]",
			methodCtx,
			name,
			number,
			minimum,
			maximum,
		)
	}
	if !finiteConfidence(value.Confidence) {
		return 0, 0, fmt.Errorf("%s: значение %q имеет некорректную уверенность", methodCtx, name)
	}
	return number, value.Confidence, nil
}

func requiredBool(observation domain.Observation, name string) (bool, float64, error) {
	const methodCtx = "automation.requiredBool"

	value, exists := observation.Values[name]
	if !exists {
		return false, 0, fmt.Errorf("%s: наблюдение не содержит обязательное значение %q", methodCtx, name)
	}
	if err := requireOCRSource(name, value); err != nil {
		return false, 0, fmt.Errorf("%s: источник значения не прошёл проверку: %w", methodCtx, err)
	}
	normalized := strings.ToLower(strings.TrimSpace(value.Normalized))
	if normalized == "" {
		normalized = strings.ToLower(strings.TrimSpace(value.Raw))
	}
	var result bool
	switch normalized {
	case "1", "true", "yes", "да", "known":
		result = true
	case "0", "false", "no", "нет", "unknown":
		result = false
	default:
		return false, 0, fmt.Errorf("%s: значение %q=%q не является логическим", methodCtx, name, normalized)
	}
	if !finiteConfidence(value.Confidence) {
		return false, 0, fmt.Errorf("%s: значение %q имеет некорректную уверенность", methodCtx, name)
	}
	return result, value.Confidence, nil
}

func requireOCRSource(name string, value domain.Value) error {
	const methodCtx = "automation.requireOCRSource"

	switch strings.ToUpper(strings.TrimSpace(value.Source)) {
	case "OCR", "OCR_CONSENSUS":
		return nil
	default:
		return fmt.Errorf(
			"%s: критичное значение %q получено не из OCR, источник=%q",
			methodCtx,
			name,
			value.Source,
		)
	}
}

func finiteConfidence(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value >= 0 && value <= 1
}

func checkedSubtract(left, right int64) (int64, error) {
	const methodCtx = "automation.checkedSubtract"

	if right > 0 && left < math.MinInt64+right {
		return 0, fmt.Errorf("%s: выход ниже диапазона int64", methodCtx)
	}
	if right < 0 && left > math.MaxInt64+right {
		return 0, fmt.Errorf("%s: переполнение int64", methodCtx)
	}
	return left - right, nil
}

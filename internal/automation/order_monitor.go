package automation

import (
	"fmt"
	"strings"
	"time"

	"github.com/arena-trading-agent/arena-trading-agent/internal/domain"
	"github.com/arena-trading-agent/arena-trading-agent/internal/trading"
)

const (
	orderSnapshotSchemaVersion = 1
	orderSnapshotRecordKind    = "order_snapshot"
	maximumOrderAgeSeconds     = int64((365 * 24 * time.Hour) / time.Second)

	orderStatusActive  = "ACTIVE"
	orderStatusListed  = "LISTED"
	orderStatusPending = "PENDING"
	orderStatusSold    = "SOLD"
	orderStatusSettled = "SETTLED"
)

// OrderSnapshot — ограниченный production-снимок одного конкретного ордера.
// Он содержит только проверенные OCR-факты и детерминированную рекомендацию.
// AutomaticRepriceAllowed всегда false: перевыставление требует отдельной
// саги, денежного события и полного аудита и поэтому здесь не выполняется.
type OrderSnapshot struct {
	SchemaVersion           int       `json:"schema_version"`
	SagaID                  string    `json:"saga_id"`
	MarketOrderID           string    `json:"market_order_id"`
	ItemID                  string    `json:"item_id"`
	Status                  string    `json:"status"`
	ListedPrice             int64     `json:"listed_price"`
	CurrentMarketPrice      int64     `json:"current_market_price"`
	MinimumAllowedPrice     int64     `json:"minimum_allowed_price"`
	ObservedFrameID         uint64    `json:"observed_frame_id"`
	ObservedAt              time.Time `json:"observed_at"`
	ListedAt                time.Time `json:"listed_at"`
	AgeSeconds              int64     `json:"age_seconds"`
	SlotOccupied            bool      `json:"slot_occupied"`
	RepriceRecommended      bool      `json:"reprice_recommended"`
	RecommendedPrice        int64     `json:"recommended_price"`
	RecommendationOnly      bool      `json:"recommendation_only"`
	AutomaticRepriceAllowed bool      `json:"automatic_reprice_allowed"`
	RecommendationReason    string    `json:"recommendation_reason"`
	Confidence              float64   `json:"confidence"`
}

func orderSnapshotKey(sagaID string) string {
	return "order/" + sagaID
}

func (r *TradeRunner) orderSnapshotFromObservation(
	saga trading.Saga,
	observation domain.Observation,
) (OrderSnapshot, error) {
	const methodCtx = "automation.TradeRunner.orderSnapshotFromObservation"

	if strings.TrimSpace(saga.ID) == "" ||
		strings.TrimSpace(saga.MarketOrderID) == "" ||
		strings.TrimSpace(saga.Opportunity.ResultItemID) == "" {
		return OrderSnapshot{}, fmt.Errorf("%s: сага не содержит полную идентичность ордера", methodCtx)
	}
	if saga.Status != trading.SagaAwaitingSale {
		return OrderSnapshot{}, fmt.Errorf(
			"%s: сага %q имеет статус %q вместо AWAITING_SALE",
			methodCtx,
			saga.ID,
			saga.Status,
		)
	}
	if observation.FrameID == 0 || observation.CreatedAt.IsZero() {
		return OrderSnapshot{}, fmt.Errorf("%s: наблюдение ордера не содержит кадр или время", methodCtx)
	}
	orderConfidence, err := requiredExactText(
		observation,
		valueMarketOrderID,
		saga.MarketOrderID,
	)
	if err != nil {
		return OrderSnapshot{}, fmt.Errorf("%s: идентификатор ордера не прошёл проверку: %w", methodCtx, err)
	}
	itemConfidence, err := requiredExactText(
		observation,
		valueOrderItemID,
		saga.Opportunity.ResultItemID,
	)
	if err != nil {
		return OrderSnapshot{}, fmt.Errorf("%s: идентификатор предмета ордера не прошёл проверку: %w", methodCtx, err)
	}
	status, statusConfidence, err := requiredText(observation, valueOrderStatus)
	if err != nil {
		return OrderSnapshot{}, fmt.Errorf("%s: не удалось прочитать состояние ордера: %w", methodCtx, err)
	}
	status = strings.ToUpper(strings.TrimSpace(status))
	slotOccupied, err := orderSlotOccupied(status)
	if err != nil {
		return OrderSnapshot{}, fmt.Errorf("%s: состояние ордера не поддерживается: %w", methodCtx, err)
	}
	listedPrice, listedConfidence, err := requiredInt(
		observation,
		valueOrderListedPrice,
		1,
		maximumObservedMoney,
	)
	if err != nil {
		return OrderSnapshot{}, fmt.Errorf("%s: не удалось прочитать цену нашего ордера: %w", methodCtx, err)
	}
	currentPrice, currentConfidence, err := requiredInt(
		observation,
		valueOrderCurrentMarketPrice,
		1,
		maximumObservedMoney,
	)
	if err != nil {
		return OrderSnapshot{}, fmt.Errorf("%s: не удалось прочитать текущую рыночную цену: %w", methodCtx, err)
	}
	ageSeconds, ageConfidence, err := requiredInt(
		observation,
		valueOrderAgeSeconds,
		0,
		maximumOrderAgeSeconds,
	)
	if err != nil {
		return OrderSnapshot{}, fmt.Errorf("%s: не удалось прочитать возраст ордера: %w", methodCtx, err)
	}
	minimumAllowedPrice, err := orderMinimumAllowedPrice(saga)
	if err != nil {
		return OrderSnapshot{}, fmt.Errorf("%s: не удалось определить ценовой предел плана: %w", methodCtx, err)
	}
	confidence := min(
		observation.Confidence,
		orderConfidence,
		itemConfidence,
		statusConfidence,
		listedConfidence,
		currentConfidence,
		ageConfidence,
	)
	if err := r.requireConfidence(confidence); err != nil {
		return OrderSnapshot{}, fmt.Errorf("%s: снимок ордера имеет недостаточную уверенность: %w", methodCtx, err)
	}

	recommended, recommendedPrice, reason := orderRepriceRecommendation(
		status,
		listedPrice,
		currentPrice,
		minimumAllowedPrice,
	)
	value := OrderSnapshot{
		SchemaVersion:           orderSnapshotSchemaVersion,
		SagaID:                  saga.ID,
		MarketOrderID:           saga.MarketOrderID,
		ItemID:                  saga.Opportunity.ResultItemID,
		Status:                  status,
		ListedPrice:             listedPrice,
		CurrentMarketPrice:      currentPrice,
		MinimumAllowedPrice:     minimumAllowedPrice,
		ObservedFrameID:         observation.FrameID,
		ObservedAt:              observation.CreatedAt,
		ListedAt:                observation.CreatedAt.Add(-time.Duration(ageSeconds) * time.Second),
		AgeSeconds:              ageSeconds,
		SlotOccupied:            slotOccupied,
		RepriceRecommended:      recommended,
		RecommendedPrice:        recommendedPrice,
		RecommendationOnly:      true,
		AutomaticRepriceAllowed: false,
		RecommendationReason:    reason,
		Confidence:              confidence,
	}
	if err := validateOrderSnapshot(value, &saga); err != nil {
		return OrderSnapshot{}, fmt.Errorf("%s: построенный снимок ордера некорректен: %w", methodCtx, err)
	}
	return value, nil
}

func requiredExactText(
	observation domain.Observation,
	name string,
	expected string,
) (float64, error) {
	const methodCtx = "automation.requiredExactText"

	value, exists := observation.Values[name]
	if !exists {
		return 0, fmt.Errorf("%s: наблюдение не содержит обязательное значение %q", methodCtx, name)
	}
	if err := requireOCRSource(name, value); err != nil {
		return 0, fmt.Errorf("%s: источник точной идентичности не прошёл проверку: %w", methodCtx, err)
	}
	raw := strings.TrimSpace(value.Raw)
	normalized := strings.TrimSpace(value.Normalized)
	if raw == "" && normalized == "" {
		return 0, fmt.Errorf("%s: точная идентичность %q пуста", methodCtx, name)
	}
	if raw != "" && normalized != "" && raw != normalized {
		return 0, fmt.Errorf(
			"%s: исходная и нормализованная идентичность %q расходятся: %q и %q",
			methodCtx,
			name,
			raw,
			normalized,
		)
	}
	actual := normalized
	if actual == "" {
		actual = raw
	}
	expected = strings.TrimSpace(expected)
	if actual != expected {
		return 0, fmt.Errorf(
			"%s: значение %q равно %q вместо ожидаемого %q",
			methodCtx,
			name,
			actual,
			expected,
		)
	}
	if !finiteConfidence(value.Confidence) {
		return 0, fmt.Errorf("%s: значение %q имеет некорректную уверенность", methodCtx, name)
	}
	return value.Confidence, nil
}

func orderMinimumAllowedPrice(saga trading.Saga) (int64, error) {
	const methodCtx = "automation.orderMinimumAllowedPrice"

	var listing *domain.TradeStep
	for index := range saga.Opportunity.Steps {
		step := &saga.Opportunity.Steps[index]
		if step.Kind != domain.TradeStepList {
			continue
		}
		if listing != nil {
			return 0, fmt.Errorf("%s: торговый план содержит более одного шага LIST", methodCtx)
		}
		listing = step
	}
	if listing == nil {
		return 0, fmt.Errorf("%s: торговый план не содержит шага LIST", methodCtx)
	}
	if listing.ItemID != saga.Opportunity.ResultItemID ||
		listing.Quantity <= 0 ||
		listing.LimitPrice <= 0 {
		return 0, fmt.Errorf(
			"%s: шаг LIST не соответствует результату сделки или ценовому пределу",
			methodCtx,
		)
	}
	return listing.LimitPrice, nil
}

func orderSlotOccupied(status string) (bool, error) {
	const methodCtx = "automation.orderSlotOccupied"

	switch status {
	case orderStatusActive, orderStatusListed, orderStatusPending:
		return true, nil
	case orderStatusSold, orderStatusSettled:
		return false, nil
	default:
		return false, fmt.Errorf("%s: неизвестное состояние %q", methodCtx, status)
	}
}

func orderRepriceRecommendation(
	status string,
	listedPrice int64,
	currentPrice int64,
	minimumAllowedPrice int64,
) (bool, int64, string) {
	const methodCtx = "automation.orderRepriceRecommendation"

	if status != orderStatusActive && status != orderStatusListed {
		return false, 0, methodCtx + ": состояние ордера не допускает рекомендацию перевыставления"
	}
	if currentPrice >= listedPrice {
		return false, 0, methodCtx + ": текущая рыночная цена не ниже цены нашего ордера"
	}
	if currentPrice < minimumAllowedPrice {
		return false, 0, methodCtx + ": текущая рыночная цена ниже минимальной цены торгового плана"
	}
	return true, currentPrice, methodCtx + ": рекомендуется оператору проверить перевыставление; автоматическое действие запрещено"
}

func validateOrderSnapshot(value OrderSnapshot, saga *trading.Saga) error {
	const methodCtx = "automation.validateOrderSnapshot"

	if value.SchemaVersion != orderSnapshotSchemaVersion {
		return fmt.Errorf("%s: версия схемы снимка %d не поддерживается", methodCtx, value.SchemaVersion)
	}
	if strings.TrimSpace(value.SagaID) == "" ||
		strings.TrimSpace(value.MarketOrderID) == "" ||
		strings.TrimSpace(value.ItemID) == "" {
		return fmt.Errorf("%s: снимок не содержит полную идентичность", methodCtx)
	}
	if saga != nil &&
		(value.SagaID != saga.ID ||
			value.MarketOrderID != saga.MarketOrderID ||
			value.ItemID != saga.Opportunity.ResultItemID) {
		return fmt.Errorf("%s: снимок относится к другой саге, ордеру или предмету", methodCtx)
	}
	if saga != nil {
		minimumAllowedPrice, err := orderMinimumAllowedPrice(*saga)
		if err != nil {
			return fmt.Errorf("%s: ценовой предел саги некорректен: %w", methodCtx, err)
		}
		if value.MinimumAllowedPrice != minimumAllowedPrice {
			return fmt.Errorf(
				"%s: минимальная цена снимка %d не совпадает с пределом саги %d",
				methodCtx,
				value.MinimumAllowedPrice,
				minimumAllowedPrice,
			)
		}
	}
	slotOccupied, err := orderSlotOccupied(value.Status)
	if err != nil {
		return fmt.Errorf("%s: некорректное состояние снимка: %w", methodCtx, err)
	}
	if value.SlotOccupied != slotOccupied {
		return fmt.Errorf("%s: занятость слота не соответствует состоянию ордера", methodCtx)
	}
	if value.ListedPrice <= 0 ||
		value.CurrentMarketPrice <= 0 ||
		value.MinimumAllowedPrice <= 0 {
		return fmt.Errorf("%s: снимок содержит некорректную цену", methodCtx)
	}
	if value.ObservedFrameID == 0 ||
		value.ObservedAt.IsZero() ||
		value.ListedAt.IsZero() ||
		value.AgeSeconds < 0 ||
		value.AgeSeconds > maximumOrderAgeSeconds ||
		!value.ListedAt.Equal(value.ObservedAt.Add(-time.Duration(value.AgeSeconds)*time.Second)) {
		return fmt.Errorf("%s: время снимка или возраст ордера некорректны", methodCtx)
	}
	if !finiteConfidence(value.Confidence) {
		return fmt.Errorf("%s: уверенность снимка некорректна", methodCtx)
	}
	recommended, price, reason := orderRepriceRecommendation(
		value.Status,
		value.ListedPrice,
		value.CurrentMarketPrice,
		value.MinimumAllowedPrice,
	)
	if value.RepriceRecommended != recommended ||
		value.RecommendedPrice != price ||
		value.RecommendationReason != reason ||
		!value.RecommendationOnly ||
		value.AutomaticRepriceAllowed {
		return fmt.Errorf("%s: рекомендация перевыставления не соответствует безопасной формуле", methodCtx)
	}
	return nil
}

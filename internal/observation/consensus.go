package observation

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"unicode"

	"github.com/arena-trading-agent/arena-trading-agent/internal/domain"
	"github.com/arena-trading-agent/arena-trading-agent/internal/protocol"
)

const maxOCRConsensusAttempts = 3

// OCRConsensusPolicy задаёт ограниченную политику повторного OCR-чтения.
//
// При принятом большинстве итоговая уверенность равна минимальной уверенности
// согласившихся чтений, умноженной на долю согласия. Поэтому расхождение 2 из 3
// даёт confidence не выше 0.667 и штатно блокируется торговым min_confidence.
// FailOnDisagreement включает более строгий режим: любое расхождение является
// ошибкой наблюдения.
type OCRConsensusPolicy struct {
	Attempts           int
	MinimumAgreement   int
	FailOnDisagreement bool
}

// DefaultOCRConsensusPolicy возвращает безопасную ограниченную политику.
func DefaultOCRConsensusPolicy() OCRConsensusPolicy {
	return OCRConsensusPolicy{
		Attempts:         maxOCRConsensusAttempts,
		MinimumAgreement: 2,
	}
}

// Validate проверяет политику и жёсткий лимит попыток.
func (p OCRConsensusPolicy) Validate() error {
	const methodCtx = "observation.OCRConsensusPolicy.Validate"

	if p.Attempts < 1 || p.Attempts > maxOCRConsensusAttempts {
		return fmt.Errorf(
			"%s: число попыток должно находиться в диапазоне 1..%d",
			methodCtx,
			maxOCRConsensusAttempts,
		)
	}
	if p.MinimumAgreement < 1 || p.MinimumAgreement > p.Attempts {
		return fmt.Errorf(
			"%s: минимальное число согласованных чтений должно находиться в диапазоне 1..%d",
			methodCtx,
			p.Attempts,
		)
	}
	return nil
}

func (o *Observer) readOCRWithConsensus(
	ctx context.Context,
	frame protocol.Frame,
	regions map[string]domain.Rectangle,
) (map[string]domain.Value, error) {
	const methodCtx = "observation.Observer.readOCRWithConsensus"

	if err := o.policy.Validate(); err != nil {
		return nil, fmt.Errorf("%s: политика OCR consensus не прошла проверку: %w", methodCtx, err)
	}
	critical := criticalRegions(regions)
	if len(critical) == 0 || o.policy.Attempts == 1 {
		values, err := o.ocr.Read(ctx, frame, regions)
		if err != nil {
			return nil, fmt.Errorf("%s: единственное чтение OCR завершилось ошибкой: %w", methodCtx, err)
		}
		return values, nil
	}

	samples, err := o.readOCRSamples(ctx, frame, regions, o.policy.Attempts)
	if err != nil {
		return nil, fmt.Errorf("%s: не удалось собрать повторные чтения OCR: %w", methodCtx, err)
	}
	if len(samples) != o.policy.Attempts {
		return nil, fmt.Errorf(
			"%s: OCR вернул %d попыток вместо %d",
			methodCtx,
			len(samples),
			o.policy.Attempts,
		)
	}

	result := make(map[string]domain.Value, len(samples[0]))
	for name, value := range samples[0] {
		result[name] = value
	}
	for name := range critical {
		value, err := aggregateCriticalValue(name, samples, o.policy)
		if err != nil {
			return nil, fmt.Errorf(
				"%s: критичное значение %q не прошло OCR consensus: %w",
				methodCtx,
				name,
				err,
			)
		}
		result[name] = value
	}
	return result, nil
}

func (o *Observer) readOCRSamples(
	ctx context.Context,
	frame protocol.Frame,
	regions map[string]domain.Rectangle,
	attempts int,
) ([]map[string]domain.Value, error) {
	const methodCtx = "observation.Observer.readOCRSamples"

	if repeated, ok := o.ocr.(OCRRepeatService); ok {
		samples, err := repeated.ReadRepeated(ctx, frame, regions, attempts)
		if err != nil {
			return nil, fmt.Errorf("%s: пакетное повторное чтение завершилось ошибкой: %w", methodCtx, err)
		}
		return samples, nil
	}

	samples := make([]map[string]domain.Value, 0, attempts)
	for attempt := 1; attempt <= attempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf(
				"%s: контекст завершён перед попыткой %d: %w",
				methodCtx,
				attempt,
				err,
			)
		}
		values, err := o.ocr.Read(ctx, frame, regions)
		if err != nil {
			return nil, fmt.Errorf(
				"%s: попытка %d завершилась ошибкой: %w",
				methodCtx,
				attempt,
				err,
			)
		}
		samples = append(samples, values)
	}
	return samples, nil
}

type consensusGroup struct {
	count          int
	minConfidence  float64
	representative domain.Value
}

func aggregateCriticalValue(
	name string,
	samples []map[string]domain.Value,
	policy OCRConsensusPolicy,
) (domain.Value, error) {
	const methodCtx = "observation.aggregateCriticalValue"

	groups := make(map[string]consensusGroup, len(samples))
	for attempt, sample := range samples {
		value, exists := sample[name]
		if !exists {
			continue
		}
		if strings.TrimSpace(value.Source) == "" ||
			(strings.TrimSpace(value.Raw) == "" && strings.TrimSpace(value.Normalized) == "") ||
			!validConfidence(value.Confidence) ||
			!validRectangle(value.Region) {
			return domain.Value{}, fmt.Errorf(
				"%s: попытка %d вернула некорректное значение",
				methodCtx,
				attempt+1,
			)
		}
		key, normalized := canonicalConsensusValue(name, value)
		if key == "" {
			return domain.Value{}, fmt.Errorf(
				"%s: попытка %d не содержит нормализуемого текста",
				methodCtx,
				attempt+1,
			)
		}
		group, exists := groups[key]
		if !exists {
			value.Normalized = normalized
			group = consensusGroup{
				minConfidence:  value.Confidence,
				representative: value,
			}
		}
		group.count++
		if value.Confidence < group.minConfidence {
			group.minConfidence = value.Confidence
		}
		if value.Confidence > group.representative.Confidence {
			value.Normalized = normalized
			group.representative = value
		}
		groups[key] = group
	}

	var winner consensusGroup
	bestGroups := 0
	for _, group := range groups {
		switch {
		case group.count > winner.count:
			winner = group
			bestGroups = 1
		case group.count == winner.count:
			bestGroups++
		}
	}
	if winner.count < policy.MinimumAgreement || bestGroups != 1 {
		return domain.Value{}, fmt.Errorf(
			"%s: согласованы только %d из %d чтений, требуется %d",
			methodCtx,
			winner.count,
			len(samples),
			policy.MinimumAgreement,
		)
	}
	if policy.FailOnDisagreement && winner.count != len(samples) {
		return domain.Value{}, fmt.Errorf(
			"%s: строгая политика отклонила расхождение %d из %d чтений",
			methodCtx,
			winner.count,
			len(samples),
		)
	}

	result := winner.representative
	result.Source = "OCR_CONSENSUS"
	result.Confidence = winner.minConfidence * float64(winner.count) / float64(len(samples))
	return result, nil
}

func canonicalConsensusValue(name string, value domain.Value) (string, string) {
	text := strings.TrimSpace(value.Normalized)
	if text == "" {
		text = strings.TrimSpace(value.Raw)
	}
	if isNumericCriticalValue(name) {
		compact := strings.Map(func(character rune) rune {
			switch {
			case unicode.IsSpace(character):
				return -1
			case character == '_', character == ',', character == '.', character == '\'', character == '’':
				return -1
			default:
				return character
			}
		}, text)
		if number, err := strconv.ParseInt(compact, 10, 64); err == nil {
			normalized := strconv.FormatInt(number, 10)
			return normalized, normalized
		}
	}
	normalized := strings.Join(strings.Fields(text), " ")
	return strings.ToLower(normalized), normalized
}

func criticalRegions(
	regions map[string]domain.Rectangle,
) map[string]domain.Rectangle {
	result := make(map[string]domain.Rectangle)
	for name, region := range regions {
		if isCriticalValue(name) {
			result[name] = region
		}
	}
	return result
}

func isCriticalValue(name string) bool {
	name = strings.ToLower(strings.TrimSpace(name))
	switch name {
	case "price",
		"balance",
		"free_inventory_slots",
		"free_market_slots",
		"purchase_price",
		"sale_price",
		"sale_commission",
		"listing_fee",
		"cooldown_seconds",
		"result_quantity",
		"market_order_id",
		"order_status",
		"settled_revenue",
		"settled_fees",
		"sold_quantity",
		"sold_item_id",
		"item_name",
		"contact_name",
		"result_item_name":
		return true
	}
	return strings.HasPrefix(name, "inventory.") &&
		(strings.HasSuffix(name, ".quantity") || strings.HasSuffix(name, ".slots")) ||
		strings.HasPrefix(name, "ingredient.") &&
			(strings.HasSuffix(name, ".quantity") || strings.HasSuffix(name, ".name"))
}

func isNumericCriticalValue(name string) bool {
	name = strings.ToLower(strings.TrimSpace(name))
	switch name {
	case "order_status",
		"sold_item_id",
		"item_name",
		"contact_name",
		"result_item_name":
		return false
	default:
		return isCriticalValue(name) && !strings.HasSuffix(name, ".name")
	}
}

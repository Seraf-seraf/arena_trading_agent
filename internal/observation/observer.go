// Package observation нормализует результаты локального vision, OCR и VLM.
package observation

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/arena-trading-agent/arena-trading-agent/internal/domain"
	"github.com/arena-trading-agent/arena-trading-agent/internal/protocol"
)

const (
	maxObservationElements = 8
	maxObservationValues   = 128
)

// LocalDetector распознаёт известные экраны и возвращает заданные ROI.
type LocalDetector interface {
	Detect(context.Context, protocol.Frame) (domain.ScreenState, float64, map[string]domain.Rectangle, error)
}

// OCRService читает значения в заданных областях известного экрана.
type OCRService interface {
	Read(context.Context, protocol.Frame, map[string]domain.Rectangle) (map[string]domain.Value, error)
}

// OCRRepeatService — необязательное расширение OCRService. Реализация может
// переиспользовать сериализованный кадр и тем самым не создавать крупный
// base64-буфер для каждой попытки consensus.
type OCRRepeatService interface {
	ReadRepeated(
		context.Context,
		protocol.Frame,
		map[string]domain.Rectangle,
		int,
	) ([]map[string]domain.Value, error)
}

// VLMService выполняет семантический разбор неизвестного экрана.
type VLMService interface {
	Ground(context.Context, protocol.Frame) (domain.Observation, error)
}

// Observer выбирает локальный или модельный путь анализа кадра.
type Observer struct {
	local  LocalDetector
	ocr    OCRService
	vlm    VLMService
	policy OCRConsensusPolicy
}

// New создаёт конвейер наблюдения.
func New(local LocalDetector, ocr OCRService, vlm VLMService) *Observer {
	return &Observer{
		local:  local,
		ocr:    ocr,
		vlm:    vlm,
		policy: DefaultOCRConsensusPolicy(),
	}
}

// NewWithOCRConsensusPolicy создаёт конвейер с явно заданной политикой
// повторного чтения критичных OCR-значений.
func NewWithOCRConsensusPolicy(
	local LocalDetector,
	ocr OCRService,
	vlm VLMService,
	policy OCRConsensusPolicy,
) (*Observer, error) {
	const methodCtx = "observation.NewWithOCRConsensusPolicy"

	if err := policy.Validate(); err != nil {
		return nil, fmt.Errorf("%s: некорректная политика OCR consensus: %w", methodCtx, err)
	}
	return &Observer{local: local, ocr: ocr, vlm: vlm, policy: policy}, nil
}

// Observe превращает кадр в проверенное наблюдение.
func (o *Observer) Observe(ctx context.Context, frame protocol.Frame) (domain.Observation, error) {
	const methodCtx = "observation.Observer.Observe"

	if ctx == nil {
		return domain.Observation{}, fmt.Errorf("%s: контекст наблюдения не задан", methodCtx)
	}
	if err := ctx.Err(); err != nil {
		return domain.Observation{}, fmt.Errorf("%s: контекст наблюдения завершён: %w", methodCtx, err)
	}
	if o == nil || o.local == nil {
		return domain.Observation{}, fmt.Errorf("%s: локальный детектор экранов не настроен", methodCtx)
	}
	if frame.ID == 0 {
		return domain.Observation{}, fmt.Errorf("%s: идентификатор кадра не задан", methodCtx)
	}
	if len(frame.Data) == 0 {
		return domain.Observation{}, fmt.Errorf("%s: кадр %d не содержит изображения", methodCtx, frame.ID)
	}
	state, confidence, regions, err := o.local.Detect(ctx, frame)
	if err != nil {
		return domain.Observation{}, fmt.Errorf(
			"%s: не удалось локально определить экран: %w",
			methodCtx,
			err,
		)
	}
	if !validState(state) {
		return domain.Observation{}, fmt.Errorf(
			"%s: локальный детектор вернул неизвестное состояние %q",
			methodCtx,
			state,
		)
	}
	if !validConfidence(confidence) {
		return domain.Observation{}, fmt.Errorf(
			"%s: локальный детектор вернул некорректную уверенность",
			methodCtx,
		)
	}
	if state == domain.StateUnknown {
		if o.vlm == nil {
			return domain.Observation{}, fmt.Errorf("%s: адаптер VLM не настроен", methodCtx)
		}
		observation, err := o.vlm.Ground(ctx, frame)
		if err != nil {
			return domain.Observation{}, fmt.Errorf(
				"%s: не удалось распознать неизвестный экран через VLM: %w",
				methodCtx,
				err,
			)
		}
		for name, value := range observation.Values {
			if !strings.HasPrefix(strings.ToUpper(strings.TrimSpace(value.Source)), "VLM") {
				value.Source = "VLM"
				observation.Values[name] = value
			}
		}
		observation.FrameID = frame.ID
		observation.CreatedAt = time.Now().UTC()
		validated, err := validate(observation)
		if err != nil {
			return domain.Observation{}, fmt.Errorf(
				"%s: наблюдение неизвестного экрана не прошло проверку: %w",
				methodCtx,
				err,
			)
		}
		// VLM помогает классифицировать и объяснять неизвестный экран, но его
		// вывод никогда не должен авторизовать ввод или денежное решение.
		// Ограничиваем confidence только после проверки исходного ответа, чтобы
		// NaN/Inf не превращались в допустимое значение при нормализации.
		validated.Confidence = min(validated.Confidence, .5)
		return validated, nil
	}
	if o.ocr == nil {
		return domain.Observation{}, fmt.Errorf("%s: адаптер OCR не настроен", methodCtx)
	}
	if len(regions) > maxObservationValues {
		return domain.Observation{}, fmt.Errorf(
			"%s: локальный детектор вернул слишком много областей OCR: %d",
			methodCtx,
			len(regions),
		)
	}
	for name, region := range regions {
		if strings.TrimSpace(name) == "" || !validRectangle(region) {
			return domain.Observation{}, fmt.Errorf(
				"%s: локальный детектор вернул некорректную область OCR %q",
				methodCtx,
				name,
			)
		}
	}
	values, err := o.readOCRWithConsensus(ctx, frame, regions)
	if err != nil {
		return domain.Observation{}, fmt.Errorf(
			"%s: не удалось прочитать известный экран через OCR: %w",
			methodCtx,
			err,
		)
	}
	validated, err := validate(domain.Observation{
		FrameID:    frame.ID,
		State:      state,
		Values:     values,
		Confidence: confidence,
		CreatedAt:  time.Now().UTC(),
	})
	if err != nil {
		return domain.Observation{}, fmt.Errorf(
			"%s: наблюдение известного экрана не прошло проверку: %w",
			methodCtx,
			err,
		)
	}
	return validated, nil
}

func validate(observation domain.Observation) (domain.Observation, error) {
	const methodCtx = "observation.validate"

	if observation.FrameID == 0 {
		return domain.Observation{}, fmt.Errorf("%s: идентификатор кадра не задан", methodCtx)
	}
	if observation.CreatedAt.IsZero() {
		return domain.Observation{}, fmt.Errorf("%s: время создания наблюдения не задано", methodCtx)
	}
	if !validState(observation.State) {
		return domain.Observation{}, fmt.Errorf(
			"%s: сервис наблюдения вернул неизвестное состояние %q",
			methodCtx,
			observation.State,
		)
	}
	if !validConfidence(observation.Confidence) {
		return domain.Observation{}, fmt.Errorf(
			"%s: сервис наблюдения вернул некорректную уверенность",
			methodCtx,
		)
	}
	if len(observation.Elements) > maxObservationElements {
		return domain.Observation{}, fmt.Errorf(
			"%s: сервис наблюдения вернул слишком много элементов интерфейса: %d",
			methodCtx,
			len(observation.Elements),
		)
	}
	for index, element := range observation.Elements {
		if strings.TrimSpace(element.Kind) == "" ||
			strings.TrimSpace(element.Label) == "" ||
			!validConfidence(element.Confidence) ||
			!validRectangle(element.Region) {
			return domain.Observation{}, fmt.Errorf(
				"%s: элемент интерфейса %d содержит некорректные данные",
				methodCtx,
				index,
			)
		}
	}
	if len(observation.Values) > maxObservationValues {
		return domain.Observation{}, fmt.Errorf(
			"%s: сервис наблюдения вернул слишком много значений: %d",
			methodCtx,
			len(observation.Values),
		)
	}
	for name, value := range observation.Values {
		if strings.TrimSpace(name) == "" ||
			strings.TrimSpace(value.Source) == "" ||
			(strings.TrimSpace(value.Raw) == "" && strings.TrimSpace(value.Normalized) == "") ||
			!validConfidence(value.Confidence) ||
			!validRectangle(value.Region) {
			return domain.Observation{}, fmt.Errorf(
				"%s: значение %q содержит некорректные данные или область происхождения",
				methodCtx,
				name,
			)
		}
	}
	return observation, nil
}

func validState(state domain.ScreenState) bool {
	switch state {
	case domain.StateUnknown, domain.StateMainMenu, domain.StateMarketHome,
		domain.StateMarketSearch, domain.StateMarketResults, domain.StateItemCard,
		domain.StatePurchaseDialog, domain.StateContacts, domain.StateContactPage,
		domain.StateContactBarter, domain.StateBarterCard, domain.StateInventory,
		domain.StateSaleDialog, domain.StateConfirmation, domain.StateErrorDialog:
		return true
	default:
		return false
	}
}

func validConfidence(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value >= 0 && value <= 1
}

func validRectangle(value domain.Rectangle) bool {
	return finite(value.X) && finite(value.Y) && finite(value.Width) && finite(value.Height) &&
		value.X >= 0 && value.Y >= 0 && value.Width > 0 && value.Height > 0 &&
		value.X+value.Width <= 1 && value.Y+value.Height <= 1
}

func finite(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0)
}

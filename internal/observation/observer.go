// Package observation нормализует результаты локального vision, OCR и VLM.
package observation

import (
	"context"
	"fmt"
	"time"

	"github.com/arena-trading-agent/arena-trading-agent/internal/domain"
	"github.com/arena-trading-agent/arena-trading-agent/internal/protocol"
)

// LocalDetector распознаёт известные экраны и возвращает заданные ROI.
type LocalDetector interface {
	Detect(context.Context, protocol.Frame) (domain.ScreenState, float64, map[string]domain.Rectangle, error)
}

// OCRService читает значения в заданных областях известного экрана.
type OCRService interface {
	Read(context.Context, protocol.Frame, map[string]domain.Rectangle) (map[string]domain.Value, error)
}

// VLMService выполняет семантический разбор неизвестного экрана.
type VLMService interface {
	Ground(context.Context, protocol.Frame) (domain.Observation, error)
}

// Observer выбирает локальный или модельный путь анализа кадра.
type Observer struct {
	local LocalDetector
	ocr   OCRService
	vlm   VLMService
}

// New создаёт конвейер наблюдения.
func New(local LocalDetector, ocr OCRService, vlm VLMService) *Observer {
	return &Observer{local: local, ocr: ocr, vlm: vlm}
}

// Observe превращает кадр в проверенное наблюдение.
func (o *Observer) Observe(ctx context.Context, frame protocol.Frame) (domain.Observation, error) {
	state, confidence, regions, err := o.local.Detect(ctx, frame)
	if err != nil {
		return domain.Observation{}, fmt.Errorf("не удалось локально определить экран: %w", err)
	}
	if state == domain.StateUnknown {
		observation, err := o.vlm.Ground(ctx, frame)
		if err != nil {
			return domain.Observation{}, fmt.Errorf("не удалось распознать неизвестный экран через VLM: %w", err)
		}
		observation.FrameID = frame.ID
		observation.CreatedAt = time.Now().UTC()
		return validate(observation)
	}
	values, err := o.ocr.Read(ctx, frame, regions)
	if err != nil {
		return domain.Observation{}, fmt.Errorf("не удалось прочитать известный экран через OCR: %w", err)
	}
	return validate(domain.Observation{FrameID: frame.ID, State: state, Values: values, Confidence: confidence, CreatedAt: time.Now().UTC()})
}

func validate(observation domain.Observation) (domain.Observation, error) {
	if observation.State == "" || observation.Confidence < 0 || observation.Confidence > 1 {
		return domain.Observation{}, fmt.Errorf("сервис наблюдения вернул некорректный результат")
	}
	for name, value := range observation.Values {
		if value.Confidence < 0 || value.Confidence > 1 {
			return domain.Observation{}, fmt.Errorf("значение %q содержит некорректную уверенность", name)
		}
	}
	return observation, nil
}

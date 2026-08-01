package vision

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/arena-trading-agent/arena-trading-agent/internal/domain"
	"github.com/arena-trading-agent/arena-trading-agent/internal/protocol"
)

const maxOCRReadAttempts = 3

// OCRClient calls the separately deployed Python OCR service.
type OCRClient struct {
	baseURL string
	client  *http.Client
}

// NewOCRClient creates an OCR adapter.
func NewOCRClient(baseURL string, timeout time.Duration) (*OCRClient, error) {
	const methodCtx = "vision.NewOCRClient"

	baseURL = strings.TrimRight(baseURL, "/")
	if baseURL == "" {
		baseURL = "http://127.0.0.1:8788"
	}
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("%s: не удалось разобрать URL сервиса OCR %q: %w", methodCtx, baseURL, err)
	}
	if (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return nil, fmt.Errorf("%s: некорректный URL сервиса OCR %q", methodCtx, baseURL)
	}
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	return &OCRClient{baseURL: baseURL, client: &http.Client{Timeout: timeout}}, nil
}

// Health checks service readiness.
func (o *OCRClient) Health(ctx context.Context) error {
	const methodCtx = "vision.OCRClient.Health"

	if ctx == nil {
		return fmt.Errorf("%s: контекст проверки готовности не задан", methodCtx)
	}
	if o == nil || o.client == nil {
		return fmt.Errorf("%s: адаптер OCR не настроен", methodCtx)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, o.baseURL+"/healthz", nil)
	if err != nil {
		return fmt.Errorf("%s: не удалось создать запрос проверки готовности сервиса OCR: %w", methodCtx, err)
	}
	response, err := o.client.Do(request)
	if err != nil {
		return fmt.Errorf("%s: сервис OCR недоступен: %w", methodCtx, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: сервис OCR вернул код HTTP %d", methodCtx, response.StatusCode)
	}
	return nil
}

// Read implements observation.OCRService.
func (o *OCRClient) Read(ctx context.Context, frame protocol.Frame, regions map[string]domain.Rectangle) (map[string]domain.Value, error) {
	const methodCtx = "vision.OCRClient.Read"

	results, err := o.ReadRepeated(ctx, frame, regions, 1)
	if err != nil {
		return nil, fmt.Errorf("%s: чтение OCR завершилось ошибкой: %w", methodCtx, err)
	}
	if len(results) != 1 {
		return nil, fmt.Errorf("%s: OCR вернул %d результатов вместо одного", methodCtx, len(results))
	}
	return results[0], nil
}

// ReadRepeated выполняет не более трёх чтений одного кадра, переиспользуя
// сериализованное тело запроса. Метод является необязательным расширением
// observation.OCRService и сохраняет совместимость простого Read.
func (o *OCRClient) ReadRepeated(
	ctx context.Context,
	frame protocol.Frame,
	regions map[string]domain.Rectangle,
	attempts int,
) ([]map[string]domain.Value, error) {
	const methodCtx = "vision.OCRClient.ReadRepeated"

	if attempts < 1 || attempts > maxOCRReadAttempts {
		return nil, fmt.Errorf(
			"%s: число попыток должно находиться в диапазоне 1..%d",
			methodCtx,
			maxOCRReadAttempts,
		)
	}
	encoded, err := o.prepareOCRRequest(ctx, frame, regions)
	if err != nil {
		return nil, fmt.Errorf("%s: не удалось подготовить запрос OCR: %w", methodCtx, err)
	}
	if len(regions) == 0 {
		results := make([]map[string]domain.Value, attempts)
		for index := range results {
			results[index] = map[string]domain.Value{}
		}
		return results, nil
	}
	results := make([]map[string]domain.Value, 0, attempts)
	for attempt := 1; attempt <= attempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf(
				"%s: контекст завершён перед попыткой %d: %w",
				methodCtx,
				attempt,
				err,
			)
		}
		values, err := o.performOCRRequest(ctx, encoded)
		if err != nil {
			return nil, fmt.Errorf(
				"%s: попытка %d завершилась ошибкой: %w",
				methodCtx,
				attempt,
				err,
			)
		}
		results = append(results, values)
	}
	return results, nil
}

func (o *OCRClient) prepareOCRRequest(
	ctx context.Context,
	frame protocol.Frame,
	regions map[string]domain.Rectangle,
) ([]byte, error) {
	const methodCtx = "vision.OCRClient.prepareOCRRequest"

	if ctx == nil {
		return nil, fmt.Errorf("%s: контекст распознавания не задан", methodCtx)
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("%s: контекст распознавания завершён: %w", methodCtx, err)
	}
	if o == nil || o.client == nil {
		return nil, fmt.Errorf("%s: адаптер OCR не настроен", methodCtx)
	}
	if len(frame.Data) == 0 {
		return nil, fmt.Errorf("%s: кадр %d не содержит изображения", methodCtx, frame.ID)
	}
	if len(regions) > 128 {
		return nil, fmt.Errorf("%s: передано слишком много областей OCR: %d", methodCtx, len(regions))
	}
	for name, region := range regions {
		if strings.TrimSpace(name) == "" || !validRectangle(region) {
			return nil, fmt.Errorf("%s: передана некорректная область OCR %q", methodCtx, name)
		}
	}
	if len(regions) == 0 {
		return nil, nil
	}
	requestBody := ocrRequest{
		FrameID:  frame.ID,
		Encoding: frame.Encoding,
		Image:    base64.StdEncoding.EncodeToString(frame.Data),
		Regions:  regions,
	}
	encoded, err := json.Marshal(requestBody)
	if err != nil {
		return nil, fmt.Errorf("%s: не удалось сериализовать запрос к сервису OCR: %w", methodCtx, err)
	}
	return encoded, nil
}

func (o *OCRClient) performOCRRequest(
	ctx context.Context,
	encoded []byte,
) (map[string]domain.Value, error) {
	const methodCtx = "vision.OCRClient.performOCRRequest"

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, o.baseURL+"/v1/ocr", bytes.NewReader(encoded))
	if err != nil {
		return nil, fmt.Errorf("%s: не удалось создать запрос к сервису OCR: %w", methodCtx, err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := o.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("%s: запрос к сервису OCR завершился ошибкой: %w", methodCtx, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		message, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return nil, fmt.Errorf(
			"%s: сервис OCR вернул код HTTP %d: %s",
			methodCtx,
			response.StatusCode,
			strings.TrimSpace(string(message)),
		)
	}
	var result ocrResponse
	if err := json.NewDecoder(io.LimitReader(response.Body, 8<<20)).Decode(&result); err != nil {
		return nil, fmt.Errorf("%s: не удалось прочитать ответ сервиса OCR: %w", methodCtx, err)
	}
	values := make(map[string]domain.Value, len(result.Values))
	for name, value := range result.Values {
		if strings.TrimSpace(name) == "" ||
			(strings.TrimSpace(value.Raw) == "" && strings.TrimSpace(value.Normalized) == "") ||
			!validConfidence(value.Confidence) ||
			!validRectangle(value.Region) {
			return nil, fmt.Errorf("%s: сервис OCR вернул некорректное значение %q", methodCtx, name)
		}
		value.Source = "OCR"
		values[name] = value
	}
	return values, nil
}

type ocrRequest struct {
	FrameID  uint64                      `json:"frame_id"`
	Encoding string                      `json:"encoding"`
	Image    string                      `json:"image"`
	Regions  map[string]domain.Rectangle `json:"regions"`
}

type ocrResponse struct {
	Values map[string]domain.Value `json:"values"`
}

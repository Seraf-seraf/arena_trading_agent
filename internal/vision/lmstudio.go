// Package vision contains adapters for external visual-computing services.
package vision

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	_ "image/jpeg"
	"image/png"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/arena-trading-agent/arena-trading-agent/internal/domain"
	"github.com/arena-trading-agent/arena-trading-agent/internal/protocol"
)

const (
	maxVisionDimension = 896
	maxVisionPixels    = 33_554_432
)

// LMStudioConfig configures the OpenAI-compatible LM Studio VLM endpoint.
type LMStudioConfig struct {
	BaseURL         string
	Model           string
	APIKey          string
	RequestTimeout  time.Duration
	AutoLoad        bool
	ContextLength   int
	ReasoningEffort string
}

// LMStudio grounds unknown UI screens without giving the model permission to
// perform input or make economic decisions.
type LMStudio struct {
	baseURL         string
	model           string
	apiKey          string
	client          *http.Client
	autoLoad        bool
	contextLength   int
	reasoningEffort string
	modelMu         sync.RWMutex
	loadMu          sync.Mutex
}

// NewLMStudio creates an LM Studio adapter. BaseURL normally points at
// http://127.0.0.1:1234.
func NewLMStudio(config LMStudioConfig) (*LMStudio, error) {
	const methodCtx = "vision.NewLMStudio"

	baseURL := strings.TrimRight(config.BaseURL, "/")
	if baseURL == "" {
		baseURL = "http://127.0.0.1:1234"
	}
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("%s: не удалось разобрать URL LM Studio %q: %w", methodCtx, baseURL, err)
	}
	if (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return nil, fmt.Errorf("%s: некорректный URL LM Studio %q", methodCtx, baseURL)
	}
	timeout := config.RequestTimeout
	if timeout <= 0 {
		timeout = 90 * time.Second
	}
	reasoningEffort := strings.ToLower(strings.TrimSpace(config.ReasoningEffort))
	if reasoningEffort == "" {
		// UI grounding is a constrained extraction task. Disabling hidden
		// reasoning avoids spending the entire output budget before JSON is
		// emitted by reasoning-first models such as Qwen3.5.
		reasoningEffort = "none"
	}
	switch reasoningEffort {
	case "none", "minimal", "low", "medium", "high", "xhigh":
	default:
		return nil, fmt.Errorf(
			"%s: некорректное значение параметра reasoning_effort %q",
			methodCtx,
			config.ReasoningEffort,
		)
	}
	return &LMStudio{
		baseURL:         baseURL,
		model:           strings.TrimSpace(config.Model),
		apiKey:          config.APIKey,
		client:          &http.Client{Timeout: timeout},
		autoLoad:        config.AutoLoad,
		contextLength:   config.ContextLength,
		reasoningEffort: reasoningEffort,
	}, nil
}

// Health verifies the server and resolves a vision-capable model when one was
// not explicitly configured.
func (l *LMStudio) Health(ctx context.Context) error {
	const methodCtx = "vision.LMStudio.Health"

	if ctx == nil {
		return fmt.Errorf("%s: контекст проверки готовности не задан", methodCtx)
	}
	if l == nil || l.client == nil {
		return fmt.Errorf("%s: адаптер LM Studio не настроен", methodCtx)
	}
	model, err := l.resolveModel(ctx)
	if err != nil {
		return fmt.Errorf("%s: не удалось определить модель компьютерного зрения: %w", methodCtx, err)
	}
	if model == "" {
		return fmt.Errorf("%s: LM Studio не содержит модель компьютерного зрения", methodCtx)
	}
	if err := l.ensureLoaded(ctx, model); err != nil {
		return fmt.Errorf("%s: модель компьютерного зрения не готова: %w", methodCtx, err)
	}
	return nil
}

// Ground implements observation.VLMService.
func (l *LMStudio) Ground(ctx context.Context, frame protocol.Frame) (domain.Observation, error) {
	const methodCtx = "vision.LMStudio.Ground"

	if ctx == nil {
		return domain.Observation{}, fmt.Errorf("%s: контекст распознавания не задан", methodCtx)
	}
	if l == nil || l.client == nil {
		return domain.Observation{}, fmt.Errorf("%s: адаптер LM Studio не настроен", methodCtx)
	}
	if len(frame.Data) == 0 {
		return domain.Observation{}, fmt.Errorf("%s: кадр %d не содержит изображения", methodCtx, frame.ID)
	}
	model, err := l.resolveModel(ctx)
	if err != nil {
		return domain.Observation{}, fmt.Errorf("%s: не удалось определить модель компьютерного зрения: %w", methodCtx, err)
	}
	if err := l.ensureLoaded(ctx, model); err != nil {
		return domain.Observation{}, fmt.Errorf("%s: модель компьютерного зрения не готова: %w", methodCtx, err)
	}
	imageData, mediaType, err := prepareVisionImage(frame)
	if err != nil {
		return domain.Observation{}, fmt.Errorf("%s: не удалось подготовить изображение: %w", methodCtx, err)
	}
	dataURL := "data:" + mediaType + ";base64," + base64.StdEncoding.EncodeToString(imageData)
	request := chatCompletionRequest{
		Model: model,
		Messages: []chatMessage{
			{
				Role: "system",
				Content: "Ты vision-модуль торгового агента Arena Breakout Infinite. " +
					"Определи только активный центральный экран UI, видимые торговые элементы и денежные значения. " +
					"Не предлагай клики, сделки или финансовые решения. " +
					"Region — это x,y левого верхнего угла и width,height размера в долях кадра; " +
					"все числа 0..1, обязательно x+width<=1 и y+height<=1.",
			},
			{
				Role: "user",
				Content: []contentPart{
					{Type: "text", Text: "Разбери текущий игровой кадр. " +
						"Состояние определяется активным содержимым, а не нижней навигацией. " +
						"Рекламная страница, событие, лобби или любой экран вне заданных торговых состояний — UNKNOWN. " +
						"Для UNKNOWN верни не более 4 крупных элементов навигации/закрытия, полезных для восстановления, " +
						"и только явно видимые денежные значения. Для известного экрана верни не более 8 только торгово-значимых элементов и 8 значений."},
					{Type: "image_url", ImageURL: &imageURL{URL: dataURL}},
				},
			},
		},
		ResponseFormat:  observationResponseFormat(),
		ReasoningEffort: l.reasoningEffort,
		Temperature:     0,
		MaxTokens:       768,
		Stream:          false,
	}
	var response chatCompletionResponse
	if err := l.doJSON(ctx, http.MethodPost, "/v1/chat/completions", request, &response); err != nil {
		return domain.Observation{}, fmt.Errorf("%s: не удалось получить разбор кадра: %w", methodCtx, err)
	}
	if len(response.Choices) == 0 {
		return domain.Observation{}, fmt.Errorf(
			"%s: LM Studio вернула ответ без вариантов в поле choices",
			methodCtx,
		)
	}
	content := strings.TrimSpace(response.Choices[0].Message.Content)
	if content == "" {
		reasoningOnly := strings.TrimSpace(response.Choices[0].Message.ReasoningContent) != ""
		return domain.Observation{}, fmt.Errorf(
			"%s: LM Studio вернула пустое поле content (finish_reason=%q, только_рассуждения=%t)",
			methodCtx,
			response.Choices[0].FinishReason,
			reasoningOnly,
		)
	}
	var grounded groundedObservation
	if err := json.Unmarshal([]byte(content), &grounded); err != nil {
		return domain.Observation{}, fmt.Errorf(
			"%s: LM Studio вернула некорректное наблюдение (finish_reason=%q, размер_в_байтах=%d): %w",
			methodCtx,
			response.Choices[0].FinishReason,
			len(content),
			err,
		)
	}
	observation, err := grounded.toDomain(frame.ID)
	if err != nil {
		return domain.Observation{}, fmt.Errorf("%s: не удалось нормализовать наблюдение: %w", methodCtx, err)
	}
	return observation, nil
}

// prepareVisionImage bounds visual tokens and transient memory. Normalized
// coordinates remain unchanged because aspect ratio is preserved.
func prepareVisionImage(frame protocol.Frame) ([]byte, string, error) {
	const methodCtx = "vision.prepareVisionImage"

	mediaType, err := frameMediaType(frame.Encoding)
	if err != nil {
		return nil, "", fmt.Errorf("%s: не удалось определить формат изображения: %w", methodCtx, err)
	}
	config, _, err := image.DecodeConfig(bytes.NewReader(frame.Data))
	if err != nil {
		return nil, "", fmt.Errorf(
			"%s: не удалось прочитать размеры кадра %d: %w",
			methodCtx,
			frame.ID,
			err,
		)
	}
	if config.Width <= 0 || config.Height <= 0 ||
		config.Width > maxVisionPixels/config.Height {
		return nil, "", fmt.Errorf(
			"%s: кадр %d имеет недопустимый размер %dx%d",
			methodCtx,
			frame.ID,
			config.Width,
			config.Height,
		)
	}
	if config.Width <= maxVisionDimension && config.Height <= maxVisionDimension {
		return frame.Data, mediaType, nil
	}
	source, _, err := image.Decode(bytes.NewReader(frame.Data))
	if err != nil {
		return nil, "", fmt.Errorf(
			"%s: не удалось декодировать кадр %d: %w",
			methodCtx,
			frame.ID,
			err,
		)
	}
	scale := min(
		float64(maxVisionDimension)/float64(config.Width),
		float64(maxVisionDimension)/float64(config.Height),
	)
	width := max(1, int(float64(config.Width)*scale))
	height := max(1, int(float64(config.Height)*scale))
	destination := image.NewRGBA(image.Rect(0, 0, width, height))
	bounds := source.Bounds()
	for y := 0; y < height; y++ {
		sourceY := bounds.Min.Y + min(bounds.Dy()-1, y*bounds.Dy()/height)
		for x := 0; x < width; x++ {
			sourceX := bounds.Min.X + min(bounds.Dx()-1, x*bounds.Dx()/width)
			destination.Set(x, y, source.At(sourceX, sourceY))
		}
	}
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, destination); err != nil {
		return nil, "", fmt.Errorf(
			"%s: не удалось уменьшить кадр %d: %w",
			methodCtx,
			frame.ID,
			err,
		)
	}
	return encoded.Bytes(), "image/png", nil
}

func (l *LMStudio) resolveModel(ctx context.Context) (string, error) {
	const methodCtx = "vision.LMStudio.resolveModel"

	l.modelMu.RLock()
	configured := l.model
	l.modelMu.RUnlock()
	var response modelsResponse
	if err := l.doJSON(ctx, http.MethodGet, "/api/v1/models", nil, &response); err != nil {
		return "", fmt.Errorf("%s: LM Studio недоступна: %w", methodCtx, err)
	}
	if configured != "" {
		model, err := requireVisionModel(response.Models, configured)
		if err != nil {
			return "", fmt.Errorf(
				"%s: настроенная модель %q не прошла проверку: %w",
				methodCtx,
				configured,
				err,
			)
		}
		return model.Key, nil
	}
	candidate := ""
	for _, model := range response.Models {
		if model.Type == "llm" && model.Capabilities.Vision && len(model.LoadedInstances) > 0 {
			candidate = model.Key
			break
		}
	}
	if candidate == "" {
		for _, model := range response.Models {
			if model.Type == "llm" && model.Capabilities.Vision {
				candidate = model.Key
				break
			}
		}
	}
	if candidate == "" {
		return "", fmt.Errorf("%s: в LM Studio не найдена модель компьютерного зрения", methodCtx)
	}
	l.modelMu.Lock()
	if l.model == "" {
		l.model = candidate
	}
	configured = l.model
	l.modelMu.Unlock()
	return configured, nil
}

func (l *LMStudio) ensureLoaded(ctx context.Context, modelKey string) error {
	const methodCtx = "vision.LMStudio.ensureLoaded"

	l.loadMu.Lock()
	defer l.loadMu.Unlock()
	var models modelsResponse
	if err := l.doJSON(ctx, http.MethodGet, "/api/v1/models", nil, &models); err != nil {
		return fmt.Errorf("%s: LM Studio недоступна: %w", methodCtx, err)
	}
	model, err := requireVisionModel(models.Models, modelKey)
	if err != nil {
		return fmt.Errorf("%s: модель %q не прошла проверку: %w", methodCtx, modelKey, err)
	}
	if len(model.LoadedInstances) > 0 {
		return nil
	}
	if !l.autoLoad {
		return fmt.Errorf(
			"%s: модель компьютерного зрения %q установлена, но не загружена",
			methodCtx,
			modelKey,
		)
	}
	contextLength := l.contextLength
	if contextLength <= 0 {
		contextLength = 2048
	}
	request := map[string]any{
		"model":                   modelKey,
		"context_length":          contextLength,
		"flash_attention":         true,
		"offload_kv_cache_to_gpu": false,
	}
	var response struct {
		Status string `json:"status"`
	}
	if err := l.doJSON(ctx, http.MethodPost, "/api/v1/models/load", request, &response); err != nil {
		return fmt.Errorf(
			"%s: не удалось загрузить модель компьютерного зрения %q: %w",
			methodCtx,
			modelKey,
			err,
		)
	}
	status := strings.ToLower(strings.TrimSpace(response.Status))
	if status != "" && status != "loaded" && status != "loading" {
		return fmt.Errorf(
			"%s: LM Studio вернула неожиданный статус загрузки %q",
			methodCtx,
			response.Status,
		)
	}
	waitCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		var current modelsResponse
		if err := l.doJSON(waitCtx, http.MethodGet, "/api/v1/models", nil, &current); err != nil {
			return fmt.Errorf(
				"%s: не удалось проверить завершение загрузки модели %q: %w",
				methodCtx,
				modelKey,
				err,
			)
		}
		currentModel, err := requireVisionModel(current.Models, modelKey)
		if err != nil {
			return fmt.Errorf(
				"%s: модель %q перестала проходить проверку во время загрузки: %w",
				methodCtx,
				modelKey,
				err,
			)
		}
		if len(currentModel.LoadedInstances) > 0 {
			return nil
		}
		select {
		case <-waitCtx.Done():
			return fmt.Errorf(
				"%s: ожидание загрузки модели %q завершено: %w",
				methodCtx,
				modelKey,
				waitCtx.Err(),
			)
		case <-ticker.C:
		}
	}
}

func (l *LMStudio) doJSON(ctx context.Context, method, path string, input, output any) error {
	const methodCtx = "vision.LMStudio.doJSON"

	var body io.Reader
	if input != nil {
		encoded, err := json.Marshal(input)
		if err != nil {
			return fmt.Errorf("%s: не удалось сериализовать запрос к LM Studio: %w", methodCtx, err)
		}
		body = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, l.baseURL+path, body)
	if err != nil {
		return fmt.Errorf("%s: не удалось создать запрос к LM Studio: %w", methodCtx, err)
	}
	request.Header.Set("Accept", "application/json")
	if input != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if l.apiKey != "" {
		request.Header.Set("Authorization", "Bearer "+l.apiKey)
	}
	response, err := l.client.Do(request)
	if err != nil {
		return fmt.Errorf("%s: запрос к LM Studio завершился ошибкой: %w", methodCtx, err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		message, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return fmt.Errorf(
			"%s: LM Studio вернула код HTTP %d: %s",
			methodCtx,
			response.StatusCode,
			strings.TrimSpace(string(message)),
		)
	}
	if output == nil {
		return nil
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 8<<20)).Decode(output); err != nil {
		return fmt.Errorf("%s: не удалось прочитать ответ LM Studio: %w", methodCtx, err)
	}
	return nil
}

func frameMediaType(encoding string) (string, error) {
	const methodCtx = "vision.frameMediaType"

	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "png", "image/png":
		return "image/png", nil
	case "jpg", "jpeg", "image/jpeg":
		return "image/jpeg", nil
	case "webp", "image/webp":
		return "image/webp", nil
	default:
		return "", fmt.Errorf(
			"%s: LM Studio не поддерживает кодировку кадра %q",
			methodCtx,
			encoding,
		)
	}
}

type contentPart struct {
	Type     string    `json:"type"`
	Text     string    `json:"text,omitempty"`
	ImageURL *imageURL `json:"image_url,omitempty"`
}

type imageURL struct {
	URL string `json:"url"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

type chatCompletionRequest struct {
	Model           string         `json:"model"`
	Messages        []chatMessage  `json:"messages"`
	ResponseFormat  map[string]any `json:"response_format"`
	ReasoningEffort string         `json:"reasoning_effort"`
	Temperature     float64        `json:"temperature"`
	MaxTokens       int            `json:"max_tokens"`
	Stream          bool           `json:"stream"`
}

type chatCompletionResponse struct {
	Choices []struct {
		Message struct {
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
}

type modelInfo struct {
	Type            string `json:"type"`
	Key             string `json:"key"`
	LoadedInstances []any  `json:"loaded_instances"`
	Capabilities    struct {
		Vision bool `json:"vision"`
	} `json:"capabilities"`
}

type modelsResponse struct {
	Models []modelInfo `json:"models"`
}

func requireVisionModel(models []modelInfo, key string) (modelInfo, error) {
	const methodCtx = "vision.requireVisionModel"

	matchIndex := -1
	for index := range models {
		if models[index].Key != key {
			continue
		}
		if matchIndex >= 0 {
			return modelInfo{}, fmt.Errorf(
				"%s: LM Studio вернула несколько записей модели %q",
				methodCtx,
				key,
			)
		}
		matchIndex = index
	}
	if matchIndex < 0 {
		return modelInfo{}, fmt.Errorf(
			"%s: модель %q не установлена в LM Studio",
			methodCtx,
			key,
		)
	}
	model := models[matchIndex]
	if model.Type != "llm" {
		return modelInfo{}, fmt.Errorf(
			"%s: модель %q имеет тип %q вместо llm",
			methodCtx,
			key,
			model.Type,
		)
	}
	if !model.Capabilities.Vision {
		return modelInfo{}, fmt.Errorf(
			"%s: модель %q не поддерживает vision",
			methodCtx,
			key,
		)
	}
	return model, nil
}

type groundedObservation struct {
	State      string            `json:"state"`
	Confidence float64           `json:"confidence"`
	Elements   []groundedElement `json:"elements"`
	Values     []groundedValue   `json:"values"`
}

type groundedElement struct {
	Kind       string           `json:"kind"`
	Label      string           `json:"label"`
	Region     domain.Rectangle `json:"region"`
	Confidence float64          `json:"confidence"`
}

type groundedValue struct {
	Name       string           `json:"name"`
	Raw        string           `json:"raw"`
	Normalized string           `json:"normalized"`
	Region     domain.Rectangle `json:"region"`
	Confidence float64          `json:"confidence"`
}

func (g groundedObservation) toDomain(frameID uint64) (domain.Observation, error) {
	const methodCtx = "vision.groundedObservation.toDomain"

	state := domain.ScreenState(g.State)
	if !knownState(state) {
		return domain.Observation{}, fmt.Errorf(
			"%s: LM Studio вернула неизвестное состояние %q",
			methodCtx,
			g.State,
		)
	}
	if !validConfidence(g.Confidence) {
		return domain.Observation{}, fmt.Errorf(
			"%s: LM Studio вернула некорректное значение уверенности",
			methodCtx,
		)
	}
	observation := domain.Observation{
		FrameID:    frameID,
		State:      state,
		Confidence: g.Confidence,
		CreatedAt:  time.Now().UTC(),
		Elements:   make([]domain.UIElement, 0, len(g.Elements)),
		Values:     make(map[string]domain.Value, len(g.Values)),
	}
	for _, element := range g.Elements {
		region, adjusted, ok := normalizeGroundedRectangle(element.Region)
		if !validConfidence(element.Confidence) || !ok {
			// A malformed optional grounding target must never become an
			// action coordinate. Drop it and lower the whole observation
			// below the default decision threshold while preserving a usable
			// screen classification.
			observation.Confidence = min(observation.Confidence, .5)
			continue
		}
		if adjusted {
			element.Confidence = min(element.Confidence, .5)
			observation.Confidence = min(observation.Confidence, .5)
		}
		observation.Elements = append(observation.Elements, domain.UIElement{
			Kind: element.Kind, Label: element.Label, Region: region,
			Confidence: element.Confidence, GeometryAdjusted: adjusted,
		})
	}
	for _, value := range g.Values {
		region, adjusted, ok := normalizeGroundedRectangle(value.Region)
		if value.Name == "" || !validConfidence(value.Confidence) || !ok {
			// Monetary data without a valid provenance region is unusable.
			// Omit it and make the observation explicitly low-confidence.
			observation.Confidence = min(observation.Confidence, .5)
			continue
		}
		source := "VLM"
		if adjusted {
			source = "VLM_ADJUSTED"
			value.Confidence = min(value.Confidence, .5)
			observation.Confidence = min(observation.Confidence, .5)
		}
		observation.Values[value.Name] = domain.Value{
			Raw: value.Raw, Normalized: value.Normalized, Source: source,
			Confidence: value.Confidence, Region: region,
		}
	}
	return observation, nil
}

func knownState(state domain.ScreenState) bool {
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
	return value >= 0 && value <= 1
}

func validRectangle(value domain.Rectangle) bool {
	return value.X >= 0 && value.Y >= 0 && value.Width > 0 && value.Height > 0 &&
		value.X+value.Width <= 1 && value.Y+value.Height <= 1
}

// normalizeGroundedRectangle clips only an otherwise valid VLM box at the
// right/bottom frame edge. The adjustment is surfaced to callers and caps
// confidence, so corrected geometry can never authorize a high-confidence
// action or monetary decision.
func normalizeGroundedRectangle(value domain.Rectangle) (domain.Rectangle, bool, bool) {
	if value.X < 0 || value.X >= 1 || value.Y < 0 || value.Y >= 1 ||
		value.Width <= 0 || value.Height <= 0 {
		return domain.Rectangle{}, false, false
	}
	adjusted := false
	if value.X+value.Width > 1 {
		value.Width = 1 - value.X
		adjusted = true
	}
	if value.Y+value.Height > 1 {
		value.Height = 1 - value.Y
		adjusted = true
	}
	if !validRectangle(value) {
		return domain.Rectangle{}, adjusted, false
	}
	return value, adjusted, true
}

func observationResponseFormat() map[string]any {
	rectangle := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"x":      map[string]any{"type": "number", "minimum": 0, "maximum": 1},
			"y":      map[string]any{"type": "number", "minimum": 0, "maximum": 1},
			"width":  map[string]any{"type": "number", "exclusiveMinimum": 0, "maximum": 1},
			"height": map[string]any{"type": "number", "exclusiveMinimum": 0, "maximum": 1},
		},
		"required":             []string{"x", "y", "width", "height"},
		"additionalProperties": false,
	}
	states := []string{
		string(domain.StateUnknown), string(domain.StateMainMenu), string(domain.StateMarketHome),
		string(domain.StateMarketSearch), string(domain.StateMarketResults), string(domain.StateItemCard),
		string(domain.StatePurchaseDialog), string(domain.StateContacts), string(domain.StateContactPage),
		string(domain.StateContactBarter), string(domain.StateBarterCard), string(domain.StateInventory),
		string(domain.StateSaleDialog), string(domain.StateConfirmation), string(domain.StateErrorDialog),
	}
	return map[string]any{
		"type": "json_schema",
		"json_schema": map[string]any{
			"name":   "arena_observation",
			"strict": true,
			"schema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"state":      map[string]any{"type": "string", "enum": states},
					"confidence": map[string]any{"type": "number", "minimum": 0, "maximum": 1},
					"elements": map[string]any{"type": "array", "maxItems": 8, "items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"kind":       map[string]any{"type": "string"},
							"label":      map[string]any{"type": "string"},
							"region":     rectangle,
							"confidence": map[string]any{"type": "number", "minimum": 0, "maximum": 1},
						},
						"required":             []string{"kind", "label", "region", "confidence"},
						"additionalProperties": false,
					}},
					"values": map[string]any{"type": "array", "maxItems": 8, "items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"name":       map[string]any{"type": "string"},
							"raw":        map[string]any{"type": "string"},
							"normalized": map[string]any{"type": "string"},
							"region":     rectangle,
							"confidence": map[string]any{"type": "number", "minimum": 0, "maximum": 1},
						},
						"required":             []string{"name", "raw", "normalized", "region", "confidence"},
						"additionalProperties": false,
					}},
				},
				"required":             []string{"state", "confidence", "elements", "values"},
				"additionalProperties": false,
			},
		},
	}
}

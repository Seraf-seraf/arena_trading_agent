package vision_test

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/arena-trading-agent/arena-trading-agent/internal/domain"
	"github.com/arena-trading-agent/arena-trading-agent/internal/protocol"
	"github.com/arena-trading-agent/arena-trading-agent/internal/vision"
)

func TestLMStudioHealthAutoLoadsConfiguredModel(t *testing.T) {
	var loaded atomic.Bool
	var loadRequestValid atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/models":
			instances := []any{}
			if loaded.Load() {
				instances = append(instances, map[string]any{"id": "small-vlm"})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"models": []any{map[string]any{
				"type": "llm", "key": "small-vlm", "loaded_instances": instances,
				"capabilities": map[string]any{"vision": true},
			}}})
		case "/api/v1/models/load":
			var request map[string]any
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Errorf("не удалось разобрать запрос загрузки модели: %v", err)
			}
			if request["model"] == "small-vlm" &&
				request["context_length"] == float64(2048) &&
				request["offload_kv_cache_to_gpu"] == false {
				loadRequestValid.Store(true)
			}
			loaded.Store(true)
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "loaded"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client, err := vision.NewLMStudio(vision.LMStudioConfig{
		BaseURL: server.URL, Model: "small-vlm", AutoLoad: true, ContextLength: 2048,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Health(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !loaded.Load() {
		t.Fatal("модель не была загружена")
	}
	if !loadRequestValid.Load() {
		t.Fatal("запрос загрузки модели не подтвердил CPU-only параметры")
	}
}

func TestLMStudioHealthRejectsConfiguredModelWithoutVision(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/models" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"models": []any{map[string]any{
			"type": "llm", "key": "small-vlm", "loaded_instances": []any{},
			"capabilities": map[string]any{"vision": false},
		}}})
	}))
	defer server.Close()

	client, err := vision.NewLMStudio(vision.LMStudioConfig{
		BaseURL: server.URL, Model: "small-vlm", AutoLoad: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	err = client.Health(context.Background())
	if err == nil || !strings.Contains(err.Error(), `модель "small-vlm" не поддерживает vision`) {
		t.Fatalf("неожиданная ошибка проверки настроенной модели: %v", err)
	}
}

func TestLMStudioHealthRejectsDuplicateConfiguredModelKey(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/models" {
			http.NotFound(w, r)
			return
		}
		model := map[string]any{
			"type": "llm", "key": "small-vlm", "loaded_instances": []any{},
			"capabilities": map[string]any{"vision": true},
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"models": []any{model, model}})
	}))
	defer server.Close()

	client, err := vision.NewLMStudio(vision.LMStudioConfig{
		BaseURL: server.URL, Model: "small-vlm", AutoLoad: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	err = client.Health(context.Background())
	if err == nil || !strings.Contains(err.Error(), `несколько записей модели "small-vlm"`) {
		t.Fatalf("неожиданная ошибка неоднозначного ключа модели: %v", err)
	}
}

func TestLMStudioHealthDoesNotFallbackFromConfiguredModel(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/models" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"models": []any{map[string]any{
			"type": "llm", "key": "other-vlm",
			"loaded_instances": []any{map[string]any{"id": "other-vlm"}},
			"capabilities":     map[string]any{"vision": true},
		}}})
	}))
	defer server.Close()

	client, err := vision.NewLMStudio(vision.LMStudioConfig{
		BaseURL: server.URL, Model: "small-vlm", AutoLoad: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	err = client.Health(context.Background())
	if err == nil || !strings.Contains(err.Error(), `модель "small-vlm" не установлена`) {
		t.Fatalf("настроенная модель не должна заменяться другой vision-моделью: %v", err)
	}
}

func TestLMStudioGroundsFrameWithStructuredOutput(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/models":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"models": []any{map[string]any{
					"type": "llm", "key": "test-vlm",
					"capabilities":     map[string]any{"vision": true},
					"loaded_instances": []any{map[string]any{"id": "test-vlm"}},
				}},
			})
		case "/v1/chat/completions":
			var request map[string]any
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Errorf("не удалось разобрать запрос: %v", err)
			}
			if request["response_format"] == nil {
				t.Error("отсутствует формат структурированного ответа")
			}
			if request["reasoning_effort"] != "none" {
				t.Errorf(
					"параметр reasoning_effort=%v, ожидалось значение none",
					request["reasoning_effort"],
				)
			}
			responseFormat := request["response_format"].(map[string]any)
			jsonSchema := responseFormat["json_schema"].(map[string]any)
			schema := jsonSchema["schema"].(map[string]any)
			properties := schema["properties"].(map[string]any)
			if properties["elements"].(map[string]any)["maxItems"] != float64(8) {
				t.Error("количество элементов должно быть ограничено восемью")
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"choices": []any{map[string]any{
					"message": map[string]any{
						"content": `{"state":"MAIN_MENU","confidence":0.98,"elements":[{"kind":"button","label":"market","region":{"x":0.1,"y":0.2,"width":0.2,"height":0.1},"confidence":0.9}],"values":[{"name":"balance","raw":"12 345","normalized":"12345","region":{"x":0.7,"y":0.02,"width":0.2,"height":0.05},"confidence":0.95}]}`,
					},
				}},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client, err := vision.NewLMStudio(vision.LMStudioConfig{BaseURL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	observation, err := client.Ground(context.Background(), protocol.Frame{
		ID: 7, Encoding: "png", Data: testPNG(t),
	})
	if err != nil {
		t.Fatal(err)
	}
	if observation.FrameID != 7 || observation.State != domain.StateMainMenu {
		t.Fatalf("неожиданное наблюдение: %+v", observation)
	}
	if observation.Values["balance"].Source != "VLM" {
		t.Fatalf("неожиданное значение: %+v", observation.Values["balance"])
	}
}

func TestLMStudioRejectsUnsupportedReasoningEffort(t *testing.T) {
	t.Parallel()
	if _, err := vision.NewLMStudio(vision.LMStudioConfig{
		BaseURL: "http://127.0.0.1:1234", ReasoningEffort: "unbounded",
	}); err == nil {
		t.Fatal("ожидалась ошибка для неподдерживаемого значения параметра reasoning_effort")
	}
}

func TestLMStudioReportsTruncatedStructuredOutput(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/models" {
			_ = json.NewEncoder(w).Encode(map[string]any{"models": []any{map[string]any{
				"type": "llm", "key": "test", "loaded_instances": []any{map[string]any{"id": "test"}},
				"capabilities": map[string]any{"vision": true},
			}}})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{
			"finish_reason": "length",
			"message":       map[string]any{"content": `{"state":"UNKNOWN"`},
		}}})
	}))
	defer server.Close()

	client, err := vision.NewLMStudio(vision.LMStudioConfig{BaseURL: server.URL, Model: "test"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Ground(context.Background(), protocol.Frame{Encoding: "png", Data: testPNG(t)})
	if err == nil || !strings.Contains(err.Error(), `finish_reason="length"`) {
		t.Fatalf("неожиданная ошибка: %v", err)
	}
}

func TestLMStudioClipsOutOfBoundsGroundingAndCapsConfidence(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/models" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"models": []any{map[string]any{
					"type": "llm", "key": "test", "loaded_instances": []any{map[string]any{"id": "test"}},
					"capabilities": map[string]any{"vision": true},
				}},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{
				"message": map[string]any{
					"content": `{"state":"MAIN_MENU","confidence":0.8,"elements":[{"kind":"button","label":"clipped","region":{"x":0.9,"y":0.1,"width":0.2,"height":0.1},"confidence":0.8},{"kind":"button","label":"dropped","region":{"x":0.1,"y":1.08,"width":0.2,"height":0.1},"confidence":0.9}],"values":[]}`,
				},
			}},
		})
	}))
	defer server.Close()

	client, err := vision.NewLMStudio(vision.LMStudioConfig{BaseURL: server.URL, Model: "test"})
	if err != nil {
		t.Fatal(err)
	}
	observation, err := client.Ground(context.Background(), protocol.Frame{Encoding: "png", Data: testPNG(t)})
	if err != nil {
		t.Fatal(err)
	}
	if observation.Confidence != .5 || len(observation.Elements) != 1 {
		t.Fatalf("неожиданное наблюдение: %+v", observation)
	}
	element := observation.Elements[0]
	if element.Label != "clipped" || !element.GeometryAdjusted || element.Confidence != .5 ||
		math.Abs(element.Region.Width-.1) > 1e-9 {
		t.Fatalf("геометрия не была безопасно скорректирована: %+v", element)
	}
}

func testPNG(t *testing.T) []byte {
	t.Helper()
	source := image.NewRGBA(image.Rect(0, 0, 2, 2))
	source.Set(0, 0, color.White)
	var output bytes.Buffer
	if err := png.Encode(&output, source); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

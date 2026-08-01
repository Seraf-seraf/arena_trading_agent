package vision_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/arena-trading-agent/arena-trading-agent/internal/domain"
	"github.com/arena-trading-agent/arena-trading-agent/internal/protocol"
	"github.com/arena-trading-agent/arena-trading-agent/internal/vision"
)

func TestOCRClientReadsNamedRegions(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.WriteHeader(http.StatusOK)
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("не удалось разобрать запрос: %v", err)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"values": map[string]any{
				"price": map[string]any{
					"raw": "18 430", "normalized": "18430", "source": "",
					"confidence": 0.97,
					"region":     map[string]any{"x": 0.1, "y": 0.1, "width": 0.2, "height": 0.1},
				},
			},
		})
	}))
	defer server.Close()

	client, err := vision.NewOCRClient(server.URL, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Health(context.Background()); err != nil {
		t.Fatal(err)
	}
	values, err := client.Read(context.Background(), protocol.Frame{
		ID: 1, Encoding: "png", Data: []byte("image"),
	}, map[string]domain.Rectangle{
		"price": {X: .1, Y: .1, Width: .2, Height: .1},
	})
	if err != nil {
		t.Fatal(err)
	}
	if values["price"].Normalized != "18430" || values["price"].Source != "OCR" {
		t.Fatalf("неожиданное значение OCR: %+v", values["price"])
	}
}

func TestOCRClientReadRepeatedUsesBoundedAttempts(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"values": map[string]any{
				"balance": map[string]any{
					"raw": "12345", "normalized": "12345",
					"confidence": 0.97,
					"region":     map[string]any{"x": 0.1, "y": 0.1, "width": 0.2, "height": 0.1},
				},
			},
		})
	}))
	defer server.Close()

	client, err := vision.NewOCRClient(server.URL, 0)
	if err != nil {
		t.Fatal(err)
	}
	results, err := client.ReadRepeated(
		context.Background(),
		protocol.Frame{ID: 2, Encoding: "png", Data: []byte("same-image")},
		map[string]domain.Rectangle{
			"balance": {X: .1, Y: .1, Width: .2, Height: .1},
		},
		3,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 3 || calls.Load() != 3 {
		t.Fatalf("получено %d результатов и %d запросов вместо 3", len(results), calls.Load())
	}
	for index, values := range results {
		if values["balance"].Source != "OCR" {
			t.Fatalf("попытка %d потеряла источник OCR: %+v", index+1, values)
		}
	}

	_, err = client.ReadRepeated(
		context.Background(),
		protocol.Frame{ID: 2, Encoding: "png", Data: []byte("same-image")},
		map[string]domain.Rectangle{
			"balance": {X: .1, Y: .1, Width: .2, Height: .1},
		},
		4,
	)
	if err == nil || !strings.Contains(err.Error(), "1..3") {
		t.Fatalf("ожидалась ошибка лимита повторов, получено: %v", err)
	}
	if calls.Load() != 3 {
		t.Fatalf("запрос сверх лимита дошёл до сервиса: вызовов %d", calls.Load())
	}
}

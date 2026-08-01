package automation

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/arena-trading-agent/arena-trading-agent/internal/domain"
	"github.com/arena-trading-agent/arena-trading-agent/internal/repository"
)

func TestHandlerLocalizesRoutingErrors(t *testing.T) {
	t.Parallel()

	handler := (&Engine{}).Handler()
	tests := []struct {
		name       string
		method     string
		path       string
		wantStatus int
		wantText   string
		wantAllow  string
	}{
		{
			name:       "method not allowed",
			method:     http.MethodPost,
			path:       "/api/v1/automation/evaluation",
			wantStatus: http.StatusMethodNotAllowed,
			wantText:   "automation.Engine.methodNotAllowedHTTP: метод HTTP не поддерживается",
			wantAllow:  "GET, HEAD",
		},
		{
			name:       "order method not allowed",
			method:     http.MethodPost,
			path:       "/api/v1/automation/orders/execution",
			wantStatus: http.StatusMethodNotAllowed,
			wantText:   "automation.Engine.methodNotAllowedHTTP: метод HTTP не поддерживается",
			wantAllow:  "GET, HEAD",
		},
		{
			name:       "not found",
			method:     http.MethodGet,
			path:       "/api/v1/automation/неизвестно",
			wantStatus: http.StatusNotFound,
			wantText:   "automation.Engine.notFoundHTTP: запрошенный ресурс не найден",
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			request := httptest.NewRequest(test.method, test.path, nil)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.wantStatus {
				t.Fatalf("ожидался код %d, получен %d", test.wantStatus, response.Code)
			}
			if body := response.Body.String(); !strings.Contains(body, test.wantText) {
				t.Fatalf("ответ не содержит русский контекст %q: %q", test.wantText, body)
			}
			if allow := response.Header().Get("Allow"); allow != test.wantAllow {
				t.Fatalf("заголовок Allow=%q, ожидался %q", allow, test.wantAllow)
			}
		})
	}
}

func TestOrderSnapshotHTTP(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 30, 15, 0, 0, 0, time.UTC)
	valid := engineOrderSnapshot(testOrderSaga(), orderStatusActive, now)
	validPayload, err := json.Marshal(valid)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name       string
		record     *domain.RuntimeRecord
		wantStatus int
		wantText   string
	}{
		{
			name: "success",
			record: &domain.RuntimeRecord{
				Key: orderSnapshotKey("execution"), Kind: orderSnapshotRecordKind,
				Payload: validPayload, UpdatedAt: now,
			},
			wantStatus: http.StatusOK,
			wantText:   `"market_order_id":"order-1"`,
		},
		{
			name:       "not found",
			wantStatus: http.StatusNotFound,
			wantText:   "снимок ордера не найден",
		},
		{
			name: "corrupt JSON",
			record: &domain.RuntimeRecord{
				Key: orderSnapshotKey("execution"), Kind: orderSnapshotRecordKind,
				Payload: []byte("{"), UpdatedAt: now,
			},
			wantStatus: http.StatusInternalServerError,
			wantText:   "не удалось декодировать снимок ордера",
		},
		{
			name: "wrong kind",
			record: &domain.RuntimeRecord{
				Key: orderSnapshotKey("execution"), Kind: "другой-тип",
				Payload: validPayload, UpdatedAt: now,
			},
			wantStatus: http.StatusInternalServerError,
			wantText:   "runtime-запись имеет тип",
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			store := repository.NewMemory()
			if test.record != nil {
				if err := store.SaveRuntimeRecord(
					context.Background(),
					*test.record,
				); err != nil {
					t.Fatal(err)
				}
			}
			handler := (&Engine{store: store}).Handler()
			request := httptest.NewRequest(
				http.MethodGet,
				"/api/v1/automation/orders/execution",
				nil,
			)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.wantStatus ||
				!strings.Contains(response.Body.String(), test.wantText) {
				t.Fatalf(
					"ответ status=%d body=%q, ожидались %d и %q",
					response.Code,
					response.Body.String(),
					test.wantStatus,
					test.wantText,
				)
			}
		})
	}
}

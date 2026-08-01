package session

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDecodeOptionalJSON(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		body    string
		wantID  string
		wantErr bool
	}{
		{name: "empty", body: ""},
		{name: "valid", body: `{"agent_id":"windows-1"}`, wantID: "windows-1"},
		{name: "whitespace after value", body: " {\"agent_id\":\"windows-1\"}\n\t", wantID: "windows-1"},
		{name: "unknown field", body: `{"unknown":true}`, wantErr: true},
		{name: "second object", body: `{"agent_id":"one"}{"agent_id":"two"}`, wantErr: true},
		{name: "trailing token", body: `{"agent_id":"one"} true`, wantErr: true},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			httpRequest := httptest.NewRequest("POST", "/api/v1/runtime/observe", strings.NewReader(test.body))
			var value request
			err := decodeOptionalJSON(httpRequest, &value)
			if test.wantErr && err == nil {
				t.Fatal("ожидалась ошибка")
			}
			if !test.wantErr && err != nil {
				t.Fatalf("неожиданная ошибка: %v", err)
			}
			if !test.wantErr && value.AgentID != test.wantID {
				t.Fatalf("agent_id=%q, ожидался %q", value.AgentID, test.wantID)
			}
		})
	}
}

func TestHandlerLocalizesRoutingErrors(t *testing.T) {
	t.Parallel()

	handler := (&Coordinator{}).Handler()
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
			method:     http.MethodPut,
			path:       "/api/v1/runtime",
			wantStatus: http.StatusMethodNotAllowed,
			wantText:   "session.Coordinator.methodNotAllowedHTTP: метод HTTP не поддерживается",
			wantAllow:  "GET, HEAD",
		},
		{
			name:       "not found",
			method:     http.MethodGet,
			path:       "/api/v1/runtime/неизвестно",
			wantStatus: http.StatusNotFound,
			wantText:   "session.Coordinator.notFoundHTTP: запрошенный ресурс не найден",
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

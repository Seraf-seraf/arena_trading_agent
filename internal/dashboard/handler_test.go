package dashboard

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandlerServesDashboard(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	response := httptest.NewRecorder()

	Handler().ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("ожидался код %d, получен %d", http.StatusOK, response.Code)
	}
	body := response.Body.String()
	for _, required := range []string{
		"Автоматизация и сделки",
		"/api/v1/automation",
		"/api/v1/automation/executions?limit=20",
		"renderSummary",
	} {
		if !strings.Contains(body, required) {
			t.Fatalf("страница панели не содержит %q", required)
		}
	}
}

func TestHandlerReturnsContextualRussianNotFound(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/неизвестно", nil)
	response := httptest.NewRecorder()

	Handler().ServeHTTP(response, request)

	if response.Code != http.StatusNotFound {
		t.Fatalf("ожидался код %d, получен %d", http.StatusNotFound, response.Code)
	}
	if body := response.Body.String(); !strings.Contains(body, "dashboard.Handler.ServeHTTP: запрошенная страница не найдена") {
		t.Fatalf("ответ не содержит русский контекст метода: %q", body)
	}
}

func TestHandlerReturnsContextualRussianMethodNotAllowed(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/dashboard", nil)
	response := httptest.NewRecorder()

	Handler().ServeHTTP(response, request)

	if response.Code != http.StatusMethodNotAllowed {
		t.Fatalf("ожидался код %d, получен %d", http.StatusMethodNotAllowed, response.Code)
	}
	if body := response.Body.String(); !strings.Contains(body, "dashboard.Handler.ServeHTTP: метод HTTP не поддерживается") {
		t.Fatalf("ответ не содержит русский контекст метода: %q", body)
	}
	if allow := response.Header().Get("Allow"); allow != "GET, HEAD" {
		t.Fatalf("заголовок Allow=%q, ожидался %q", allow, "GET, HEAD")
	}
}

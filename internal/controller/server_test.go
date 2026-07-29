package controller_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/arena-trading-agent/arena-trading-agent/internal/controller"
	"github.com/arena-trading-agent/arena-trading-agent/internal/protocol"
)

func TestAgentLifecycleIsReflectedInHealth(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	httpServer := httptest.NewServer(controller.NewServer(logger).Handler())
	defer httpServer.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	wsURL := "ws" + strings.TrimPrefix(httpServer.URL, "http") + "/ws/agent"
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("не удалось подключиться к тестовому серверу: %v", err)
	}

	hello, err := protocol.NewEnvelope(protocol.MessageHello, "hello-1", protocol.Hello{AgentID: "test-agent", Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if err := wsjson.Write(ctx, conn, hello); err != nil {
		t.Fatalf("не удалось отправить HELLO: %v", err)
	}
	waitForAgents(t, httpServer.URL, 1)

	if err := conn.Close(websocket.StatusNormalClosure, "тест завершён"); err != nil {
		t.Fatalf("не удалось закрыть соединение: %v", err)
	}
	waitForAgents(t, httpServer.URL, 0)
}

func waitForAgents(t *testing.T, serverURL string, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		response, err := http.Get(serverURL + "/healthz")
		if err != nil {
			t.Fatalf("GET /healthz: %v", err)
		}
		var health struct {
			Agents int `json:"agents"`
		}
		err = json.NewDecoder(response.Body).Decode(&health)
		response.Body.Close()
		if err != nil {
			t.Fatalf("не удалось прочитать healthz: %v", err)
		}
		if health.Agents == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("число подключённых агентов не стало равным %d", want)
}

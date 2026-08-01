package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
)

const maxRuntimeRequestBody = 1 << 20

type request struct {
	AgentID string `json:"agent_id,omitempty"`
}

// Handler exposes observation and preflight without exposing input.
func (c *Coordinator) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/runtime", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, c.Snapshot())
	})
	mux.HandleFunc("POST /api/v1/runtime/observe", c.observeHTTP)
	mux.HandleFunc("POST /api/v1/runtime/preflight", c.preflightHTTP)
	mux.HandleFunc("/api/v1/runtime", c.methodNotAllowedHTTP)
	mux.HandleFunc("/api/v1/runtime/observe", c.methodNotAllowedHTTP)
	mux.HandleFunc("/api/v1/runtime/preflight", c.methodNotAllowedHTTP)
	mux.HandleFunc("/", c.notFoundHTTP)
	return mux
}

func (c *Coordinator) methodNotAllowedHTTP(w http.ResponseWriter, request *http.Request) {
	const methodCtx = "session.Coordinator.methodNotAllowedHTTP"

	allowed := http.MethodPost
	if request.URL.Path == "/api/v1/runtime" {
		allowed = "GET, HEAD"
	}
	w.Header().Set("Allow", allowed)
	writeJSON(w, http.StatusMethodNotAllowed, map[string]string{
		"error": methodCtx + ": метод HTTP не поддерживается",
	})
}

func (c *Coordinator) notFoundHTTP(w http.ResponseWriter, _ *http.Request) {
	const methodCtx = "session.Coordinator.notFoundHTTP"

	writeJSON(w, http.StatusNotFound, map[string]string{
		"error": methodCtx + ": запрошенный ресурс не найден",
	})
}

func (c *Coordinator) observeHTTP(w http.ResponseWriter, httpRequest *http.Request) {
	const methodCtx = "session.Coordinator.observeHTTP"

	var value request
	if err := decodeOptionalJSON(httpRequest, &value); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": fmt.Sprintf("%s: не удалось разобрать запрос наблюдения: %v", methodCtx, err),
		})
		return
	}
	observation, err := c.Observe(httpRequest.Context(), value.AgentID)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{
			"error": fmt.Sprintf("%s: наблюдение завершилось ошибкой: %v", methodCtx, err),
		})
		return
	}
	writeJSON(w, http.StatusOK, observation)
}

func (c *Coordinator) preflightHTTP(w http.ResponseWriter, httpRequest *http.Request) {
	const methodCtx = "session.Coordinator.preflightHTTP"

	var value request
	if err := decodeOptionalJSON(httpRequest, &value); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": fmt.Sprintf("%s: не удалось разобрать запрос предварительной проверки: %v", methodCtx, err),
		})
		return
	}
	trade, _ := strconv.ParseBool(httpRequest.URL.Query().Get("trade"))
	result := c.Preflight(httpRequest.Context(), value.AgentID, trade)
	status := http.StatusOK
	if trade && !result.TradeReady {
		status = http.StatusConflict
	}
	writeJSON(w, status, result)
}

func decodeOptionalJSON(httpRequest *http.Request, destination any) error {
	const methodCtx = "session.decodeOptionalJSON"

	if httpRequest.Body == nil || httpRequest.ContentLength == 0 {
		return nil
	}
	body := http.MaxBytesReader(nil, httpRequest.Body, maxRuntimeRequestBody)
	decoder := json.NewDecoder(body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return fmt.Errorf("%s: некорректный JSON: %w", methodCtx, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err != nil {
			return fmt.Errorf("%s: не удалось проверить данные после JSON: %w", methodCtx, err)
		}
		return fmt.Errorf("%s: лишние данные после JSON", methodCtx)
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

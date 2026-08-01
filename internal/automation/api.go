package automation

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/arena-trading-agent/arena-trading-agent/internal/domain"
	"github.com/arena-trading-agent/arena-trading-agent/internal/repository"
)

// Handler exposes read-only automation state. Mode changes remain centralized
// in controller's preflight-protected /api/v1/mode endpoint.
func (e *Engine) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/automation", func(w http.ResponseWriter, _ *http.Request) {
		writeAutomationJSON(w, http.StatusOK, e.Snapshot())
	})
	mux.HandleFunc("GET /api/v1/automation/evaluation", e.evaluationHTTP)
	mux.HandleFunc("GET /api/v1/automation/executions", e.executionsHTTP)
	mux.HandleFunc("GET /api/v1/automation/checkpoints/{executionID}", e.checkpointHTTP)
	mux.HandleFunc("GET /api/v1/automation/orders/{executionID}", e.orderSnapshotHTTP)
	mux.HandleFunc("/api/v1/automation", e.methodNotAllowedHTTP)
	mux.HandleFunc("/api/v1/automation/evaluation", e.methodNotAllowedHTTP)
	mux.HandleFunc("/api/v1/automation/executions", e.methodNotAllowedHTTP)
	mux.HandleFunc("/api/v1/automation/checkpoints/{executionID}", e.methodNotAllowedHTTP)
	mux.HandleFunc("/api/v1/automation/orders/{executionID}", e.methodNotAllowedHTTP)
	mux.HandleFunc("/", e.notFoundHTTP)
	return mux
}

func (e *Engine) methodNotAllowedHTTP(w http.ResponseWriter, _ *http.Request) {
	const methodCtx = "automation.Engine.methodNotAllowedHTTP"

	w.Header().Set("Allow", "GET, HEAD")
	writeAutomationJSON(w, http.StatusMethodNotAllowed, map[string]string{
		"error": methodCtx + ": метод HTTP не поддерживается",
	})
}

func (e *Engine) notFoundHTTP(w http.ResponseWriter, _ *http.Request) {
	const methodCtx = "automation.Engine.notFoundHTTP"

	writeAutomationJSON(w, http.StatusNotFound, map[string]string{
		"error": methodCtx + ": запрошенный ресурс не найден",
	})
}

func (e *Engine) evaluationHTTP(w http.ResponseWriter, request *http.Request) {
	const methodCtx = "automation.Engine.evaluationHTTP"

	record, err := e.store.RuntimeRecord(request.Context(), evaluationKey)
	if errors.Is(err, repository.ErrNotFound) {
		writeAutomationJSON(w, http.StatusNotFound, map[string]string{"error": methodCtx + ": оценка возможностей ещё не выполнялась"})
		return
	}
	if err != nil {
		writeAutomationJSON(w, http.StatusInternalServerError, map[string]string{
			"error": fmt.Sprintf("%s: не удалось прочитать сохранённую оценку: %v", methodCtx, err),
		})
		return
	}
	var value any
	if err := json.Unmarshal(record.Payload, &value); err != nil {
		writeAutomationJSON(w, http.StatusInternalServerError, map[string]string{
			"error": fmt.Sprintf("%s: не удалось декодировать сохранённую оценку: %v", methodCtx, err),
		})
		return
	}
	writeAutomationJSON(w, http.StatusOK, map[string]any{
		"updated_at": record.UpdatedAt,
		"evaluation": value,
	})
}

func (e *Engine) executionsHTTP(w http.ResponseWriter, request *http.Request) {
	const methodCtx = "automation.Engine.executionsHTTP"

	limit := 100
	if raw := request.URL.Query().Get("limit"); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 || value > 1000 {
			writeAutomationJSON(w, http.StatusBadRequest, map[string]string{"error": methodCtx + ": параметр limit должен быть в диапазоне 1..1000"})
			return
		}
		limit = value
	}
	values, err := e.store.ListExecutions(request.Context(), domain.ExecutionFilter{Limit: limit})
	if err != nil {
		writeAutomationJSON(w, http.StatusInternalServerError, map[string]string{
			"error": fmt.Sprintf("%s: не удалось получить исполнения сделок: %v", methodCtx, err),
		})
		return
	}
	writeAutomationJSON(w, http.StatusOK, map[string]any{"executions": values})
}

func (e *Engine) checkpointHTTP(w http.ResponseWriter, request *http.Request) {
	const methodCtx = "automation.Engine.checkpointHTTP"

	executionID := request.PathValue("executionID")
	if executionID == "" {
		writeAutomationJSON(w, http.StatusBadRequest, map[string]string{"error": methodCtx + ": параметр executionID обязателен"})
		return
	}
	record, err := e.store.RuntimeRecord(request.Context(), "saga/"+executionID)
	if errors.Is(err, repository.ErrNotFound) {
		writeAutomationJSON(w, http.StatusNotFound, map[string]string{"error": methodCtx + ": контрольная точка не найдена"})
		return
	}
	if err != nil {
		writeAutomationJSON(w, http.StatusInternalServerError, map[string]string{
			"error": fmt.Sprintf("%s: не удалось прочитать контрольную точку: %v", methodCtx, err),
		})
		return
	}
	var value any
	if err := json.Unmarshal(record.Payload, &value); err != nil {
		writeAutomationJSON(w, http.StatusInternalServerError, map[string]string{
			"error": fmt.Sprintf("%s: не удалось декодировать контрольную точку: %v", methodCtx, err),
		})
		return
	}
	writeAutomationJSON(w, http.StatusOK, value)
}

func (e *Engine) orderSnapshotHTTP(w http.ResponseWriter, request *http.Request) {
	const methodCtx = "automation.Engine.orderSnapshotHTTP"

	executionID := strings.TrimSpace(request.PathValue("executionID"))
	if executionID == "" {
		writeAutomationJSON(w, http.StatusBadRequest, map[string]string{
			"error": methodCtx + ": параметр executionID обязателен",
		})
		return
	}
	record, err := e.store.RuntimeRecord(
		request.Context(),
		orderSnapshotKey(executionID),
	)
	if errors.Is(err, repository.ErrNotFound) {
		writeAutomationJSON(w, http.StatusNotFound, map[string]string{
			"error": methodCtx + ": снимок ордера не найден",
		})
		return
	}
	if err != nil {
		writeAutomationJSON(w, http.StatusInternalServerError, map[string]string{
			"error": fmt.Sprintf("%s: не удалось прочитать снимок ордера: %v", methodCtx, err),
		})
		return
	}
	if record.Kind != orderSnapshotRecordKind {
		writeAutomationJSON(w, http.StatusInternalServerError, map[string]string{
			"error": fmt.Sprintf(
				"%s: runtime-запись имеет тип %q вместо %q",
				methodCtx,
				record.Kind,
				orderSnapshotRecordKind,
			),
		})
		return
	}
	var snapshot OrderSnapshot
	if err := json.Unmarshal(record.Payload, &snapshot); err != nil {
		writeAutomationJSON(w, http.StatusInternalServerError, map[string]string{
			"error": fmt.Sprintf("%s: не удалось декодировать снимок ордера: %v", methodCtx, err),
		})
		return
	}
	if err := validateOrderSnapshot(snapshot, nil); err != nil {
		writeAutomationJSON(w, http.StatusInternalServerError, map[string]string{
			"error": fmt.Sprintf("%s: сохранённый снимок ордера некорректен: %v", methodCtx, err),
		})
		return
	}
	if snapshot.SagaID != executionID {
		writeAutomationJSON(w, http.StatusInternalServerError, map[string]string{
			"error": fmt.Sprintf(
				"%s: снимок относится к саге %q вместо %q",
				methodCtx,
				snapshot.SagaID,
				executionID,
			),
		})
		return
	}
	writeAutomationJSON(w, http.StatusOK, snapshot)
}

func writeAutomationJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

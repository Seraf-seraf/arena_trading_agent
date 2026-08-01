package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/arena-trading-agent/arena-trading-agent/internal/domain"
	"github.com/arena-trading-agent/arena-trading-agent/internal/protocol"
)

const maxAPIRequestBytes = 1 << 20

// APICommandRequest — JSON-контракт POST /api/v1/commands.
//
// Допустимые Type: FRAME_REQUEST, FRAME_REGION_REQUEST,
// WINDOW_STATUS_REQUEST и EMERGENCY_STOP. AgentID можно опустить, если
// подключён ровно один агент; в nested endpoint он берётся из path.
// ACTION_REQUEST намеренно недоступен: только внутренние типизированные
// исполнители могут дойти до Server.RequestAction.
type APICommandRequest struct {
	AgentID    string                  `json:"agent_id,omitempty"`
	Type       protocol.MessageType    `json:"type"`
	RequestID  string                  `json:"request_id,omitempty"`
	TimeoutMS  int                     `json:"timeout_ms,omitempty"`
	AfterFrame uint64                  `json:"after_frame,omitempty"`
	Region     *domain.Rectangle       `json:"region,omitempty"`
	Action     *protocol.ActionRequest `json:"action,omitempty"`
	Reason     string                  `json:"reason,omitempty"`
}

// APICommandResponse сохраняет transport request id и типизированный result.
type APICommandResponse struct {
	RequestID string `json:"request_id,omitempty"`
	Type      string `json:"type"`
	Result    any    `json:"result,omitempty"`
	Sent      bool   `json:"sent,omitempty"`
}

type modeRequest struct {
	Mode domain.AgentMode `json:"mode"`
}

func (s *Server) runtimeState(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.State())
}

func (s *Server) agentStateHTTP(w http.ResponseWriter, request *http.Request) {
	const methodCtx = "controller.Server.agentStateHTTP"

	state, ok := s.AgentState(request.PathValue("agentID"))
	if !ok {
		writeAPIError(w, fmt.Errorf("%s: состояние агента недоступно: %w", methodCtx, ErrAgentNotConnected))
		return
	}
	writeJSON(w, http.StatusOK, state)
}

func (s *Server) latestFrameHTTP(w http.ResponseWriter, request *http.Request) {
	const methodCtx = "controller.Server.latestFrameHTTP"

	agentID, err := s.resolveAgentID(request.PathValue("agentID"), request.URL.Query().Get("agent_id"))
	if err != nil {
		writeAPIError(w, err)
		return
	}
	frame, ok := s.LatestFrame(agentID)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": methodCtx + ": для агента ещё нет кадра"})
		return
	}

	raw, _ := strconv.ParseBool(request.URL.Query().Get("raw"))
	if raw || strings.Contains(request.Header.Get("Accept"), "image/") {
		contentType := "application/octet-stream"
		switch strings.ToLower(frame.Encoding) {
		case "jpeg", "jpg":
			contentType = "image/jpeg"
		case "png":
			contentType = "image/png"
		case "webp":
			contentType = "image/webp"
		}
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("X-Frame-ID", strconv.FormatUint(frame.ID, 10))
		w.Header().Set("X-Frame-Captured-At", frame.CapturedAt.Format(time.RFC3339Nano))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(frame.Data)
		return
	}
	writeJSON(w, http.StatusOK, frame)
}

func (s *Server) modeHTTP(w http.ResponseWriter, request *http.Request) {
	if request.Method == http.MethodGet {
		state := s.State()
		writeJSON(w, http.StatusOK, map[string]any{
			"mode":            state.Mode,
			"mode_updated_at": state.ModeUpdatedAt,
		})
		return
	}

	var value modeRequest
	if err := decodeJSON(w, request, &value); err != nil {
		writeAPIError(w, err)
		return
	}
	if err := s.SetModeContext(request.Context(), value.Mode); err != nil {
		writeAPIError(w, err)
		return
	}
	state := s.State()
	writeJSON(w, http.StatusOK, map[string]any{
		"mode":            state.Mode,
		"mode_updated_at": state.ModeUpdatedAt,
	})
}

func (s *Server) commandHTTP(w http.ResponseWriter, request *http.Request) {
	const methodCtx = "controller.Server.commandHTTP"

	var command APICommandRequest
	if err := decodeJSON(w, request, &command); err != nil {
		writeAPIError(w, err)
		return
	}
	agentID, err := s.resolveAgentID(request.PathValue("agentID"), command.AgentID)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	if command.TimeoutMS < 0 || command.TimeoutMS > 60_000 {
		writeAPIError(w, fmt.Errorf("%s: timeout_ms должен быть в диапазоне 0..60000", methodCtx))
		return
	}

	ctx := request.Context()
	cancel := func() {}
	if command.TimeoutMS > 0 {
		ctx, cancel = context.WithTimeout(ctx, time.Duration(command.TimeoutMS)*time.Millisecond)
	}
	defer cancel()

	response, err := s.executeAPICommand(ctx, agentID, command)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) executeAPICommand(ctx context.Context, agentID string, command APICommandRequest) (APICommandResponse, error) {
	const methodCtx = "controller.Server.executeAPICommand"

	switch command.Type {
	case protocol.MessageFrameRequest:
		if command.Region != nil {
			return APICommandResponse{}, fmt.Errorf("%s: FRAME_REQUEST не принимает region", methodCtx)
		}
		frame, err := s.requestFrame(ctx, agentID, command.RequestID, protocol.FrameRequest{AfterFrame: command.AfterFrame})
		if err != nil {
			return APICommandResponse{}, fmt.Errorf("%s: запрос кадра завершился ошибкой: %w", methodCtx, err)
		}
		return APICommandResponse{RequestID: command.RequestID, Type: string(protocol.MessageFrame), Result: frame}, nil

	case protocol.MessageFrameRegionRequest:
		if command.Region == nil || !validRectangle(*command.Region) {
			return APICommandResponse{}, fmt.Errorf("%s: FRAME_REGION_REQUEST требует корректную область", methodCtx)
		}
		frame, err := s.requestFrame(ctx, agentID, command.RequestID, protocol.FrameRequest{
			AfterFrame: command.AfterFrame,
			Region:     command.Region,
		})
		if err != nil {
			return APICommandResponse{}, fmt.Errorf("%s: запрос области кадра завершился ошибкой: %w", methodCtx, err)
		}
		return APICommandResponse{RequestID: command.RequestID, Type: string(protocol.MessageFrameRegion), Result: frame}, nil

	case protocol.MessageWindowStatusRequest:
		status, err := s.requestWindowStatus(ctx, agentID, command.RequestID)
		if err != nil {
			return APICommandResponse{}, fmt.Errorf("%s: запрос состояния окна завершился ошибкой: %w", methodCtx, err)
		}
		return APICommandResponse{RequestID: command.RequestID, Type: string(protocol.MessageWindowStatus), Result: status}, nil

	case protocol.MessageActionRequest:
		return APICommandResponse{}, fmt.Errorf("%s: команда отклонена: %w", methodCtx, ErrRawActionAPIDisabled)

	case protocol.MessageEmergencyStop:
		err := s.SendEmergencyStop(ctx, agentID, command.Reason)
		if err != nil {
			return APICommandResponse{}, fmt.Errorf("%s: аварийная остановка завершилась ошибкой: %w", methodCtx, err)
		}
		return APICommandResponse{RequestID: command.RequestID, Type: string(protocol.MessageEmergencyStop), Sent: true}, nil

	default:
		return APICommandResponse{}, fmt.Errorf("%s: тип %q нельзя отправить через API команд", methodCtx, command.Type)
	}
}

func (s *Server) resolveAgentID(values ...string) (string, error) {
	const methodCtx = "controller.Server.resolveAgentID"

	for _, value := range values {
		if value != "" {
			if _, ok := s.findAgent(value); !ok {
				return "", fmt.Errorf("%s: агент %q не найден: %w", methodCtx, value, ErrAgentNotConnected)
			}
			return value, nil
		}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if len(s.agents) == 0 {
		return "", fmt.Errorf("%s: нет подключённых агентов: %w", methodCtx, ErrAgentNotConnected)
	}
	if len(s.agents) != 1 {
		return "", fmt.Errorf("%s: поле agent_id обязательно при нескольких подключениях", methodCtx)
	}
	for agentID := range s.agents {
		return agentID, nil
	}
	panic(methodCtx + ": достигнуто недостижимое состояние")
}

func decodeJSON(w http.ResponseWriter, request *http.Request, destination any) error {
	const methodCtx = "controller.decodeJSON"

	request.Body = http.MaxBytesReader(w, request.Body, maxAPIRequestBytes)
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return fmt.Errorf("%s: некорректный JSON: %w", methodCtx, err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return fmt.Errorf("%s: после JSON-объекта присутствуют лишние данные", methodCtx)
	}
	return nil
}

func writeAPIError(w http.ResponseWriter, err error) {
	status := http.StatusBadRequest
	switch {
	case errors.Is(err, ErrAgentNotConnected):
		status = http.StatusNotFound
	case errors.Is(err, ErrModeDisallowsInput), errors.Is(err, ErrModeAuthorization),
		errors.Is(err, ErrModeDisallowsMoney), errors.Is(err, ErrRequestIDInUse):
		status = http.StatusConflict
	case errors.Is(err, ErrRawActionAPIDisabled):
		status = http.StatusForbidden
	case errors.Is(err, context.DeadlineExceeded):
		status = http.StatusGatewayTimeout
	case errors.Is(err, ErrAgentReplaced), errors.Is(err, ErrUnexpectedResponse):
		status = http.StatusBadGateway
	}
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

// Package controller реализует транспортный контур главного процесса.
package controller

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/arena-trading-agent/arena-trading-agent/internal/protocol"
)

const heartbeatTimeout = 15 * time.Second

// Server принимает подключения Windows Agent и отслеживает их состояние.
type Server struct {
	logger *slog.Logger
	mu     sync.RWMutex
	agents map[string]time.Time
}

// NewServer создаёт транспортный сервер контроллера.
func NewServer(logger *slog.Logger) *Server {
	return &Server{logger: logger, agents: make(map[string]time.Time)}
}

// Handler возвращает HTTP-маршруты контроллера.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.health)
	mux.HandleFunc("GET /ws/agent", s.agent)
	return mux
}

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	s.mu.RLock()
	connected := len(s.agents)
	s.mu.RUnlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok", "agents": connected})
}

func (s *Server) agent(w http.ResponseWriter, r *http.Request) {
	logger := s.logger.With("метод", "Server.agent")
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: []string{"localhost:*", "127.0.0.1:*"}})
	if err != nil {
		logger.Error("не удалось принять подключение агента", "ошибка", err)
		return
	}
	defer conn.CloseNow()

	ctx := r.Context()
	var agentID string
	for {
		readCtx, cancel := context.WithTimeout(ctx, heartbeatTimeout)
		var envelope protocol.Envelope
		err = wsjson.Read(readCtx, conn, &envelope)
		cancel()
		if err != nil {
			if !errors.Is(err, context.Canceled) {
				logger.Warn("соединение с агентом закрыто", "agent_id", agentID, "ошибка", err)
			}
			break
		}

		switch envelope.Type {
		case protocol.MessageHello:
			var hello protocol.Hello
			if err := json.Unmarshal(envelope.Payload, &hello); err != nil || hello.AgentID == "" {
				logger.Warn("получено некорректное приветствие", "ошибка", err)
				continue
			}
			agentID = hello.AgentID
			s.touch(agentID)
			logger.Info("агент подключён", "agent_id", agentID, "версия", hello.Version)
		case protocol.MessageHeartbeat:
			if agentID == "" {
				logger.Warn("heartbeat получен до приветствия")
				continue
			}
			s.touch(agentID)
		default:
			logger.Debug("получено сообщение агента", "тип", envelope.Type, "agent_id", agentID)
		}
	}
	if agentID != "" {
		s.mu.Lock()
		delete(s.agents, agentID)
		s.mu.Unlock()
	}
}

func (s *Server) touch(agentID string) {
	s.mu.Lock()
	s.agents[agentID] = time.Now().UTC()
	s.mu.Unlock()
}

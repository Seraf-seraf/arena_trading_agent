// Package controller реализует транспортный контур главного процесса.
package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/arena-trading-agent/arena-trading-agent/internal/domain"
	"github.com/arena-trading-agent/arena-trading-agent/internal/protocol"
)

const (
	defaultHeartbeatTimeout = 15 * time.Second
	defaultRequestTimeout   = 8 * time.Second
	defaultAuditTimeout     = 2 * time.Second
	maxRememberedEvents     = 100
)

var (
	// ErrAgentNotConnected означает, что команда не может быть маршрутизирована.
	ErrAgentNotConnected = errors.New("Windows-агент не подключён")
	// ErrAgentReplaced завершает ожидания старого подключения при reconnect.
	ErrAgentReplaced = errors.New("подключение Windows-агента заменено")
	// ErrModeDisallowsInput не даёт OBSERVE, SIMULATE и PAUSED отправить ввод.
	ErrModeDisallowsInput = errors.New("текущий режим запрещает ввод")
	// ErrModeAuthorization означает, что SCAN/TRADE не прошёл свежий
	// production preflight либо его результат устарел до открытия input gate.
	ErrModeAuthorization = errors.New("режим с вводом не прошёл авторизацию")
	// ErrRequestIDInUse защищает correlation map от неоднозначных ответов.
	ErrRequestIDInUse = errors.New("идентификатор запроса уже используется")
	// ErrUnexpectedResponse означает нарушение duplex-протокола агентом.
	ErrUnexpectedResponse = errors.New("получен неожиданный ответ агента")
	// ErrAuditPersistence означает, что controller не смог надёжно записать
	// ACTION_REQUEST и поэтому не отправил потенциально денежное действие.
	ErrAuditPersistence = errors.New("не удалось сохранить протокольный аудит")
	// ErrRawActionAPIDisabled не позволяет dashboard или произвольному HTTP
	// клиенту обойти Navigator/TradeExecutor и напрямую управлять мышью.
	ErrRawActionAPIDisabled = errors.New("ACTION_REQUEST через commands API отключён")
	// ErrModeDisallowsMoney separates harmless UI scanning from inventory and
	// money mutations even though both modes technically permit input.
	ErrModeDisallowsMoney = errors.New("текущий режим запрещает денежное действие")
	// ErrEvidencePersistence means an action may have happened but its exact
	// verification frame could not be written durably. Further input is unsafe.
	ErrEvidencePersistence = errors.New("не удалось сохранить контрольный кадр")
	// ErrActionNotSent подтверждает, что защищённая команда была отклонена
	// локально до первой попытки записи в WebSocket.
	ErrActionNotSent = errors.New("команда гарантированно не отправлена агенту")
)

// AuditStore is the narrow durable-journal contract used by the transport
// runtime. repository.Store satisfies it without creating a package cycle.
type AuditStore interface {
	SaveAction(context.Context, domain.ActionRecord) error
	SaveActionResult(context.Context, domain.ActionResultRecord) error
	SaveEvent(context.Context, domain.AgentEventRecord) error
}

// ModeAuthorizer performs the production preflight required before an
// input-capable mode can be entered.
type ModeAuthorizer func(context.Context, domain.AgentMode) error

// ActionFrameSink durably records the exact frame returned with ACTION_RESULT.
type ActionFrameSink func(context.Context, string, string, protocol.Frame) error

// Option настраивает runtime сервера. Опции главным образом нужны тестам и
// embedding-сценариям; production defaults согласованы с heartbeat агента.
type Option func(*Server)

// WithHeartbeatTimeout меняет время, после которого агент без HEARTBEAT
// отключается, даже если он продолжает отправлять другие сообщения.
func WithHeartbeatTimeout(timeout time.Duration) Option {
	return func(server *Server) {
		if timeout > 0 {
			server.heartbeatTimeout = timeout
		}
	}
}

// WithRequestTimeout меняет default deadline синхронной команды.
func WithRequestTimeout(timeout time.Duration) Option {
	return func(server *Server) {
		if timeout > 0 {
			server.requestTimeout = timeout
		}
	}
}

// WithAuditTimeout bounds an inbound journal write so a broken storage
// backend cannot indefinitely stall the WebSocket read loop.
func WithAuditTimeout(timeout time.Duration) Option {
	return func(server *Server) {
		if timeout > 0 {
			server.auditTimeout = timeout
		}
	}
}

// WithAuditStore enables durable protocol journaling. ACTION_REQUEST is
// persisted before it reaches the wire; inbound outcomes and events are
// persisted without turning a storage failure into a protocol violation.
func WithAuditStore(store AuditStore) Option {
	return func(server *Server) {
		server.auditStore = store
	}
}

// WithActionFrameSink enables mandatory post-action visual evidence.
func WithActionFrameSink(sink ActionFrameSink) Option {
	return func(server *Server) {
		server.actionFrameSink = sink
	}
}

// Server принимает подключения Windows Agent, маршрутизирует команды и хранит
// последний безопасный снимок runtime для API/dashboard.
type Server struct {
	logger           *slog.Logger
	heartbeatTimeout time.Duration
	requestTimeout   time.Duration
	auditTimeout     time.Duration
	auditStore       AuditStore
	actionFrameSink  ActionFrameSink
	sequence         atomic.Uint64

	actionGate       sync.RWMutex
	guardedSendMu    sync.Mutex
	guardedSendSeq   uint64
	guardedSendStops map[uint64]context.CancelFunc
	modeTransitionMu sync.Mutex
	modeAuthorizerMu sync.RWMutex
	mu               sync.RWMutex
	agents           map[string]*agentConnection
	mode             domain.AgentMode
	modeUpdatedAt    time.Time
	modeAuthorizer   ModeAuthorizer
	safetyEpoch      atomic.Uint64
}

// AgentState — небольшой immutable snapshot подключения без изображения кадра.
type AgentState struct {
	AgentID          string                 `json:"agent_id"`
	Version          string                 `json:"version"`
	Features         []string               `json:"features,omitempty"`
	ConnectedAt      time.Time              `json:"connected_at"`
	LastHeartbeatAt  time.Time              `json:"last_heartbeat_at"`
	LastMessageAt    time.Time              `json:"last_message_at"`
	Window           *protocol.WindowStatus `json:"window,omitempty"`
	LatestFrame      *FrameSummary          `json:"latest_frame,omitempty"`
	LastEvent        *protocol.AgentEvent   `json:"last_event,omitempty"`
	EventCount       uint64                 `json:"event_count"`
	AutomationPaused bool                   `json:"automation_paused"`
	EmergencyStopped bool                   `json:"emergency_stopped"`
	SafetyReason     string                 `json:"safety_reason,omitempty"`
}

// FrameSummary исключает тяжёлое поле Data из общего состояния.
type FrameSummary struct {
	ID            uint64           `json:"id"`
	CapturedAt    time.Time        `json:"captured_at"`
	ContentDigest string           `json:"content_digest,omitempty"`
	Region        domain.Rectangle `json:"region"`
	Encoding      string           `json:"encoding"`
	Size          int              `json:"size"`
}

// RuntimeState — контракт GET /api/v1/state.
type RuntimeState struct {
	Mode          domain.AgentMode `json:"mode"`
	ModeUpdatedAt time.Time        `json:"mode_updated_at"`
	Agents        []AgentState     `json:"agents"`
}

type pendingResponse struct {
	envelope protocol.Envelope
	err      error
}

type agentConnection struct {
	server      *Server
	conn        *websocket.Conn
	id          string
	version     string
	features    []string
	connectedAt time.Time

	stateMu          sync.RWMutex
	lastHeartbeatAt  time.Time
	lastMessageAt    time.Time
	window           *protocol.WindowStatus
	latestFrame      *protocol.Frame
	events           []protocol.AgentEvent
	eventCount       uint64
	automationPaused bool
	emergencyStopped bool
	safetyReason     string

	// writeGate сериализует WebSocket writes и, в отличие от sync.Mutex,
	// позволяет ожидающему ACTION_REQUEST немедленно выйти по отмене safety.
	writeGate chan struct{}

	pendingMu sync.Mutex
	pending   map[string]chan pendingResponse
	// pendingTypes хранит единственный допустимый тип ответа для каждого
	// коррелированного запроса и не содержит payload.
	pendingTypes map[string]protocol.MessageType

	closeOnce sync.Once
	closed    chan struct{}
}

// NewServer создаёт duplex transport server. Начальный режим всегда PAUSED:
// одного подключения агента недостаточно для разрешения ввода.
func NewServer(logger *slog.Logger, options ...Option) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	now := time.Now().UTC()
	server := &Server{
		logger:           logger,
		heartbeatTimeout: defaultHeartbeatTimeout,
		requestTimeout:   defaultRequestTimeout,
		auditTimeout:     defaultAuditTimeout,
		agents:           make(map[string]*agentConnection),
		guardedSendStops: make(map[uint64]context.CancelFunc),
		mode:             domain.ModePaused,
		modeUpdatedAt:    now,
	}
	for _, option := range options {
		option(server)
	}
	return server
}

// Handler возвращает HTTP/WebSocket маршруты контроллера.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.health)
	mux.HandleFunc("GET /ws/agent", s.agent)
	mux.HandleFunc("/healthz", s.methodNotAllowed)
	mux.HandleFunc("/ws/agent", s.methodNotAllowed)

	mux.HandleFunc("GET /api/v1/state", s.runtimeState)
	mux.HandleFunc("GET /api/v1/frame", s.latestFrameHTTP)
	mux.HandleFunc("POST /api/v1/commands", s.commandHTTP)
	mux.HandleFunc("GET /api/v1/mode", s.modeHTTP)
	mux.HandleFunc("PUT /api/v1/mode", s.modeHTTP)
	mux.HandleFunc("POST /api/v1/mode", s.modeHTTP)
	mux.HandleFunc("/api/v1/state", s.methodNotAllowed)
	mux.HandleFunc("/api/v1/frame", s.methodNotAllowed)
	mux.HandleFunc("/api/v1/commands", s.methodNotAllowed)
	mux.HandleFunc("/api/v1/mode", s.methodNotAllowed)

	mux.HandleFunc("GET /api/v1/agents/{agentID}", s.agentStateHTTP)
	mux.HandleFunc("GET /api/v1/agents/{agentID}/frame", s.latestFrameHTTP)
	mux.HandleFunc("POST /api/v1/agents/{agentID}/commands", s.commandHTTP)
	mux.HandleFunc("/api/v1/agents/{agentID}", s.methodNotAllowed)
	mux.HandleFunc("/api/v1/agents/{agentID}/frame", s.methodNotAllowed)
	mux.HandleFunc("/api/v1/agents/{agentID}/commands", s.methodNotAllowed)
	mux.HandleFunc("/", s.notFound)
	return mux
}

func (s *Server) methodNotAllowed(w http.ResponseWriter, request *http.Request) {
	const methodCtx = "controller.Server.methodNotAllowed"

	writeJSON(w, http.StatusMethodNotAllowed, map[string]string{
		"error": fmt.Sprintf(
			"%s: HTTP-метод %s не поддерживается для пути %s",
			methodCtx,
			request.Method,
			request.URL.Path,
		),
	})
}

func (s *Server) notFound(w http.ResponseWriter, request *http.Request) {
	const methodCtx = "controller.Server.notFound"

	writeJSON(w, http.StatusNotFound, map[string]string{
		"error": fmt.Sprintf(
			"%s: HTTP-маршрут %s не найден",
			methodCtx,
			request.URL.Path,
		),
	})
}

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	state := s.State()
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "ok",
		"agents": len(state.Agents),
		"mode":   state.Mode,
	})
}

func (s *Server) agent(w http.ResponseWriter, request *http.Request) {
	const methodCtx = "controller.Server.agent"
	logger := s.logger.With("метод", methodCtx)
	conn, err := websocket.Accept(w, request, &websocket.AcceptOptions{
		OriginPatterns: []string{"localhost:*", "127.0.0.1:*"},
	})
	if err != nil {
		logger.Error("не удалось принять подключение агента", "ошибка", err)
		return
	}
	defer conn.CloseNow()
	conn.SetReadLimit(protocol.MaxTransportMessageBytes)

	ctx := request.Context()
	handshakeCtx, cancel := context.WithTimeout(ctx, s.heartbeatTimeout)
	var envelope protocol.Envelope
	err = wsjson.Read(handshakeCtx, conn, &envelope)
	cancel()
	if err != nil {
		logger.Warn("агент не выполнил первоначальный обмен сообщениями", "ошибка", err)
		return
	}
	if envelope.Type != protocol.MessageHello {
		_ = conn.Close(websocket.StatusPolicyViolation, methodCtx+": первым сообщением должен быть HELLO")
		return
	}
	var hello protocol.Hello
	if err := json.Unmarshal(envelope.Payload, &hello); err != nil || hello.AgentID == "" {
		_ = conn.Close(websocket.StatusPolicyViolation, methodCtx+": некорректный HELLO")
		return
	}

	now := time.Now().UTC()
	agent := &agentConnection{
		server:           s,
		conn:             conn,
		id:               hello.AgentID,
		version:          hello.Version,
		features:         append([]string(nil), hello.Features...),
		connectedAt:      now,
		lastHeartbeatAt:  now,
		lastMessageAt:    now,
		automationPaused: hello.AutomationPaused,
		emergencyStopped: hello.EmergencyStopped,
		safetyReason:     hello.SafetyReason,
		pending:          make(map[string]chan pendingResponse),
		pendingTypes:     make(map[string]protocol.MessageType),
		writeGate:        make(chan struct{}, 1),
		closed:           make(chan struct{}),
	}
	s.register(agent)
	logger.Info("агент подключён", "идентификатор_агента", agent.id, "версия", agent.version)

	var disconnectErr error
	defer func() {
		if disconnectErr == nil {
			disconnectErr = ErrAgentNotConnected
		}
		s.unregister(agent, disconnectErr)
	}()

	for {
		if !s.isCurrent(agent) {
			disconnectErr = ErrAgentReplaced
			return
		}
		deadline := agent.heartbeatDeadline(s.heartbeatTimeout)
		readCtx, readCancel := context.WithDeadline(ctx, deadline)
		envelope = protocol.Envelope{}
		err = wsjson.Read(readCtx, conn, &envelope)
		readCancel()
		if err != nil {
			if time.Now().After(deadline) {
				disconnectErr = fmt.Errorf(
					"%s: превышено время ожидания сигнала активности: %w",
					methodCtx,
					context.DeadlineExceeded,
				)
			} else {
				disconnectErr = err
			}
			if !errors.Is(err, context.Canceled) && !errors.Is(disconnectErr, ErrAgentReplaced) {
				logger.Warn(
					"соединение с агентом закрыто",
					"идентификатор_агента", agent.id,
					"ошибка", disconnectErr,
				)
			}
			return
		}
		agent.markMessage()
		if err := s.handleEnvelope(agent, envelope); err != nil {
			disconnectErr = err
			logger.Warn(
				"агент нарушил протокол",
				"идентификатор_агента", agent.id,
				"ошибка", err,
			)
			_ = conn.Close(websocket.StatusUnsupportedData, methodCtx+": некорректное сообщение")
			return
		}
	}
}

func (s *Server) register(agent *agentConnection) {
	s.mu.Lock()
	previous := s.agents[agent.id]
	s.agents[agent.id] = agent
	s.mu.Unlock()

	// Закрываем старое подключение после atomic replacement. unregister старого
	// обработчика сравнит указатели и не сможет удалить новый connection.
	if previous != nil && previous != agent {
		s.pause()
		previous.shutdown(ErrAgentReplaced)
	}
}

func (s *Server) unregister(agent *agentConnection, reason error) {
	removed := false
	s.mu.Lock()
	if s.agents[agent.id] == agent {
		delete(s.agents, agent.id)
		removed = true
	}
	s.mu.Unlock()
	agent.shutdown(reason)
	if removed {
		s.pause()
	}
}

func (s *Server) isCurrent(agent *agentConnection) bool {
	s.mu.RLock()
	current := s.agents[agent.id] == agent
	s.mu.RUnlock()
	return current
}

func (s *Server) handleEnvelope(agent *agentConnection, envelope protocol.Envelope) error {
	const methodCtx = "controller.Server.handleEnvelope"

	if !s.isCurrent(agent) {
		return fmt.Errorf("%s: сообщение поступило из заменённого подключения: %w", methodCtx, ErrAgentReplaced)
	}

	switch envelope.Type {
	case protocol.MessageHeartbeat:
		var heartbeat protocol.Heartbeat
		if err := json.Unmarshal(envelope.Payload, &heartbeat); err != nil {
			return fmt.Errorf("%s: некорректный HEARTBEAT: %w", methodCtx, err)
		}
		if heartbeat.AgentID != agent.id {
			return fmt.Errorf("%s: HEARTBEAT содержит agent_id %q вместо %q", methodCtx, heartbeat.AgentID, agent.id)
		}
		agent.markHeartbeat()

	case protocol.MessageWindowStatus:
		correlationID := responseCorrelationID(envelope)
		if agent.rejectUnexpectedPendingType(correlationID, envelope.Type) {
			return nil
		}
		var status protocol.WindowStatus
		if err := json.Unmarshal(envelope.Payload, &status); err != nil {
			return fmt.Errorf("%s: некорректный WINDOW_STATUS: %w", methodCtx, err)
		}
		agent.storeWindow(status)
		agent.deliver(correlationID, envelope)

	case protocol.MessageFrame, protocol.MessageFrameRegion:
		correlationID := responseCorrelationID(envelope)
		if agent.rejectUnexpectedPendingType(correlationID, envelope.Type) {
			return nil
		}
		var frame protocol.Frame
		if err := json.Unmarshal(envelope.Payload, &frame); err != nil {
			return fmt.Errorf("%s: некорректный %s: %w", methodCtx, envelope.Type, err)
		}
		frame, err := normalizeInboundFrame(frame)
		if err != nil {
			return fmt.Errorf("%s: некорректный %s: %w", methodCtx, envelope.Type, err)
		}
		agent.storeFrame(frame)
		agent.deliver(correlationID, envelope)

	case protocol.MessageActionResult:
		var result protocol.ActionResult
		if err := json.Unmarshal(envelope.Payload, &result); err != nil {
			return fmt.Errorf("%s: некорректный ACTION_RESULT: %w", methodCtx, err)
		}
		correlationID := envelope.CorrelationID
		if correlationID == "" {
			correlationID = result.ID
		}
		if agent.rejectUnexpectedPendingType(correlationID, envelope.Type) {
			return nil
		}
		if result.Frame != nil {
			if result.ResultFrame != 0 && result.Frame.ID != result.ResultFrame {
				return fmt.Errorf(
					"%s: ACTION_RESULT содержит идентификатор кадра %d вместо result_frame=%d",
					methodCtx,
					result.Frame.ID,
					result.ResultFrame,
				)
			}
			normalizedFrame, err := normalizeInboundFrame(*result.Frame)
			if err != nil {
				return fmt.Errorf("%s: ACTION_RESULT содержит некорректный контрольный кадр: %w", methodCtx, err)
			}
			result.Frame = &normalizedFrame
			result.ResultFrame = result.Frame.ID
			agent.storeFrame(*result.Frame)
		}
		var outcomeErr error
		if err := s.persistActionResult(agent.id, correlationID, envelope, result); err != nil {
			outcomeErr = errors.Join(outcomeErr, err)
		}
		if result.Frame != nil && s.actionFrameSink != nil {
			frameCtx, cancel := context.WithTimeout(context.Background(), s.auditTimeout)
			err := s.actionFrameSink(frameCtx, agent.id, result.ID, *result.Frame)
			cancel()
			if err != nil {
				outcomeErr = errors.Join(
					outcomeErr,
					fmt.Errorf(
						"%s: %w для действия %q: %v",
						methodCtx,
						ErrEvidencePersistence,
						result.ID,
						err,
					),
				)
			}
		}
		if outcomeErr != nil {
			s.pause()
			agent.deliverError(correlationID, outcomeErr)
			s.logger.With("метод", methodCtx).Error(
				"результат действия не удалось сохранить полностью; автоматика остановлена",
				"идентификатор_агента", agent.id,
				"идентификатор_действия", result.ID,
				"ошибка", outcomeErr,
			)
			return nil
		}
		agent.deliver(correlationID, envelope)

	case protocol.MessageAgentEvent:
		correlationID := responseCorrelationID(envelope)
		var event protocol.AgentEvent
		if err := json.Unmarshal(envelope.Payload, &event); err != nil {
			return fmt.Errorf("%s: некорректный AGENT_EVENT: %w", methodCtx, err)
		}
		if event.At.IsZero() {
			event.At = time.Now().UTC()
		}
		agent.storeEvent(event)
		if err := s.persistAgentEvent(agent.id, envelope, event); err != nil {
			s.pause()
			s.logger.With("метод", methodCtx).Error(
				"событие агента не удалось сохранить; автоматика остановлена",
				"идентификатор_агента", agent.id,
				"идентификатор_события", envelope.MessageID,
				"ошибка", err,
			)
			agent.deliverError(correlationID, err)
			return nil
		}
		switch event.Kind {
		case "AUTOMATION_PAUSED":
			// Локальный supervisor имеет приоритет над режимом controller:
			// после физического ввода пользователя ни одна следующая команда
			// не должна пройти через input gate.
			mode := s.Mode()
			if mode != domain.ModeObserve {
				mode = domain.ModePaused
			}
			s.closeInputGate(mode)
		case "EMERGENCY_STOP_APPLIED":
			s.pause()
		}
		if agent.rejectUnexpectedPendingType(correlationID, envelope.Type) {
			return nil
		}
		agent.deliver(correlationID, envelope)

	case protocol.MessageEmergencyStop:
		var stop protocol.EmergencyStop
		if err := json.Unmarshal(envelope.Payload, &stop); err != nil {
			return fmt.Errorf("%s: некорректный EMERGENCY_STOP: %w", methodCtx, err)
		}
		if stop.At.IsZero() {
			stop.At = time.Now().UTC()
		}
		agent.markEmergency(stop)
		s.pause()
		agent.deliver(responseCorrelationID(envelope), envelope)
		if err := s.persistEmergencyStop(agent.id, envelope, stop); err != nil {
			s.logger.With("метод", methodCtx).Error(
				"аварийную остановку не удалось сохранить в журнале",
				"идентификатор_агента", agent.id,
				"идентификатор_сообщения", envelope.MessageID,
				"ошибка", err,
			)
		}

	case protocol.MessageHello:
		return fmt.Errorf("%s: повторный HELLO в одном подключении", methodCtx)

	default:
		return fmt.Errorf("%s: неподдерживаемый тип сообщения %q", methodCtx, envelope.Type)
	}
	return nil
}

// State возвращает согласованный снимок runtime.
func (s *Server) State() RuntimeState {
	s.mu.RLock()
	state := RuntimeState{
		Mode:          s.mode,
		ModeUpdatedAt: s.modeUpdatedAt,
		Agents:        make([]AgentState, 0, len(s.agents)),
	}
	for _, agent := range s.agents {
		state.Agents = append(state.Agents, agent.snapshot())
	}
	s.mu.RUnlock()
	sort.Slice(state.Agents, func(i, j int) bool {
		return state.Agents[i].AgentID < state.Agents[j].AgentID
	})
	return state
}

// AgentState возвращает снимок конкретного подключения.
func (s *Server) AgentState(agentID string) (AgentState, bool) {
	agent, ok := s.findAgent(agentID)
	if !ok {
		return AgentState{}, false
	}
	return agent.snapshot(), true
}

// LatestFrame возвращает защитную копию последнего входящего кадра.
func (s *Server) LatestFrame(agentID string) (protocol.Frame, bool) {
	agent, ok := s.findAgent(agentID)
	if !ok {
		return protocol.Frame{}, false
	}
	return agent.frame()
}

// Mode возвращает текущий режим controller.
func (s *Server) Mode() domain.AgentMode {
	s.mu.RLock()
	mode := s.mode
	s.mu.RUnlock()
	return mode
}

// SetModeAuthorizer installs the production preflight callback. It is safe to
// configure after dependent runtime components have been composed.
func (s *Server) SetModeAuthorizer(authorizer ModeAuthorizer) {
	s.modeAuthorizerMu.Lock()
	s.modeAuthorizer = authorizer
	s.modeAuthorizerMu.Unlock()
}

// SetMode валидирует явный переход режима. PAUSED остаётся default и также
// устанавливается автоматически при EMERGENCY_STOP.
func (s *Server) SetMode(mode domain.AgentMode) error {
	const methodCtx = "controller.Server.SetMode"

	if err := s.SetModeContext(context.Background(), mode); err != nil {
		return fmt.Errorf("%s: не удалось установить режим %s: %w", methodCtx, mode, err)
	}
	return nil
}

// SetModeContext authorizes SCAN/TRADE against a fresh preflight. During that
// potentially slow check the runtime remains PAUSED. Any safety transition
// invalidates the result before the action gate can reopen.
func (s *Server) SetModeContext(ctx context.Context, mode domain.AgentMode) error {
	const methodCtx = "controller.Server.SetModeContext"

	if !validMode(mode) {
		return fmt.Errorf("%s: неподдерживаемый режим %q", methodCtx, mode)
	}
	if mode != domain.ModeScan && mode != domain.ModeTrade {
		s.closeInputGate(mode)
		return nil
	}

	s.modeTransitionMu.Lock()
	defer s.modeTransitionMu.Unlock()

	authorizationEpoch := s.closeInputGate(domain.ModePaused)

	s.modeAuthorizerMu.RLock()
	authorizer := s.modeAuthorizer
	s.modeAuthorizerMu.RUnlock()
	if authorizer != nil {
		if ctx == nil {
			ctx = context.Background()
		}
		if err := authorizer(ctx, mode); err != nil {
			return fmt.Errorf("%s: %w: проверка готовности: %w", methodCtx, ErrModeAuthorization, err)
		}
	}

	s.actionGate.Lock()
	defer s.actionGate.Unlock()
	if s.safetyEpoch.Load() != authorizationEpoch {
		return fmt.Errorf("%s: %w: предварительная проверка устарела из-за события безопасности", methodCtx, ErrModeAuthorization)
	}
	s.setMode(mode)
	return nil
}

func (s *Server) setMode(mode domain.AgentMode) {
	s.mu.Lock()
	if s.mode != mode {
		s.mode = mode
		s.modeUpdatedAt = time.Now().UTC()
	}
	s.mu.Unlock()
}

func (s *Server) pause() {
	s.closeInputGate(domain.ModePaused)
}

// closeInputGate публикует безопасный режим до ожидания зависшего transport
// write и отменяет все защищённые записи. Поэтому локальная/удалённая пауза
// не зависит от сетевого таймаута текущего ACTION_REQUEST.
func (s *Server) closeInputGate(mode domain.AgentMode) uint64 {
	epoch := s.safetyEpoch.Add(1)
	s.setMode(mode)
	s.cancelGuardedSends()
	s.actionGate.Lock()
	s.actionGate.Unlock()
	return epoch
}

func (s *Server) registerGuardedSend(
	ctx context.Context,
) (context.Context, func()) {
	sendCtx, cancel := context.WithCancel(ctx)
	s.guardedSendMu.Lock()
	s.guardedSendSeq++
	id := s.guardedSendSeq
	s.guardedSendStops[id] = cancel
	s.guardedSendMu.Unlock()
	return sendCtx, func() {
		s.guardedSendMu.Lock()
		delete(s.guardedSendStops, id)
		s.guardedSendMu.Unlock()
		cancel()
	}
}

func (s *Server) cancelGuardedSends() {
	s.guardedSendMu.Lock()
	stops := make([]context.CancelFunc, 0, len(s.guardedSendStops))
	for _, cancel := range s.guardedSendStops {
		stops = append(stops, cancel)
	}
	s.guardedSendMu.Unlock()
	for _, cancel := range stops {
		cancel()
	}
}

func validMode(mode domain.AgentMode) bool {
	switch mode {
	case domain.ModeObserve, domain.ModeScan, domain.ModeSimulate, domain.ModeTrade, domain.ModePaused:
		return true
	default:
		return false
	}
}

func (s *Server) findAgent(agentID string) (*agentConnection, bool) {
	s.mu.RLock()
	agent, ok := s.agents[agentID]
	s.mu.RUnlock()
	return agent, ok
}

func (s *Server) nextID(prefix string) string {
	return fmt.Sprintf("%s-%d-%d", prefix, time.Now().UnixNano(), s.sequence.Add(1))
}

func (a *agentConnection) markMessage() {
	a.stateMu.Lock()
	a.lastMessageAt = time.Now().UTC()
	a.stateMu.Unlock()
}

func (a *agentConnection) markHeartbeat() {
	now := time.Now().UTC()
	a.stateMu.Lock()
	a.lastHeartbeatAt = now
	a.lastMessageAt = now
	a.stateMu.Unlock()
}

func (a *agentConnection) heartbeatDeadline(timeout time.Duration) time.Time {
	a.stateMu.RLock()
	deadline := a.lastHeartbeatAt.Add(timeout)
	a.stateMu.RUnlock()
	return deadline
}

func (a *agentConnection) storeWindow(status protocol.WindowStatus) {
	a.stateMu.Lock()
	a.window = &status
	a.stateMu.Unlock()
}

func (a *agentConnection) storeFrame(frame protocol.Frame) {
	a.stateMu.Lock()
	if a.latestFrame == nil ||
		(frame.ID > a.latestFrame.ID &&
			(a.latestFrame.CapturedAt.IsZero() ||
				frame.CapturedAt.IsZero() ||
				!frame.CapturedAt.Before(a.latestFrame.CapturedAt))) {
		// Копируем тяжёлый payload только после монотонной проверки: старый
		// кадр не должен ни заменить latest, ни кратковременно удвоить память.
		frame.Data = append([]byte(nil), frame.Data...)
		a.latestFrame = &frame
	}
	a.stateMu.Unlock()
}

func (a *agentConnection) frameBasis(frameID uint64) (protocol.Frame, uint64, bool) {
	a.stateMu.RLock()
	if a.latestFrame == nil {
		a.stateMu.RUnlock()
		return protocol.Frame{}, 0, false
	}
	latestID := a.latestFrame.ID
	if latestID != frameID ||
		a.latestFrame.CapturedAt.IsZero() ||
		!protocol.ValidFrameDigest(a.latestFrame.ContentDigest) {
		a.stateMu.RUnlock()
		return protocol.Frame{}, latestID, false
	}
	frame := *a.latestFrame
	a.stateMu.RUnlock()
	return frame, latestID, true
}

func (a *agentConnection) storeEvent(event protocol.AgentEvent) {
	event.Details = cloneStringMap(event.Details)
	a.stateMu.Lock()
	switch event.Kind {
	case "AUTOMATION_PAUSED":
		a.automationPaused = true
		a.safetyReason = event.Message
	case "AUTOMATION_RESUMED":
		a.automationPaused = false
		a.safetyReason = ""
	case "EMERGENCY_STOP_APPLIED":
		a.automationPaused = true
		a.emergencyStopped = true
		a.safetyReason = event.Message
	}
	a.eventCount++
	a.events = append(a.events, event)
	if len(a.events) > maxRememberedEvents {
		copy(a.events, a.events[len(a.events)-maxRememberedEvents:])
		a.events = a.events[:maxRememberedEvents]
	}
	a.stateMu.Unlock()
}

func (a *agentConnection) markEmergency(stop protocol.EmergencyStop) {
	a.stateMu.Lock()
	a.automationPaused = true
	a.emergencyStopped = true
	a.safetyReason = stop.Reason
	a.eventCount++
	a.events = append(a.events, protocol.AgentEvent{
		Kind:     "EMERGENCY_STOP",
		Severity: "critical",
		Message:  stop.Reason,
		At:       stop.At,
	})
	if len(a.events) > maxRememberedEvents {
		a.events = append([]protocol.AgentEvent(nil), a.events[len(a.events)-maxRememberedEvents:]...)
	}
	a.stateMu.Unlock()
}

func (a *agentConnection) snapshot() AgentState {
	a.stateMu.RLock()
	snapshot := AgentState{
		AgentID:          a.id,
		Version:          a.version,
		Features:         append([]string(nil), a.features...),
		ConnectedAt:      a.connectedAt,
		LastHeartbeatAt:  a.lastHeartbeatAt,
		LastMessageAt:    a.lastMessageAt,
		EventCount:       a.eventCount,
		AutomationPaused: a.automationPaused,
		EmergencyStopped: a.emergencyStopped,
		SafetyReason:     a.safetyReason,
	}
	if a.window != nil {
		value := *a.window
		snapshot.Window = &value
	}
	if a.latestFrame != nil {
		snapshot.LatestFrame = &FrameSummary{
			ID:            a.latestFrame.ID,
			CapturedAt:    a.latestFrame.CapturedAt,
			ContentDigest: a.latestFrame.ContentDigest,
			Region:        a.latestFrame.Region,
			Encoding:      a.latestFrame.Encoding,
			Size:          len(a.latestFrame.Data),
		}
	}
	if len(a.events) > 0 {
		value := a.events[len(a.events)-1]
		value.Details = cloneStringMap(value.Details)
		snapshot.LastEvent = &value
	}
	a.stateMu.RUnlock()
	return snapshot
}

func (a *agentConnection) frame() (protocol.Frame, bool) {
	a.stateMu.RLock()
	if a.latestFrame == nil {
		a.stateMu.RUnlock()
		return protocol.Frame{}, false
	}
	frame := *a.latestFrame
	frame.Data = append([]byte(nil), a.latestFrame.Data...)
	a.stateMu.RUnlock()
	return frame, true
}

func (a *agentConnection) send(ctx context.Context, envelope protocol.Envelope) error {
	const methodCtx = "controller.agentConnection.send"

	select {
	case <-ctx.Done():
		return fmt.Errorf(
			"%s: контекст завершён до захвата очереди записи: %w",
			methodCtx,
			ctx.Err(),
		)
	case <-a.closed:
		return fmt.Errorf("%s: соединение закрыто: %w", methodCtx, ErrAgentNotConnected)
	case a.writeGate <- struct{}{}:
	}
	defer func() { <-a.writeGate }()
	select {
	case <-ctx.Done():
		return fmt.Errorf(
			"%s: контекст завершён перед записью: %w",
			methodCtx,
			ctx.Err(),
		)
	case <-a.closed:
		return fmt.Errorf("%s: соединение закрыто перед записью: %w", methodCtx, ErrAgentNotConnected)
	default:
	}
	if err := wsjson.Write(ctx, a.conn, envelope); err != nil {
		return fmt.Errorf("%s: не удалось отправить %s агенту %s: %w", methodCtx, envelope.Type, a.id, err)
	}
	return nil
}

func (a *agentConnection) addPending(
	requestID string,
	expectedType protocol.MessageType,
) (<-chan pendingResponse, error) {
	const methodCtx = "controller.agentConnection.addPending"

	a.pendingMu.Lock()
	defer a.pendingMu.Unlock()
	select {
	case <-a.closed:
		return nil, fmt.Errorf("%s: соединение закрыто: %w", methodCtx, ErrAgentNotConnected)
	default:
	}
	if _, exists := a.pending[requestID]; exists {
		return nil, fmt.Errorf("%s: запрос %q не зарегистрирован: %w", methodCtx, requestID, ErrRequestIDInUse)
	}
	result := make(chan pendingResponse, 1)
	a.pending[requestID] = result
	a.pendingTypes[requestID] = expectedType
	return result, nil
}

func (a *agentConnection) removePending(requestID string) {
	a.pendingMu.Lock()
	delete(a.pending, requestID)
	delete(a.pendingTypes, requestID)
	a.pendingMu.Unlock()
}

func (a *agentConnection) rejectUnexpectedPendingType(
	requestID string,
	actualType protocol.MessageType,
) bool {
	const methodCtx = "controller.agentConnection.rejectUnexpectedPendingType"

	if requestID == "" {
		return false
	}
	a.pendingMu.Lock()
	result, ok := a.pending[requestID]
	expectedType := a.pendingTypes[requestID]
	if ok && expectedType != "" && expectedType != actualType {
		delete(a.pending, requestID)
		delete(a.pendingTypes, requestID)
	}
	a.pendingMu.Unlock()
	if !ok || expectedType == "" || expectedType == actualType {
		return false
	}
	result <- pendingResponse{err: fmt.Errorf(
		"%s: %w: для запроса %q ожидался %s, получен %s",
		methodCtx,
		ErrUnexpectedResponse,
		requestID,
		expectedType,
		actualType,
	)}
	return true
}

func (a *agentConnection) deliver(requestID string, envelope protocol.Envelope) bool {
	if requestID == "" {
		return false
	}
	a.pendingMu.Lock()
	result, ok := a.pending[requestID]
	if ok {
		delete(a.pending, requestID)
		delete(a.pendingTypes, requestID)
	}
	a.pendingMu.Unlock()
	if ok {
		result <- pendingResponse{envelope: envelope}
	}
	return ok
}

func (a *agentConnection) deliverError(requestID string, err error) bool {
	if requestID == "" {
		return false
	}
	a.pendingMu.Lock()
	result, ok := a.pending[requestID]
	if ok {
		delete(a.pending, requestID)
		delete(a.pendingTypes, requestID)
	}
	a.pendingMu.Unlock()
	if ok {
		result <- pendingResponse{err: err}
	}
	return ok
}

func (a *agentConnection) shutdown(reason error) {
	a.closeOnce.Do(func() {
		close(a.closed)
		a.conn.CloseNow()

		a.pendingMu.Lock()
		pending := a.pending
		a.pending = make(map[string]chan pendingResponse)
		a.pendingTypes = make(map[string]protocol.MessageType)
		a.pendingMu.Unlock()
		for _, result := range pending {
			result <- pendingResponse{err: reason}
		}
	})
}

func cloneStringMap(source map[string]string) map[string]string {
	if source == nil {
		return nil
	}
	clone := make(map[string]string, len(source))
	for key, value := range source {
		clone[key] = value
	}
	return clone
}

func responseCorrelationID(envelope protocol.Envelope) string {
	if envelope.CorrelationID != "" {
		return envelope.CorrelationID
	}
	return envelope.MessageID
}

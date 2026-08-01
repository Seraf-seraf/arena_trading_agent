// Package session coordinates observation and preflight around the duplex
// controller without allowing vision services to send input themselves.
package session

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/arena-trading-agent/arena-trading-agent/internal/controller"
	"github.com/arena-trading-agent/arena-trading-agent/internal/domain"
	"github.com/arena-trading-agent/arena-trading-agent/internal/observation"
	"github.com/arena-trading-agent/arena-trading-agent/internal/protocol"
	"github.com/arena-trading-agent/arena-trading-agent/internal/recording"
	"github.com/arena-trading-agent/arena-trading-agent/internal/repository"
)

// HealthChecker is implemented by LM Studio and OCR adapters.
type HealthChecker interface {
	Health(context.Context) error
}

// ModeReadiness adds domain/configuration checks after the hardware and vision
// preflight succeeds.
type ModeReadiness func(context.Context, domain.AgentMode) error

// Config contains deterministic preflight requirements.
type Config struct {
	ObserveInterval    time.Duration
	ExpectedWidth      int
	ExpectedHeight     int
	ExpectedDPIPercent int
	MinConfidence      float64
}

// Coordinator owns observation state and the periodic OBSERVE loop.
type Coordinator struct {
	controller *controller.Server
	observer   *observation.Observer
	store      repository.Store
	recording  *recording.Store
	vlm        HealthChecker
	ocr        HealthChecker
	logger     *slog.Logger
	config     Config

	mu            sync.RWMutex
	latest        domain.Observation
	latestRecord  recording.FrameRecord
	lastError     string
	observedCount uint64
	observing     bool
	readiness     ModeReadiness
}

// Snapshot is returned to the dashboard API.
type Snapshot struct {
	Latest        *domain.Observation    `json:"latest_observation,omitempty"`
	LatestFrame   *recording.FrameRecord `json:"latest_frame,omitempty"`
	LastError     string                 `json:"last_error,omitempty"`
	ObservedCount uint64                 `json:"observed_count"`
	Observing     bool                   `json:"observing"`
}

// Check is one independently auditable preflight result.
type Check struct {
	Name     string `json:"name"`
	OK       bool   `json:"ok"`
	Critical bool   `json:"critical"`
	Message  string `json:"message,omitempty"`
}

// PreflightResult describes whether a mode may start.
type PreflightResult struct {
	AgentID     string              `json:"agent_id"`
	TradeReady  bool                `json:"trade_ready"`
	Checks      []Check             `json:"checks"`
	Window      *structWindow       `json:"window,omitempty"`
	Observation *domain.Observation `json:"observation,omitempty"`
	CheckedAt   time.Time           `json:"checked_at"`
}

// structWindow prevents session callers from depending on transport internals
// in JSON while retaining all safety-relevant values.
type structWindow struct {
	Active     bool `json:"active"`
	Minimized  bool `json:"minimized"`
	Width      int  `json:"width"`
	Height     int  `json:"height"`
	DPIPercent int  `json:"dpi_percent"`
}

// New creates a coordinator.
func New(
	server *controller.Server,
	observer *observation.Observer,
	store repository.Store,
	recorder *recording.Store,
	vlm HealthChecker,
	ocr HealthChecker,
	logger *slog.Logger,
	config Config,
) *Coordinator {
	if config.ObserveInterval <= 0 {
		config.ObserveInterval = 5 * time.Second
	}
	if config.MinConfidence <= 0 {
		config.MinConfidence = .8
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Coordinator{
		controller: server, observer: observer, store: store, recording: recorder,
		vlm: vlm, ocr: ocr, logger: logger, config: config,
	}
}

// Run observes periodically only after an explicit OBSERVE mode transition.
func (c *Coordinator) Run(ctx context.Context) {
	const methodCtx = "session.Coordinator.Run"
	logger := c.logger.With("метод", methodCtx)

	ticker := time.NewTicker(c.config.ObserveInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if c.controller.Mode() != domain.ModeObserve {
				continue
			}
			if _, err := c.Observe(ctx, ""); err != nil {
				c.setError(err)
				logger.Warn("цикл наблюдения завершился ошибкой", "ошибка", err)
			}
		}
	}
}

// Observe captures, records, recognizes and persists a fresh frame.
func (c *Coordinator) Observe(ctx context.Context, agentID string) (domain.Observation, error) {
	const methodCtx = "session.Coordinator.Observe"

	latestFrame := uint64(0)
	resolvedAgent, err := c.resolveAgent(agentID)
	if err != nil {
		return domain.Observation{}, fmt.Errorf("%s: не удалось определить агента: %w", methodCtx, err)
	}
	if state, ok := c.controller.AgentState(resolvedAgent); ok && state.LatestFrame != nil {
		latestFrame = state.LatestFrame.ID
	}
	_, value, err := c.ObserveAfter(ctx, resolvedAgent, latestFrame)
	if err != nil {
		return domain.Observation{}, fmt.Errorf("%s: наблюдение завершилось ошибкой: %w", methodCtx, err)
	}
	return value, nil
}

// ObserveAfter implements navigation.ObservationSource. It persists and
// records the same frame it returns and rejects a replayed/non-monotonic ID.
func (c *Coordinator) ObserveAfter(
	ctx context.Context,
	agentID string,
	afterFrame uint64,
) (protocol.Frame, domain.Observation, error) {
	const methodCtx = "session.Coordinator.ObserveAfter"

	if c.observer == nil || c.store == nil || c.recording == nil {
		return protocol.Frame{}, domain.Observation{}, fmt.Errorf("%s: runtime наблюдений не настроен", methodCtx)
	}
	agentID, err := c.resolveAgent(agentID)
	if err != nil {
		return protocol.Frame{}, domain.Observation{}, fmt.Errorf("%s: не удалось определить агента: %w", methodCtx, err)
	}
	c.mu.Lock()
	if c.observing {
		c.mu.Unlock()
		return protocol.Frame{}, domain.Observation{}, fmt.Errorf("%s: наблюдение уже выполняется", methodCtx)
	}
	c.observing = true
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.observing = false
		c.mu.Unlock()
	}()

	frame, err := c.controller.RequestFrame(ctx, agentID, afterFrame)
	if err != nil {
		return protocol.Frame{}, domain.Observation{}, fmt.Errorf("%s: не удалось захватить кадр: %w", methodCtx, err)
	}
	if frame.ID <= afterFrame {
		return protocol.Frame{}, domain.Observation{}, fmt.Errorf(
			"%s: агент вернул кадр %d не новее %d",
			methodCtx,
			frame.ID,
			afterFrame,
		)
	}
	record, err := c.recording.SaveFrame(frame)
	if err != nil {
		return protocol.Frame{}, domain.Observation{}, fmt.Errorf("%s: не удалось записать кадр: %w", methodCtx, err)
	}
	value, err := c.observer.Observe(ctx, frame)
	if err != nil {
		return protocol.Frame{}, domain.Observation{}, fmt.Errorf("%s: не удалось распознать кадр %d: %w", methodCtx, frame.ID, err)
	}
	if err := c.store.SaveObservation(ctx, value); err != nil {
		return protocol.Frame{}, domain.Observation{}, fmt.Errorf("%s: не удалось сохранить наблюдение: %w", methodCtx, err)
	}
	if err := c.recording.SaveObservation(record, value); err != nil {
		return protocol.Frame{}, domain.Observation{}, fmt.Errorf("%s: не удалось записать файл наблюдения рядом с кадром: %w", methodCtx, err)
	}
	c.mu.Lock()
	c.latest = value
	c.latestRecord = record
	c.lastError = ""
	c.observedCount++
	c.mu.Unlock()
	return frame, value, nil
}

// Preflight verifies services, connection, window, capture and recognition.
// trade=true additionally requires an input-capable agent and configured
// resolution/DPI; it never changes controller mode by itself.
func (c *Coordinator) Preflight(ctx context.Context, agentID string, trade bool) PreflightResult {
	const methodCtx = "session.Coordinator.Preflight"

	result := PreflightResult{CheckedAt: time.Now().UTC()}
	agentID, err := c.resolveAgent(agentID)
	if err != nil {
		result.Checks = append(result.Checks, failedCheck("agent", true, err))
		return result
	}
	result.AgentID = agentID
	agentState, _ := c.controller.AgentState(agentID)
	result.Checks = append(result.Checks, Check{Name: "agent", OK: true, Critical: true})
	emergencyMessage := ""
	if agentState.EmergencyStopped {
		emergencyMessage = methodCtx + ": Windows-агент требует перезапуска после аварийной остановки"
	}
	result.Checks = append(result.Checks, Check{
		Name: "emergency_stop", OK: !agentState.EmergencyStopped, Critical: true,
		Message: emergencyMessage,
	})
	if trade {
		inputCapable := slices.Contains(agentState.Features, "sequential_actions") &&
			slices.Contains(agentState.Features, "send_input")
		message := ""
		if !inputCapable {
			message = methodCtx + ": Windows-агент запущен без флага -allow-input"
		}
		result.Checks = append(result.Checks, Check{
			Name: "input", OK: inputCapable, Critical: true, Message: message,
		})
	}
	// A calibrated known-screen route remains operational without VLM. OCR is
	// still mandatory for any input-capable mode.
	c.checkService(ctx, &result, "lm_studio", c.vlm, false)
	c.checkService(ctx, &result, "ocr", c.ocr, trade)

	status, err := c.controller.RequestWindowStatus(ctx, agentID)
	if err != nil {
		result.Checks = append(result.Checks, failedCheck("window", true, err))
		return result
	}
	result.Window = &structWindow{
		Active: status.Active, Minimized: status.Minimized, Width: status.Width,
		Height: status.Height, DPIPercent: status.DPIPercent,
	}
	windowOK := status.Active && !status.Minimized && status.Width > 0 && status.Height > 0
	message := ""
	if !windowOK {
		message = methodCtx + ": окно игры должно быть активным и не свёрнутым"
	}
	result.Checks = append(result.Checks, Check{Name: "window", OK: windowOK, Critical: true, Message: message})
	geometryConfigured := c.config.ExpectedWidth > 0 &&
		c.config.ExpectedHeight > 0 &&
		c.config.ExpectedDPIPercent > 0
	if geometryConfigured {
		geometryOK := status.Width == c.config.ExpectedWidth &&
			status.Height == c.config.ExpectedHeight &&
			status.DPIPercent == c.config.ExpectedDPIPercent
		result.Checks = append(result.Checks, Check{
			Name: "geometry", OK: geometryOK, Critical: trade,
			Message: fmt.Sprintf("%s: получено %dx%d @ %d%%", methodCtx, status.Width, status.Height, status.DPIPercent),
		})
	} else if trade {
		result.Checks = append(result.Checks, Check{
			Name: "geometry", OK: false, Critical: true,
			Message: methodCtx + ": для режима с вводом задайте ожидаемые ширину, высоту и DPI",
		})
	}
	if hasCriticalFailure(result.Checks) {
		return result
	}

	value, err := c.Observe(ctx, agentID)
	if err != nil {
		result.Checks = append(result.Checks, failedCheck("observation", true, err))
	} else {
		result.Observation = &value
		ok := value.Confidence >= c.config.MinConfidence && value.State != domain.StateUnknown
		result.Checks = append(result.Checks, Check{
			Name: "observation", OK: ok, Critical: true,
			Message: fmt.Sprintf("%s: состояние=%s, уверенность=%.3f", methodCtx, value.State, value.Confidence),
		})
	}
	result.TradeReady = !hasCriticalFailure(result.Checks)
	return result
}

// AuthorizeMode is installed into controller's input gate. It performs a
// fresh, full preflight for both SCAN and TRADE; it never changes mode itself.
func (c *Coordinator) AuthorizeMode(ctx context.Context, mode domain.AgentMode) error {
	const methodCtx = "session.Coordinator.AuthorizeMode"

	if mode != domain.ModeScan && mode != domain.ModeTrade {
		return nil
	}
	result := c.Preflight(ctx, "", true)
	if !result.TradeReady {
		failures := make([]string, 0, len(result.Checks))
		for _, check := range result.Checks {
			if check.Critical && !check.OK {
				if check.Message == "" {
					failures = append(failures, check.Name)
				} else {
					failures = append(failures, check.Name+": "+check.Message)
				}
			}
		}
		if len(failures) == 0 {
			failures = append(failures, "неизвестная ошибка предварительной проверки")
		}
		return fmt.Errorf("%s: режим %s не прошёл предварительную проверку: %s", methodCtx, mode, strings.Join(failures, "; "))
	}
	c.mu.RLock()
	readiness := c.readiness
	c.mu.RUnlock()
	if readiness != nil {
		if err := readiness(ctx, mode); err != nil {
			return fmt.Errorf("%s: доменная готовность режима %s не подтверждена: %w", methodCtx, mode, err)
		}
	}
	return nil
}

// SetModeReadiness installs the domain preflight after all runtime modules
// have been composed.
func (c *Coordinator) SetModeReadiness(readiness ModeReadiness) {
	c.mu.Lock()
	c.readiness = readiness
	c.mu.Unlock()
}

// Snapshot returns a defensive copy of current observation state.
func (c *Coordinator) Snapshot() Snapshot {
	c.mu.RLock()
	defer c.mu.RUnlock()
	result := Snapshot{
		LastError: c.lastError, ObservedCount: c.observedCount, Observing: c.observing,
	}
	if !c.latest.CreatedAt.IsZero() {
		value := c.latest
		result.Latest = &value
	}
	if c.latestRecord.Path != "" {
		value := c.latestRecord
		result.LatestFrame = &value
	}
	return result
}

func (c *Coordinator) resolveAgent(agentID string) (string, error) {
	const methodCtx = "session.Coordinator.resolveAgent"

	if agentID != "" {
		if _, ok := c.controller.AgentState(agentID); !ok {
			return "", fmt.Errorf("%s: агент %q не найден: %w", methodCtx, agentID, controller.ErrAgentNotConnected)
		}
		return agentID, nil
	}
	state := c.controller.State()
	if len(state.Agents) == 0 {
		return "", fmt.Errorf("%s: нет подключённых агентов: %w", methodCtx, controller.ErrAgentNotConnected)
	}
	if len(state.Agents) != 1 {
		return "", fmt.Errorf("%s: поле agent_id обязательно при нескольких агентах", methodCtx)
	}
	return state.Agents[0].AgentID, nil
}

func (c *Coordinator) checkService(ctx context.Context, result *PreflightResult, name string, service HealthChecker, critical bool) {
	const methodCtx = "session.Coordinator.checkService"

	if service == nil {
		result.Checks = append(result.Checks, Check{
			Name: name, OK: false, Critical: critical, Message: methodCtx + ": сервис не настроен",
		})
		return
	}
	if err := service.Health(ctx); err != nil {
		result.Checks = append(result.Checks, failedCheck(name, critical, err))
		return
	}
	result.Checks = append(result.Checks, Check{Name: name, OK: true, Critical: critical})
}

func (c *Coordinator) setError(err error) {
	c.mu.Lock()
	c.lastError = err.Error()
	c.mu.Unlock()
}

func failedCheck(name string, critical bool, err error) Check {
	return Check{Name: name, OK: false, Critical: critical, Message: err.Error()}
}

func hasCriticalFailure(checks []Check) bool {
	for _, check := range checks {
		if check.Critical && !check.OK {
			return true
		}
	}
	return false
}

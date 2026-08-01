// Package agent implements the local Windows Agent runtime.
package agent

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/arena-trading-agent/arena-trading-agent/internal/protocol"
)

const (
	defaultHeartbeatInterval      = 5 * time.Second
	defaultReconnectMinimum       = 250 * time.Millisecond
	defaultReconnectMaximum       = 10 * time.Second
	maxTransportMessage           = protocol.MaxTransportMessageBytes
	maxActionRequestBytes         = 64 << 10
	maxActionIdentifierBytes      = 256
	maxSessionIdentifierBytes     = 256
	maxRememberedActions          = 64
	maxRememberedActionBytes      = 32 << 20
	maxRememberedActionTombstones = 4096
	seenActionFilterWords         = 1 << 17
	seenActionFilterBits          = seenActionFilterWords * 64
	seenActionFilterHashCount     = 4
)

// ClientOptions wires the transport to the eyes, safety state and the sole
// action executor. Nil platform dependencies leave heartbeat-only mode, which
// is useful for lifecycle diagnostics.
type ClientOptions struct {
	ControllerURL string
	AgentID       string
	Version       string
	Logger        *slog.Logger

	Capture  CaptureDriver
	Window   WindowManager
	Executor *ActionExecutor

	IsStopped          func() bool
	IsEmergencyStopped func() bool
	SafetyReason       func() string
	EmergencyStop      func(reason string, userInitiated bool)
	OnActionResult     func(protocol.ActionResult)

	HeartbeatInterval time.Duration
	ReconnectMinimum  time.Duration
	ReconnectMaximum  time.Duration
}

// Client maintains an outgoing, reconnecting duplex connection to controller.
type Client struct {
	controllerURL      string
	agentID            string
	version            string
	logger             *slog.Logger
	capture            CaptureDriver
	window             WindowManager
	executor           *ActionExecutor
	isStopped          func() bool
	isEmergencyStopped func() bool
	safetyReason       func() string
	emergencyStop      func(string, bool)
	onActionResult     func(protocol.ActionResult)

	heartbeatInterval time.Duration
	reconnectMinimum  time.Duration
	reconnectMaximum  time.Duration
	sequence          atomic.Uint64
	captureMu         sync.Mutex
	actionMu          sync.Mutex
	actions           map[string]actionTombstone
	actionOrder       []string
	actionResults     map[string]cachedActionResult
	actionResultOrder []string
	actionResultBytes int
	seenActions       [seenActionFilterWords]uint64
	events            chan protocol.AgentEvent
	emergencyMu       sync.Mutex
	emergencies       chan protocol.EmergencyStop
}

type actionTombstone struct {
	fingerprint [sha256.Size]byte
	completed   bool
}

type cachedActionResult struct {
	result     protocol.ActionResult
	frameBytes int
}

// NewClient preserves the original heartbeat-only constructor.
func NewClient(controllerURL, agentID, version string, logger *slog.Logger) *Client {
	return NewRuntimeClient(ClientOptions{
		ControllerURL: controllerURL,
		AgentID:       agentID,
		Version:       version,
		Logger:        logger,
	})
}

// NewRuntimeClient creates a production duplex transport client.
func NewRuntimeClient(options ClientOptions) *Client {
	logger := options.Logger
	if logger == nil {
		logger = slog.Default()
	}
	if options.HeartbeatInterval <= 0 {
		options.HeartbeatInterval = defaultHeartbeatInterval
	}
	if options.ReconnectMinimum <= 0 {
		options.ReconnectMinimum = defaultReconnectMinimum
	}
	if options.ReconnectMaximum < options.ReconnectMinimum {
		options.ReconnectMaximum = defaultReconnectMaximum
	}
	if options.IsStopped == nil {
		options.IsStopped = func() bool { return false }
	}
	if options.IsEmergencyStopped == nil {
		options.IsEmergencyStopped = func() bool { return false }
	}
	if options.SafetyReason == nil {
		options.SafetyReason = func() string { return "" }
	}
	if options.EmergencyStop == nil {
		options.EmergencyStop = func(string, bool) {}
	}
	if options.OnActionResult == nil {
		options.OnActionResult = func(protocol.ActionResult) {}
	}
	return &Client{
		controllerURL:      options.ControllerURL,
		agentID:            options.AgentID,
		version:            options.Version,
		logger:             logger,
		capture:            options.Capture,
		window:             options.Window,
		executor:           options.Executor,
		isStopped:          options.IsStopped,
		isEmergencyStopped: options.IsEmergencyStopped,
		safetyReason:       options.SafetyReason,
		emergencyStop:      options.EmergencyStop,
		onActionResult:     options.OnActionResult,
		heartbeatInterval:  options.HeartbeatInterval,
		reconnectMinimum:   options.ReconnectMinimum,
		reconnectMaximum:   options.ReconnectMaximum,
		actions:            make(map[string]actionTombstone),
		actionResults:      make(map[string]cachedActionResult),
		events:             make(chan protocol.AgentEvent, 64),
		emergencies:        make(chan protocol.EmergencyStop, 1),
	}
}

// PublishEvent queues a local runtime event for the current or next
// controller connection. Telemetry is bounded so a broken controller cannot
// exhaust Windows memory.
func (c *Client) PublishEvent(event protocol.AgentEvent) error {
	const methodCtx = "agent.Client.PublishEvent"

	if event.At.IsZero() {
		event.At = time.Now().UTC()
	}
	select {
	case c.events <- event:
		return nil
	default:
		return fmt.Errorf("%s: очередь событий агента заполнена", methodCtx)
	}
}

// PublishEmergencyStop keeps the latest emergency signal and sends it with
// priority, including after reconnect.
func (c *Client) PublishEmergencyStop(reason string, userInitiated bool) {
	const methodCtx = "agent.Client.PublishEmergencyStop"

	if reason == "" {
		reason = "причина аварийной остановки не указана"
	}
	stop := protocol.EmergencyStop{
		Reason: methodCtx + ": " + reason,
		At:     time.Now().UTC(), UserInitiated: userInitiated,
	}
	c.emergencyMu.Lock()
	defer c.emergencyMu.Unlock()
	select {
	case c.emergencies <- stop:
		return
	default:
	}
	select {
	case <-c.emergencies:
	default:
	}
	select {
	case c.emergencies <- stop:
	default:
		// Получатель мог освободить слот, а конкурентный publisher — снова
		// занять его до захвата mutex в старой версии. Mutex делает ветку
		// практически недостижимой, но неблокирующий select сохраняет
		// аварийный путь гарантированно без ожидания.
	}
}

// Run reconnects until its context is cancelled. A transport loss never
// authorizes retrying an input command; only new requests are executed.
func (c *Client) Run(ctx context.Context) error {
	const methodCtx = "agent.Client.Run"
	logger := c.logger.With("метод", methodCtx)

	if c.controllerURL == "" || c.agentID == "" {
		return fmt.Errorf("%s: требуются URL контроллера и идентификатор агента", methodCtx)
	}
	backoff := c.reconnectMinimum
	for {
		if err := ctx.Err(); err != nil {
			return nil
		}
		err := c.runConnection(ctx)
		if ctx.Err() != nil {
			return nil
		}
		logger.Warn("соединение с контроллером потеряно", "ошибка", err, "повтор_через", backoff)
		timer := time.NewTimer(jitter(backoff))
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
		backoff *= 2
		if backoff > c.reconnectMaximum {
			backoff = c.reconnectMaximum
		}
	}
}

func (c *Client) runConnection(ctx context.Context) error {
	const methodCtx = "agent.Client.runConnection"
	logger := c.logger.With("метод", methodCtx)

	conn, _, err := websocket.Dial(ctx, c.controllerURL, nil)
	if err != nil {
		return fmt.Errorf("%s: не удалось подключиться к контроллеру: %w", methodCtx, err)
	}
	conn.SetReadLimit(maxTransportMessage)
	defer conn.Close(websocket.StatusNormalClosure, "агент завершает соединение")

	connectionCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	// FRAME and ACTION_RESULT envelopes may contain encoded screenshots, so the
	// socket backlog is deliberately small and provides memory backpressure.
	outbound := make(chan protocol.Envelope, 2)
	writerError := make(chan error, 1)
	go func() {
		err := c.writeLoop(connectionCtx, conn, outbound)
		if err != nil {
			cancel()
		}
		writerError <- err
	}()

	features := []string{"heartbeat"}
	if c.capture != nil {
		features = append(features, "gdi_capture", "frame_region")
	}
	if c.window != nil {
		features = append(features, "window_status")
	}
	if c.executor != nil {
		features = append(features, "sequential_actions", "send_input", "emergency_stop")
	}
	hello, err := c.envelope(protocol.MessageHello, "", protocol.Hello{
		AgentID: c.agentID, Version: c.version, Features: features,
		AutomationPaused: c.isStopped(), EmergencyStopped: c.isEmergencyStopped(),
		SafetyReason: c.safetyReason(),
	})
	if err != nil {
		return err
	}
	if err := enqueue(connectionCtx, outbound, hello); err != nil {
		return err
	}
	logger.Info("соединение с контроллером установлено", "идентификатор_агента", c.agentID)

	heartbeatDone := make(chan struct{})
	go func() {
		defer close(heartbeatDone)
		c.heartbeatLoop(connectionCtx, outbound)
	}()

	// An unbuffered queue means that while actionLoop is executing input there
	// is no receiver and every additional command is rejected immediately.
	actionQueue := make(chan actionJob)
	actionDone := make(chan struct{})
	go func() {
		defer close(actionDone)
		c.actionLoop(connectionCtx, outbound, actionQueue)
	}()

	// Only one queued or running capture is admitted. A slow capture therefore
	// cannot create an unbounded set of goroutines or retained screenshots.
	captureQueue := make(chan captureJob, 1)
	var captureBusy atomic.Bool
	captureDone := make(chan struct{})
	go func() {
		defer close(captureDone)
		c.captureLoop(connectionCtx, outbound, captureQueue, &captureBusy)
	}()

	eventDone := make(chan struct{})
	go func() {
		defer close(eventDone)
		c.eventLoop(connectionCtx, outbound)
	}()

	for {
		var incoming protocol.Envelope
		if err := wsjson.Read(connectionCtx, conn, &incoming); err != nil {
			cancel()
			<-heartbeatDone
			<-actionDone
			<-captureDone
			<-eventDone
			select {
			case writeErr := <-writerError:
				if writeErr != nil {
					return fmt.Errorf("%s: цикл записи завершился ошибкой: %w", methodCtx, writeErr)
				}
			default:
			}
			return fmt.Errorf("%s: не удалось прочитать сообщение контроллера: %w", methodCtx, err)
		}
		if err := c.handle(
			connectionCtx,
			incoming,
			outbound,
			actionQueue,
			captureQueue,
			&captureBusy,
		); err != nil {
			logger.Warn(
				"команда контроллера отклонена",
				"тип_сообщения", incoming.Type,
				"идентификатор_сообщения", incoming.MessageID,
				"ошибка", err,
			)
			event := protocol.AgentEvent{
				Kind: "PROTOCOL_ERROR", Severity: "error", Message: err.Error(), At: time.Now().UTC(),
			}
			if sendErr := c.sendEvent(
				connectionCtx,
				outbound,
				incoming.MessageID,
				event.Kind,
				event.Severity,
				event.Message,
			); sendErr != nil {
				return fmt.Errorf(
					"%s: не удалось отправить событие ошибки протокола: %w",
					methodCtx,
					sendErr,
				)
			}
		}
		select {
		case err := <-writerError:
			if err != nil {
				return err
			}
		default:
		}
	}
}

func (c *Client) eventLoop(ctx context.Context, outbound chan<- protocol.Envelope) {
	const methodCtx = "agent.Client.eventLoop"
	logger := c.logger.With("метод", methodCtx)

	sendEmergency := func(stop protocol.EmergencyStop) error {
		const methodCtx = "agent.Client.eventLoop.sendEmergency"

		envelope, err := c.envelope(protocol.MessageEmergencyStop, "", stop)
		if err != nil {
			return fmt.Errorf("%s: не удалось сформировать аварийную остановку: %w", methodCtx, err)
		}
		if err := enqueue(ctx, outbound, envelope); err != nil {
			return fmt.Errorf("%s: не удалось поставить аварийную остановку в очередь: %w", methodCtx, err)
		}
		return nil
	}
	sendAgentEvent := func(event protocol.AgentEvent) error {
		const methodCtx = "agent.Client.eventLoop.sendAgentEvent"

		envelope, err := c.envelope(protocol.MessageAgentEvent, "", event)
		if err != nil {
			return fmt.Errorf("%s: не удалось сформировать событие агента: %w", methodCtx, err)
		}
		if err := enqueue(ctx, outbound, envelope); err != nil {
			return fmt.Errorf("%s: не удалось поставить событие агента в очередь: %w", methodCtx, err)
		}
		return nil
	}

	for {
		select {
		case <-ctx.Done():
			return
		case stop := <-c.emergencies:
			if err := sendEmergency(stop); err != nil {
				logger.Error("не удалось отправить аварийную остановку", "ошибка", err)
				return
			}
		default:
			select {
			case <-ctx.Done():
				return
			case stop := <-c.emergencies:
				if err := sendEmergency(stop); err != nil {
					logger.Error("не удалось отправить аварийную остановку", "ошибка", err)
					return
				}
			case event := <-c.events:
				if err := sendAgentEvent(event); err != nil {
					logger.Error(
						"не удалось отправить событие агента",
						"тип_события", event.Kind,
						"ошибка", err,
					)
					return
				}
			}
		}
	}
}

func (c *Client) handle(
	ctx context.Context,
	incoming protocol.Envelope,
	outbound chan<- protocol.Envelope,
	actionQueue chan<- actionJob,
	captureQueue chan<- captureJob,
	captureBusy *atomic.Bool,
) error {
	const methodCtx = "agent.Client.handle"

	switch incoming.Type {
	case protocol.MessageWindowStatusRequest:
		return c.respondWindow(ctx, incoming, outbound)
	case protocol.MessageFrameRequest, protocol.MessageFrameRegionRequest:
		if captureBusy == nil || captureQueue == nil {
			return fmt.Errorf("%s: очередь захвата не настроена", methodCtx)
		}
		if !captureBusy.CompareAndSwap(false, true) {
			if err := c.sendEvent(
				ctx,
				outbound,
				incoming.MessageID,
				"CAPTURE_BUSY",
				"warning",
				methodCtx+": захват уже выполняется, новый запрос отклонён",
			); err != nil {
				return fmt.Errorf("%s: не удалось отправить состояние занятости захвата: %w", methodCtx, err)
			}
			return nil
		}
		select {
		case captureQueue <- captureJob{request: incoming}:
			return nil
		case <-ctx.Done():
			captureBusy.Store(false)
			return fmt.Errorf(
				"%s: соединение завершено до постановки запроса захвата в очередь: %w",
				methodCtx,
				ctx.Err(),
			)
		default:
			captureBusy.Store(false)
			if err := c.sendEvent(
				ctx,
				outbound,
				incoming.MessageID,
				"CAPTURE_BUSY",
				"warning",
				methodCtx+": очередь захвата заполнена, новый запрос отклонён",
			); err != nil {
				return fmt.Errorf("%s: не удалось отправить состояние очереди захвата: %w", methodCtx, err)
			}
			return nil
		}
	case protocol.MessageActionRequest:
		if len(incoming.Payload) > maxActionRequestBytes {
			return fmt.Errorf(
				"%s: ACTION_REQUEST превышает лимит %d байт",
				methodCtx,
				maxActionRequestBytes,
			)
		}
		var request protocol.ActionRequest
		if err := json.Unmarshal(incoming.Payload, &request); err != nil {
			return fmt.Errorf("%s: некорректный ACTION_REQUEST: %w", methodCtx, err)
		}
		cached, fingerprint, reserveErr := c.reserveAction(request)
		if reserveErr != nil {
			if err := c.sendActionResult(ctx, outbound, incoming.MessageID, protocol.ActionResult{
				ID: request.ID, Error: reserveErr.Error(), CompletedAt: time.Now().UTC(),
			}); err != nil {
				return fmt.Errorf("%s: не удалось отправить отказ действия: %w", methodCtx, err)
			}
			return nil
		}
		if cached != nil {
			if err := c.sendActionResult(ctx, outbound, incoming.MessageID, *cached); err != nil {
				return fmt.Errorf("%s: не удалось повторно отправить результат действия: %w", methodCtx, err)
			}
			return nil
		}
		job := actionJob{
			correlationID: incoming.MessageID,
			request:       request,
			fingerprint:   fingerprint,
		}
		select {
		case actionQueue <- job:
		default:
			c.cancelActionReservation(request.ID, fingerprint)
			result := protocol.ActionResult{
				ID: request.ID, Error: methodCtx + ": исполнитель уже занят другой командой", CompletedAt: time.Now().UTC(),
			}
			if err := c.sendActionResult(ctx, outbound, incoming.MessageID, result); err != nil {
				return fmt.Errorf("%s: не удалось отправить сообщение о занятости исполнителя: %w", methodCtx, err)
			}
			return nil
		}
	case protocol.MessageEmergencyStop:
		var stop protocol.EmergencyStop
		if err := json.Unmarshal(incoming.Payload, &stop); err != nil {
			return fmt.Errorf("%s: некорректный EMERGENCY_STOP: %w", methodCtx, err)
		}
		c.emergencyStop(stop.Reason, stop.UserInitiated)
		event := protocol.AgentEvent{
			Kind: "EMERGENCY_STOP_APPLIED", Severity: "critical", Message: stop.Reason, At: time.Now().UTC(),
		}
		response, err := c.envelope(protocol.MessageAgentEvent, incoming.MessageID, event)
		if err != nil {
			return fmt.Errorf("%s: не удалось сформировать событие аварийной остановки: %w", methodCtx, err)
		}
		if err := enqueue(ctx, outbound, response); err != nil {
			return fmt.Errorf("%s: не удалось поставить событие аварийной остановки в очередь: %w", methodCtx, err)
		}
		return nil
	default:
		return fmt.Errorf("%s: неподдерживаемая команда контроллера %q", methodCtx, incoming.Type)
	}
	return nil
}

func (c *Client) heartbeatLoop(ctx context.Context, outbound chan<- protocol.Envelope) {
	const methodCtx = "agent.Client.heartbeatLoop"
	logger := c.logger.With("метод", methodCtx)

	ticker := time.NewTicker(c.heartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case at := <-ticker.C:
			envelope, err := c.envelope(protocol.MessageHeartbeat, "", protocol.Heartbeat{
				AgentID: c.agentID, At: at.UTC(),
			})
			if err != nil {
				logger.Error("не удалось сформировать heartbeat", "ошибка", err)
				return
			}
			if err := enqueue(ctx, outbound, envelope); err != nil {
				logger.Error("не удалось поставить heartbeat в очередь отправки", "ошибка", err)
				return
			}
		}
	}
}

func (c *Client) writeLoop(
	ctx context.Context,
	conn *websocket.Conn,
	outbound <-chan protocol.Envelope,
) error {
	const methodCtx = "agent.Client.writeLoop"

	for {
		select {
		case <-ctx.Done():
			return nil
		case envelope := <-outbound:
			writeCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
			err := wsjson.Write(writeCtx, conn, envelope)
			cancel()
			if err != nil {
				return fmt.Errorf("%s: не удалось отправить %s: %w", methodCtx, envelope.Type, err)
			}
		}
	}
}

func (c *Client) respondWindow(
	ctx context.Context,
	request protocol.Envelope,
	outbound chan<- protocol.Envelope,
) error {
	const methodCtx = "agent.Client.respondWindow"

	if c.window == nil {
		if err := c.sendEvent(
			ctx,
			outbound,
			request.MessageID,
			"WINDOW_UNAVAILABLE",
			"error",
			methodCtx+": диспетчер окна не настроен",
		); err != nil {
			return fmt.Errorf("%s: не удалось отправить отсутствие диспетчера окна: %w", methodCtx, err)
		}
		return nil
	}
	status, err := c.window.Status(ctx)
	if err != nil {
		if sendErr := c.sendEvent(
			ctx,
			outbound,
			request.MessageID,
			"WINDOW_ERROR",
			"error",
			fmt.Sprintf("%s: не удалось получить состояние окна: %v", methodCtx, err),
		); sendErr != nil {
			return fmt.Errorf("%s: не удалось отправить ошибку диспетчера окна: %w", methodCtx, sendErr)
		}
		return nil
	}
	response, err := c.envelope(protocol.MessageWindowStatus, request.MessageID, status)
	if err != nil {
		return fmt.Errorf("%s: не удалось сформировать состояние окна: %w", methodCtx, err)
	}
	if err := enqueue(ctx, outbound, response); err != nil {
		return fmt.Errorf("%s: не удалось поставить состояние окна в очередь: %w", methodCtx, err)
	}
	return nil
}

type captureJob struct {
	request protocol.Envelope
}

func (c *Client) captureLoop(
	ctx context.Context,
	outbound chan<- protocol.Envelope,
	queue <-chan captureJob,
	busy *atomic.Bool,
) {
	const methodCtx = "agent.Client.captureLoop"
	logger := c.logger.With("метод", methodCtx)

	for {
		select {
		case <-ctx.Done():
			return
		case job := <-queue:
			err := func() error {
				defer busy.Store(false)
				return c.respondFrame(ctx, job.request, outbound)
			}()
			if err != nil {
				logger.Error(
					"не удалось обработать запрос захвата",
					"идентификатор_сообщения", job.request.MessageID,
					"ошибка", err,
				)
			}
		}
	}
}

func (c *Client) respondFrame(
	ctx context.Context,
	request protocol.Envelope,
	outbound chan<- protocol.Envelope,
) error {
	const methodCtx = "agent.Client.respondFrame"

	if c.capture == nil {
		if err := c.sendEvent(
			ctx,
			outbound,
			request.MessageID,
			"CAPTURE_UNAVAILABLE",
			"error",
			methodCtx+": драйвер захвата не настроен",
		); err != nil {
			return fmt.Errorf("%s: не удалось отправить отсутствие драйвера захвата: %w", methodCtx, err)
		}
		return nil
	}
	var parameters protocol.FrameRequest
	if err := json.Unmarshal(request.Payload, &parameters); err != nil {
		if sendErr := c.sendEvent(
			ctx,
			outbound,
			request.MessageID,
			"CAPTURE_REQUEST_INVALID",
			"error",
			fmt.Sprintf("%s: не удалось разобрать параметры захвата: %v", methodCtx, err),
		); sendErr != nil {
			return fmt.Errorf("%s: не удалось отправить ошибку параметров захвата: %w", methodCtx, sendErr)
		}
		return nil
	}
	c.captureMu.Lock()
	var (
		frame protocol.Frame
		err   error
	)
	if parameters.Region != nil {
		frame, err = c.capture.CaptureRegion(ctx, *parameters.Region)
	} else {
		frame, err = c.capture.Capture(ctx)
	}
	c.captureMu.Unlock()
	if err != nil {
		if sendErr := c.sendEvent(
			ctx,
			outbound,
			request.MessageID,
			"CAPTURE_ERROR",
			"error",
			fmt.Sprintf("%s: не удалось захватить кадр: %v", methodCtx, err),
		); sendErr != nil {
			return fmt.Errorf("%s: не удалось отправить ошибку захвата: %w", methodCtx, sendErr)
		}
		return nil
	}
	messageType := protocol.MessageFrame
	if parameters.Region != nil {
		messageType = protocol.MessageFrameRegion
	}
	response, err := c.envelope(messageType, request.MessageID, frame)
	if err != nil {
		return fmt.Errorf("%s: не удалось сформировать сообщение с кадром: %w", methodCtx, err)
	}
	if err := enqueue(ctx, outbound, response); err != nil {
		return fmt.Errorf("%s: не удалось поставить кадр в очередь отправки: %w", methodCtx, err)
	}
	return nil
}

type actionJob struct {
	correlationID string
	request       protocol.ActionRequest
	fingerprint   [sha256.Size]byte
}

func (c *Client) actionLoop(ctx context.Context, outbound chan<- protocol.Envelope, queue <-chan actionJob) {
	const methodCtx = "agent.Client.actionLoop"
	logger := c.logger.With("метод", methodCtx)

	for {
		select {
		case <-ctx.Done():
			return
		case job := <-queue:
			var result protocol.ActionResult
			switch {
			case c.executor == nil:
				result = protocol.ActionResult{
					ID: job.request.ID, Error: methodCtx + ": исполнитель действий не настроен", CompletedAt: time.Now().UTC(),
				}
			case c.isStopped():
				result = protocol.ActionResult{
					ID: job.request.ID, Error: methodCtx + ": агент остановлен", CompletedAt: time.Now().UTC(),
				}
			default:
				func() {
					actionCtx, cancel := context.WithDeadline(ctx, job.request.Deadline)
					defer cancel()
					c.captureMu.Lock()
					defer c.captureMu.Unlock()
					result = c.executor.Execute(actionCtx, job.request)
				}()
			}
			c.rememberActionResult(job.request.ID, job.fingerprint, result)
			c.onActionResult(result)
			if err := c.sendActionResult(ctx, outbound, job.correlationID, result); err != nil {
				logger.Error(
					"не удалось отправить результат действия",
					"идентификатор_действия", job.request.ID,
					"идентификатор_сообщения", job.correlationID,
					"ошибка", err,
				)
				if ctx.Err() != nil {
					return
				}
			}
		}
	}
}

func (c *Client) sendActionResult(
	ctx context.Context,
	outbound chan<- protocol.Envelope,
	correlationID string,
	result protocol.ActionResult,
) error {
	const methodCtx = "agent.Client.sendActionResult"

	response, err := c.envelope(protocol.MessageActionResult, correlationID, result)
	if err != nil {
		return fmt.Errorf("%s: не удалось сформировать ACTION_RESULT: %w", methodCtx, err)
	}
	if err := enqueue(ctx, outbound, response); err != nil {
		return fmt.Errorf("%s: не удалось поставить ACTION_RESULT в очередь: %w", methodCtx, err)
	}
	return nil
}

// reserveAction provides at-most-once input semantics across controller
// timeouts and reconnects. A completed request is replayed; the same ID with a
// different body is rejected and can never become a second click.
func (c *Client) reserveAction(request protocol.ActionRequest) (*protocol.ActionResult, [sha256.Size]byte, error) {
	const methodCtx = "agent.Client.reserveAction"

	var zero [sha256.Size]byte
	if request.ID == "" {
		return nil, zero, fmt.Errorf("%s: ACTION_REQUEST не содержит идентификатор", methodCtx)
	}
	if len(request.ID) > maxActionIdentifierBytes {
		return nil, zero, fmt.Errorf(
			"%s: идентификатор действия превышает лимит %d байт",
			methodCtx,
			maxActionIdentifierBytes,
		)
	}
	if len(request.SessionID) > maxSessionIdentifierBytes {
		return nil, zero, fmt.Errorf(
			"%s: идентификатор сессии превышает лимит %d байт",
			methodCtx,
			maxSessionIdentifierBytes,
		)
	}
	payload, err := json.Marshal(request)
	if err != nil {
		return nil, zero, fmt.Errorf("%s: не удалось вычислить отпечаток ACTION_REQUEST: %w", methodCtx, err)
	}
	if len(payload) > maxActionRequestBytes {
		return nil, zero, fmt.Errorf(
			"%s: ACTION_REQUEST превышает лимит %d байт",
			methodCtx,
			maxActionRequestBytes,
		)
	}
	fingerprint := sha256.Sum256(payload)

	c.actionMu.Lock()
	defer c.actionMu.Unlock()
	if tombstone, exists := c.actions[request.ID]; exists {
		if tombstone.fingerprint != fingerprint {
			return nil, fingerprint, fmt.Errorf("%s: идентификатор действия уже использован для другой команды", methodCtx)
		}
		if !tombstone.completed {
			return nil, fingerprint, fmt.Errorf("%s: эта команда уже выполняется", methodCtx)
		}
		if cached, ok := c.actionResults[request.ID]; ok {
			result := cached.result
			return &result, fingerprint, nil
		}
		return nil, fingerprint, fmt.Errorf(
			"%s: команда уже была выполнена, но её результат удалён из ограниченного кэша; повторное исполнение запрещено",
			methodCtx,
		)
	}
	if c.hasSeenActionLocked(request.ID) {
		return nil, fingerprint, fmt.Errorf(
			"%s: идентификатор действия уже использовался; повторное исполнение запрещено",
			methodCtx,
		)
	}
	c.evictActionTombstoneLocked()
	if len(c.actions) >= maxRememberedActionTombstones {
		return nil, fingerprint, fmt.Errorf(
			"%s: безопасный журнал действий заполнен; новые команды запрещены до перезапуска агента",
			methodCtx,
		)
	}
	c.actions[request.ID] = actionTombstone{fingerprint: fingerprint}
	return nil, fingerprint, nil
}

func (c *Client) cancelActionReservation(id string, fingerprint [sha256.Size]byte) {
	c.actionMu.Lock()
	defer c.actionMu.Unlock()
	tombstone, exists := c.actions[id]
	if exists && !tombstone.completed && tombstone.fingerprint == fingerprint {
		delete(c.actions, id)
	}
}

func (c *Client) rememberActionResult(
	id string,
	fingerprint [sha256.Size]byte,
	result protocol.ActionResult,
) {
	frameBytes := 0
	if result.Frame != nil {
		frameBytes = len(result.Frame.Data)
	}

	c.actionMu.Lock()
	defer c.actionMu.Unlock()
	tombstone, exists := c.actions[id]
	if !exists || tombstone.fingerprint != fingerprint || tombstone.completed {
		return
	}
	tombstone.completed = true
	c.actions[id] = tombstone
	c.actionOrder = append(c.actionOrder, id)
	c.markActionSeenLocked(id)

	// Oversized frames are deliberately not retained. The compact tombstone
	// remains, so a retry is rejected instead of executing the input again.
	if frameBytes <= maxRememberedActionBytes {
		c.actionResults[id] = cachedActionResult{
			result:     cloneActionResultForCache(result),
			frameBytes: frameBytes,
		}
		c.actionResultOrder = append(c.actionResultOrder, id)
		c.actionResultBytes += frameBytes
	}

	for len(c.actionResults) > maxRememberedActions ||
		c.actionResultBytes > maxRememberedActionBytes {
		oldest := c.actionResultOrder[0]
		c.actionResultOrder = c.actionResultOrder[1:]
		value, ok := c.actionResults[oldest]
		if !ok {
			continue
		}
		c.actionResultBytes -= value.frameBytes
		delete(c.actionResults, oldest)
	}
}

func cloneActionResultForCache(result protocol.ActionResult) protocol.ActionResult {
	if result.Frame == nil {
		return result
	}
	frame := *result.Frame
	frame.Data = make([]byte, len(result.Frame.Data))
	copy(frame.Data, result.Frame.Data)
	result.Frame = &frame
	return result
}

// evictActionTombstoneLocked keeps exact fingerprints for the most recent
// actions. The fixed-size probabilistic filter still remembers an evicted ID,
// so eviction can only cause a safe false-positive rejection, never a click.
func (c *Client) evictActionTombstoneLocked() {
	for len(c.actions) >= maxRememberedActionTombstones && len(c.actionOrder) > 0 {
		oldest := c.actionOrder[0]
		c.actionOrder = c.actionOrder[1:]
		tombstone, ok := c.actions[oldest]
		if !ok || !tombstone.completed {
			continue
		}
		delete(c.actions, oldest)
		if cached, ok := c.actionResults[oldest]; ok {
			c.actionResultBytes -= cached.frameBytes
			delete(c.actionResults, oldest)
		}
	}
}

func (c *Client) hasSeenActionLocked(id string) bool {
	digest := sha256.Sum256([]byte(id))
	for index := 0; index < seenActionFilterHashCount; index++ {
		value := uint64(0)
		for offset := 0; offset < 8; offset++ {
			value = value<<8 | uint64(digest[index*8+offset])
		}
		bit := value % seenActionFilterBits
		if c.seenActions[bit/64]&(uint64(1)<<(bit%64)) == 0 {
			return false
		}
	}
	return true
}

func (c *Client) markActionSeenLocked(id string) {
	digest := sha256.Sum256([]byte(id))
	for index := 0; index < seenActionFilterHashCount; index++ {
		value := uint64(0)
		for offset := 0; offset < 8; offset++ {
			value = value<<8 | uint64(digest[index*8+offset])
		}
		bit := value % seenActionFilterBits
		c.seenActions[bit/64] |= uint64(1) << (bit % 64)
	}
}

func (c *Client) sendEvent(
	ctx context.Context,
	outbound chan<- protocol.Envelope,
	correlationID, kind, severity, message string,
) error {
	const methodCtx = "agent.Client.sendEvent"

	event := protocol.AgentEvent{Kind: kind, Severity: severity, Message: message, At: time.Now().UTC()}
	response, err := c.envelope(protocol.MessageAgentEvent, correlationID, event)
	if err != nil {
		return fmt.Errorf("%s: не удалось сформировать событие агента %s: %w", methodCtx, kind, err)
	}
	if err := enqueue(ctx, outbound, response); err != nil {
		return fmt.Errorf("%s: не удалось поставить событие агента %s в очередь: %w", methodCtx, kind, err)
	}
	return nil
}

func (c *Client) envelope(messageType protocol.MessageType, correlationID string, payload any) (protocol.Envelope, error) {
	const methodCtx = "agent.Client.envelope"

	messageID := fmt.Sprintf("%s-%d-%d", c.agentID, time.Now().UnixNano(), c.sequence.Add(1))
	envelope, err := protocol.NewCorrelatedEnvelope(messageType, messageID, correlationID, payload)
	if err != nil {
		return protocol.Envelope{}, fmt.Errorf("%s: не удалось сформировать сообщение %s: %w", methodCtx, messageType, err)
	}
	return envelope, nil
}

func enqueue(ctx context.Context, outbound chan<- protocol.Envelope, envelope protocol.Envelope) error {
	const methodCtx = "agent.enqueue"

	select {
	case outbound <- envelope:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("%s: контекст завершён до постановки сообщения в очередь: %w", methodCtx, ctx.Err())
	}
}

func jitter(value time.Duration) time.Duration {
	if value <= 1 {
		return value
	}
	// 80–120%, bounded and deterministic enough for a single local client.
	width := value / 5
	return value - width + time.Duration(rand.Int64N(int64(width*2)+1))
}

// IsConnectionError helps callers distinguish a normal cancellation from a
// transport failure if they embed a single connection in tests.
func IsConnectionError(err error) bool {
	return err != nil && !errors.Is(err, context.Canceled)
}

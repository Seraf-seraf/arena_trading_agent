package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/arena-trading-agent/arena-trading-agent/internal/domain"
	"github.com/arena-trading-agent/arena-trading-agent/internal/protocol"
)

// RequestFrame запрашивает свежий полный кадр и ждёт коррелированный FRAME.
// Метод разрешён во всех режимах, включая PAUSED.
func (s *Server) RequestFrame(ctx context.Context, agentID string, afterFrame uint64) (protocol.Frame, error) {
	const methodCtx = "controller.Server.RequestFrame"

	frame, err := s.requestFrame(ctx, agentID, "", protocol.FrameRequest{AfterFrame: afterFrame})
	if err != nil {
		return protocol.Frame{}, fmt.Errorf("%s: не удалось получить кадр: %w", methodCtx, err)
	}
	return frame, nil
}

// RequestFrameRegion запрашивает свежий crop в нормализованных координатах.
func (s *Server) RequestFrameRegion(ctx context.Context, agentID string, region domain.Rectangle, afterFrame uint64) (protocol.Frame, error) {
	const methodCtx = "controller.Server.RequestFrameRegion"

	if !validRectangle(region) {
		return protocol.Frame{}, fmt.Errorf("%s: область кадра выходит за нормализованную клиентскую область", methodCtx)
	}
	frame, err := s.requestFrame(ctx, agentID, "", protocol.FrameRequest{AfterFrame: afterFrame, Region: &region})
	if err != nil {
		return protocol.Frame{}, fmt.Errorf("%s: не удалось получить область кадра: %w", methodCtx, err)
	}
	return frame, nil
}

func (s *Server) requestFrame(ctx context.Context, agentID, requestID string, request protocol.FrameRequest) (protocol.Frame, error) {
	const methodCtx = "controller.Server.requestFrame"

	messageType := protocol.MessageFrameRequest
	if request.Region != nil {
		messageType = protocol.MessageFrameRegionRequest
	}
	envelope, err := s.request(ctx, agentID, messageType, requestID, request)
	if err != nil {
		return protocol.Frame{}, fmt.Errorf("%s: запрос кадра завершился ошибкой: %w", methodCtx, err)
	}
	expectedType := protocol.MessageFrame
	if request.Region != nil {
		expectedType = protocol.MessageFrameRegion
	}
	if envelope.Type != expectedType {
		return protocol.Frame{}, fmt.Errorf(
			"%s: %w: ожидался %s, получен %s",
			methodCtx,
			ErrUnexpectedResponse,
			expectedType,
			envelope.Type,
		)
	}
	var frame protocol.Frame
	if err := json.Unmarshal(envelope.Payload, &frame); err != nil {
		return protocol.Frame{}, fmt.Errorf("%s: не удалось декодировать %s: %w", methodCtx, expectedType, err)
	}
	frame, err = normalizeInboundFrame(frame)
	if err != nil {
		return protocol.Frame{}, fmt.Errorf("%s: получен некорректный %s: %w", methodCtx, expectedType, err)
	}
	if frame.ID <= request.AfterFrame {
		return protocol.Frame{}, fmt.Errorf(
			"%s: %w: кадр %d не новее after_frame=%d",
			methodCtx,
			ErrUnexpectedResponse,
			frame.ID,
			request.AfterFrame,
		)
	}
	return frame, nil
}

// RequestWindowStatus запрашивает актуальные window/DPI/foreground условия.
func (s *Server) RequestWindowStatus(ctx context.Context, agentID string) (protocol.WindowStatus, error) {
	const methodCtx = "controller.Server.RequestWindowStatus"

	status, err := s.requestWindowStatus(ctx, agentID, "")
	if err != nil {
		return protocol.WindowStatus{}, fmt.Errorf("%s: не удалось получить состояние окна: %w", methodCtx, err)
	}
	return status, nil
}

func (s *Server) requestWindowStatus(ctx context.Context, agentID, requestID string) (protocol.WindowStatus, error) {
	const methodCtx = "controller.Server.requestWindowStatus"

	envelope, err := s.request(ctx, agentID, protocol.MessageWindowStatusRequest, requestID, protocol.WindowStatusRequest{})
	if err != nil {
		return protocol.WindowStatus{}, fmt.Errorf("%s: запрос состояния окна завершился ошибкой: %w", methodCtx, err)
	}
	if envelope.Type != protocol.MessageWindowStatus {
		return protocol.WindowStatus{}, fmt.Errorf("%s: %w: ожидался WINDOW_STATUS, получен %s", methodCtx, ErrUnexpectedResponse, envelope.Type)
	}
	var status protocol.WindowStatus
	if err := json.Unmarshal(envelope.Payload, &status); err != nil {
		return protocol.WindowStatus{}, fmt.Errorf("%s: не удалось декодировать WINDOW_STATUS: %w", methodCtx, err)
	}
	return status, nil
}

// RequestAction отправляет единственную input-команду и ждёт ACTION_RESULT.
// Ввод разрешён только после явного перехода controller в SCAN либо TRADE.
func (s *Server) RequestAction(ctx context.Context, agentID string, request protocol.ActionRequest) (protocol.ActionResult, error) {
	const methodCtx = "controller.Server.RequestAction"

	mode := s.Mode()
	if mode != domain.ModeScan && mode != domain.ModeTrade {
		return protocol.ActionResult{}, fmt.Errorf("%s: %w: режим %s", methodCtx, ErrModeDisallowsInput, mode)
	}
	if request.ID == "" {
		request.ID = s.nextID("action")
	}
	if request.Class == "" {
		request.Class = protocol.ActionNavigation
	}
	if mode == domain.ModeScan && request.Class != protocol.ActionNavigation {
		return protocol.ActionResult{}, fmt.Errorf(
			"%s: %w: режим %s, класс %s",
			methodCtx,
			ErrModeDisallowsMoney,
			mode,
			request.Class,
		)
	}
	if request.Deadline.IsZero() {
		request.Deadline = time.Now().UTC().Add(s.requestTimeout)
	}
	if err := s.enrichActionBasis(agentID, &request); err != nil {
		return protocol.ActionResult{}, fmt.Errorf("%s: не удалось привязать действие к исходному кадру: %w", methodCtx, err)
	}
	if err := validateActionRequest(request); err != nil {
		return protocol.ActionResult{}, fmt.Errorf("%s: запрос действия не прошёл проверку: %w", methodCtx, err)
	}

	envelope, err := s.requestWithSendGuard(
		ctx,
		agentID,
		protocol.MessageActionRequest,
		request.ID,
		request,
		func(prepareCtx context.Context) error {
			const methodCtx = "controller.Server.RequestAction.prepare"

			if err := s.persistActionRequest(prepareCtx, agentID, request); err != nil {
				return fmt.Errorf(
					"%s: не удалось сохранить аудит действия: %w",
					methodCtx,
					err,
				)
			}
			return nil
		},
		func(context.Context) error {
			const methodCtx = "controller.Server.RequestAction.sendGuard"

			mode := s.Mode()
			if mode != domain.ModeScan && mode != domain.ModeTrade {
				return fmt.Errorf("%s: %w: режим %s", methodCtx, ErrModeDisallowsInput, mode)
			}
			if mode == domain.ModeScan && request.Class != protocol.ActionNavigation {
				return fmt.Errorf(
					"%s: %w: режим %s, класс %s",
					methodCtx,
					ErrModeDisallowsMoney,
					mode,
					request.Class,
				)
			}
			return nil
		},
	)
	if err != nil {
		requestErr := fmt.Errorf("%s: запрос действия завершился ошибкой: %w", methodCtx, err)
		if errors.Is(err, ErrActionNotSent) {
			if auditErr := s.persistUnsentActionResult(agentID, request, requestErr); auditErr != nil {
				s.pause()
				return protocol.ActionResult{}, errors.Join(
					requestErr,
					fmt.Errorf(
						"%s: не удалось закрыть аудит неотправленного действия: %w",
						methodCtx,
						auditErr,
					),
				)
			}
		}
		return protocol.ActionResult{}, requestErr
	}
	if envelope.Type != protocol.MessageActionResult {
		return protocol.ActionResult{}, fmt.Errorf("%s: %w: ожидался ACTION_RESULT, получен %s", methodCtx, ErrUnexpectedResponse, envelope.Type)
	}
	var result protocol.ActionResult
	if err := json.Unmarshal(envelope.Payload, &result); err != nil {
		return protocol.ActionResult{}, fmt.Errorf("%s: не удалось декодировать ACTION_RESULT: %w", methodCtx, err)
	}
	if result.ID != request.ID {
		return protocol.ActionResult{}, fmt.Errorf("%s: %w: идентификатор действия %q, идентификатор результата %q", methodCtx, ErrUnexpectedResponse, request.ID, result.ID)
	}
	return result, nil
}

// SendEmergencyStop немедленно переводит controller в PAUSED, отправляет
// локальному supervisor команду остановки и ждёт её коррелированное принятие.
func (s *Server) SendEmergencyStop(ctx context.Context, agentID, reason string) error {
	const methodCtx = "controller.Server.SendEmergencyStop"

	s.pause()
	if reason == "" {
		reason = "остановка запрошена контроллером"
	}
	messageID := s.nextID("emergency-stop")
	envelope, err := s.request(ctx, agentID, protocol.MessageEmergencyStop, messageID, protocol.EmergencyStop{
		Reason: reason,
		At:     time.Now().UTC(),
	})
	if err != nil {
		return fmt.Errorf("%s: агент не подтвердил аварийную остановку: %w", methodCtx, err)
	}
	if envelope.Type != protocol.MessageAgentEvent {
		return fmt.Errorf(
			"%s: %w: ожидался AGENT_EVENT, получен %s",
			methodCtx,
			ErrUnexpectedResponse,
			envelope.Type,
		)
	}
	var event protocol.AgentEvent
	if err := json.Unmarshal(envelope.Payload, &event); err != nil {
		return fmt.Errorf("%s: не удалось декодировать подтверждение остановки: %w", methodCtx, err)
	}
	if event.Kind != "EMERGENCY_STOP_APPLIED" {
		return fmt.Errorf(
			"%s: %w: агент вернул событие %q вместо EMERGENCY_STOP_APPLIED",
			methodCtx,
			ErrUnexpectedResponse,
			event.Kind,
		)
	}
	return nil
}

func (s *Server) request(ctx context.Context, agentID string, messageType protocol.MessageType, requestID string, payload any) (protocol.Envelope, error) {
	const methodCtx = "controller.Server.request"

	envelope, err := s.requestWithSendGuard(
		ctx,
		agentID,
		messageType,
		requestID,
		payload,
		nil,
		nil,
	)
	if err != nil {
		return protocol.Envelope{}, fmt.Errorf("%s: запрос %s завершился ошибкой: %w", methodCtx, messageType, err)
	}
	return envelope, nil
}

func (s *Server) requestWithSendGuard(
	ctx context.Context,
	agentID string,
	messageType protocol.MessageType,
	requestID string,
	payload any,
	prepare func(context.Context) error,
	sendGuard func(context.Context) error,
) (protocol.Envelope, error) {
	const methodCtx = "controller.Server.requestWithSendGuard"

	agent, ok := s.findAgent(agentID)
	if !ok {
		return protocol.Envelope{}, fmt.Errorf("%s: агент недоступен: %w", methodCtx, ErrAgentNotConnected)
	}
	if requestID == "" {
		requestID = s.nextID("request")
	}
	envelope, err := protocol.NewEnvelope(messageType, requestID, payload)
	if err != nil {
		return protocol.Envelope{}, fmt.Errorf("%s: не удалось сформировать сообщение %s: %w", methodCtx, messageType, err)
	}

	expectedType, err := expectedResponseType(messageType)
	if err != nil {
		return protocol.Envelope{}, fmt.Errorf("%s: нельзя определить тип ответа на %s: %w", methodCtx, messageType, err)
	}
	response, err := agent.addPending(requestID, expectedType)
	if err != nil {
		return protocol.Envelope{}, fmt.Errorf("%s: не удалось зарегистрировать ожидающий запрос %q: %w", methodCtx, requestID, err)
	}
	defer agent.removePending(requestID)

	requestCtx, cancel := withDefaultTimeout(ctx, s.requestTimeout)
	defer cancel()
	if prepare != nil {
		if err := prepare(requestCtx); err != nil {
			return protocol.Envelope{}, fmt.Errorf(
				"%s: подготовка запроса %s завершилась ошибкой: %w",
				methodCtx,
				messageType,
				err,
			)
		}
	}
	if err := s.sendRequest(requestCtx, agent, envelope, sendGuard); err != nil {
		return protocol.Envelope{}, fmt.Errorf("%s: не удалось отправить запрос %s: %w", methodCtx, messageType, err)
	}

	select {
	case value := <-response:
		if value.err != nil {
			return protocol.Envelope{}, fmt.Errorf("%s: агент вернул ошибку: %w", methodCtx, value.err)
		}
		return value.envelope, nil
	case <-requestCtx.Done():
		return protocol.Envelope{}, fmt.Errorf("%s: превышено время ожидания ответа %s: %w", methodCtx, messageType, requestCtx.Err())
	case <-agent.closed:
		// shutdown также публикует точную причину в response. Даём ей приоритет,
		// если она уже доступна.
		select {
		case value := <-response:
			if value.err != nil {
				return protocol.Envelope{}, fmt.Errorf("%s: соединение агента закрыто: %w", methodCtx, value.err)
			}
			return value.envelope, nil
		default:
			return protocol.Envelope{}, fmt.Errorf("%s: соединение агента закрыто: %w", methodCtx, ErrAgentNotConnected)
		}
	}
}

func (s *Server) sendRequest(
	ctx context.Context,
	agent *agentConnection,
	envelope protocol.Envelope,
	guard func(context.Context) error,
) error {
	const methodCtx = "controller.Server.sendRequest"

	if guard == nil {
		if err := agent.send(ctx, envelope); err != nil {
			return fmt.Errorf("%s: не удалось отправить сообщение: %w", methodCtx, err)
		}
		return nil
	}
	s.actionGate.RLock()
	defer s.actionGate.RUnlock()
	sendCtx, unregister := s.registerGuardedSend(ctx)
	defer unregister()
	if err := ctx.Err(); err != nil {
		return errors.Join(
			ErrActionNotSent,
			fmt.Errorf("%s: контекст завершён до отправки защищённого сообщения: %w", methodCtx, err),
		)
	}
	if err := guard(sendCtx); err != nil {
		return errors.Join(
			ErrActionNotSent,
			fmt.Errorf("%s: защитная проверка запретила отправку: %w", methodCtx, err),
		)
	}
	if err := agent.send(sendCtx, envelope); err != nil {
		return fmt.Errorf("%s: не удалось отправить защищённое сообщение: %w", methodCtx, err)
	}
	return nil
}

func withDefaultTimeout(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	if _, exists := ctx.Deadline(); exists {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, timeout)
}

func validateActionRequest(request protocol.ActionRequest) error {
	const methodCtx = "controller.validateActionRequest"

	if request.ID == "" {
		return fmt.Errorf("%s: запрос действия не содержит идентификатор", methodCtx)
	}
	if strings.TrimSpace(request.SessionID) == "" {
		return fmt.Errorf("%s: запрос действия не содержит session_id", methodCtx)
	}
	if request.BasedOnFrame == 0 {
		return fmt.Errorf("%s: запрос действия не содержит based_on_frame", methodCtx)
	}
	hasCapturedAt := request.BasedOnCapturedAt != nil
	hasDigest := request.BasedOnFrameDigest != ""
	if hasCapturedAt != hasDigest {
		return fmt.Errorf("%s: timestamp и digest исходного кадра должны передаваться вместе", methodCtx)
	}
	if hasCapturedAt {
		if request.BasedOnCapturedAt.IsZero() {
			return fmt.Errorf("%s: based_on_captured_at содержит нулевое время", methodCtx)
		}
		if !protocol.ValidFrameDigest(request.BasedOnFrameDigest) {
			return fmt.Errorf("%s: based_on_frame_digest имеет некорректный формат", methodCtx)
		}
	}
	if err := protocol.ValidateFrameRegionBasis(request.FrameBasis); err != nil {
		return fmt.Errorf("%s: frame_basis не прошёл проверку: %w", methodCtx, err)
	}
	if request.BasedOnState != "" && !knownExpectedState(request.BasedOnState) {
		return fmt.Errorf("%s: запрос действия содержит недопустимый based_on_state %q", methodCtx, request.BasedOnState)
	}
	if !knownExpectedState(request.ExpectedState) {
		return fmt.Errorf("%s: запрос действия содержит недопустимый expected_state %q", methodCtx, request.ExpectedState)
	}
	if math.IsNaN(request.MinVerificationConfidence) ||
		math.IsInf(request.MinVerificationConfidence, 0) ||
		request.MinVerificationConfidence < 0 ||
		request.MinVerificationConfidence > 1 {
		return fmt.Errorf(
			"%s: min_verification_confidence %.3f находится вне диапазона [0, 1]",
			methodCtx,
			request.MinVerificationConfidence,
		)
	}
	if request.Deadline.Before(time.Now()) {
		return fmt.Errorf("%s: срок действия запроса уже истёк", methodCtx)
	}
	if request.ExpectedWidth <= 0 || request.ExpectedHeight <= 0 || request.ExpectedDPIPercent <= 0 {
		return fmt.Errorf("%s: запрос действия требует положительные expected_width, expected_height и expected_dpi_percent", methodCtx)
	}
	switch request.Class {
	case protocol.ActionNavigation, protocol.ActionPurchase, protocol.ActionBarter,
		protocol.ActionListing, protocol.ActionReprice:
	default:
		return fmt.Errorf("%s: запрос действия содержит недопустимый класс %q", methodCtx, request.Class)
	}
	if request.Class != protocol.ActionNavigation && len(request.FrameBasis) == 0 {
		return fmt.Errorf(
			"%s: денежное действие класса %s требует непустое frame_basis",
			methodCtx,
			request.Class,
		)
	}
	if err := validateAction(request.Action, false); err != nil {
		return fmt.Errorf("%s: действие не прошло проверку: %w", methodCtx, err)
	}
	return nil
}

func (s *Server) enrichActionBasis(agentID string, request *protocol.ActionRequest) error {
	const methodCtx = "controller.Server.enrichActionBasis"

	if request == nil {
		return fmt.Errorf("%s: запрос действия не задан", methodCtx)
	}
	agent, ok := s.findAgent(agentID)
	if !ok {
		return fmt.Errorf("%s: агент недоступен: %w", methodCtx, ErrAgentNotConnected)
	}
	sourceFrame, latestFrameID, ok := agent.frameBasis(request.BasedOnFrame)
	if !ok {
		if latestFrameID != 0 {
			return fmt.Errorf(
				"%s: команда основана на кадре %d, последний кадр агента %d",
				methodCtx,
				request.BasedOnFrame,
				latestFrameID,
			)
		}
		// Старый клиент controller мог сформировать запрос до того, как сервер
		// начал хранить метаданные кадров. JSON остаётся совместимым, но новый
		// Windows Agent безопасно отклонит такую команду.
		return nil
	}
	capturedAt := sourceFrame.CapturedAt
	digest := sourceFrame.ContentDigest
	if request.BasedOnCapturedAt != nil &&
		!request.BasedOnCapturedAt.Equal(capturedAt) {
		return fmt.Errorf("%s: timestamp команды не совпадает с кадром %d", methodCtx, request.BasedOnFrame)
	}
	if request.BasedOnFrameDigest != "" &&
		request.BasedOnFrameDigest != digest {
		return fmt.Errorf("%s: digest команды не совпадает с кадром %d", methodCtx, request.BasedOnFrame)
	}
	if len(request.FrameBasis) > 0 {
		if err := protocol.VerifyFrameRegionBasis(sourceFrame, request.FrameBasis); err != nil {
			return fmt.Errorf(
				"%s: frame_basis не соответствует сохранённому кадру %d: %w",
				methodCtx,
				request.BasedOnFrame,
				err,
			)
		}
	}
	request.BasedOnCapturedAt = &capturedAt
	request.BasedOnFrameDigest = digest
	return nil
}

func expectedResponseType(requestType protocol.MessageType) (protocol.MessageType, error) {
	const methodCtx = "controller.expectedResponseType"

	switch requestType {
	case protocol.MessageFrameRequest:
		return protocol.MessageFrame, nil
	case protocol.MessageFrameRegionRequest:
		return protocol.MessageFrameRegion, nil
	case protocol.MessageWindowStatusRequest:
		return protocol.MessageWindowStatus, nil
	case protocol.MessageActionRequest:
		return protocol.MessageActionResult, nil
	case protocol.MessageEmergencyStop:
		return protocol.MessageAgentEvent, nil
	default:
		return "", fmt.Errorf("%s: для запроса %s не определён тип ответа", methodCtx, requestType)
	}
}

func normalizeInboundFrame(frame protocol.Frame) (protocol.Frame, error) {
	const methodCtx = "controller.normalizeInboundFrame"

	if frame.ID == 0 {
		return protocol.Frame{}, fmt.Errorf("%s: идентификатор кадра равен нулю", methodCtx)
	}
	if frame.CapturedAt.IsZero() {
		return protocol.Frame{}, fmt.Errorf("%s: captured_at кадра не задан", methodCtx)
	}
	if strings.TrimSpace(frame.Encoding) == "" || len(frame.Data) == 0 {
		return protocol.Frame{}, fmt.Errorf("%s: кадр не содержит encoding или data", methodCtx)
	}
	frame, err := protocol.NormalizeFrameDigest(frame)
	if err != nil {
		return protocol.Frame{}, fmt.Errorf("%s: digest кадра не прошёл проверку: %w", methodCtx, err)
	}
	return frame, nil
}

func validateAction(action protocol.Action, nested bool) error {
	const methodCtx = "controller.validateAction"

	switch action.Kind {
	case "MOVE":
		if action.Point == nil || !validPoint(*action.Point) {
			return fmt.Errorf("%s: MOVE содержит некорректную нормализованную координату", methodCtx)
		}
	case "CLICK":
		if action.Point != nil && !validPoint(*action.Point) {
			return fmt.Errorf("%s: CLICK содержит некорректную нормализованную координату", methodCtx)
		}
		switch strings.ToUpper(strings.TrimSpace(action.Value)) {
		case "", "LEFT", "PRIMARY", "RIGHT", "SECONDARY", "MIDDLE", "X1", "X2":
		default:
			return fmt.Errorf("%s: CLICK содержит неподдерживаемую кнопку %q", methodCtx, action.Value)
		}
	case "SCROLL":
		if action.Point != nil && !validPoint(*action.Point) {
			return fmt.Errorf("%s: SCROLL содержит некорректную нормализованную координату", methodCtx)
		}
		if action.Delta == 0 {
			return fmt.Errorf("%s: SCROLL содержит нулевое смещение", methodCtx)
		}
	case "KEY":
		if action.Value == "" {
			return fmt.Errorf("%s: KEY не содержит клавишу", methodCtx)
		}
	case "TEXT":
		if action.Value == "" {
			return fmt.Errorf("%s: TEXT не содержит текст", methodCtx)
		}
	case "SEQUENCE":
		if nested {
			return fmt.Errorf("%s: вложенная SEQUENCE запрещена", methodCtx)
		}
		if len(action.Steps) == 0 || len(action.Steps) > 64 {
			return fmt.Errorf("%s: SEQUENCE должна содержать от 1 до 64 шагов", methodCtx)
		}
		for index, step := range action.Steps {
			if err := validateAction(step, true); err != nil {
				return fmt.Errorf("%s: шаг %d последовательности: %w", methodCtx, index+1, err)
			}
		}
	default:
		return fmt.Errorf("%s: неподдерживаемое действие %q", methodCtx, action.Kind)
	}
	return nil
}

func knownExpectedState(state domain.ScreenState) bool {
	switch state {
	case domain.StateMainMenu,
		domain.StateMarketHome,
		domain.StateMarketSearch,
		domain.StateMarketResults,
		domain.StateItemCard,
		domain.StatePurchaseDialog,
		domain.StateContacts,
		domain.StateContactPage,
		domain.StateContactBarter,
		domain.StateBarterCard,
		domain.StateInventory,
		domain.StateSaleDialog,
		domain.StateConfirmation,
		domain.StateErrorDialog:
		return true
	default:
		return false
	}
}

func validPoint(point domain.Point) bool {
	return point.X >= 0 && point.X <= 1 && point.Y >= 0 && point.Y <= 1
}

func validRectangle(rectangle domain.Rectangle) bool {
	return rectangle.X >= 0 &&
		rectangle.Y >= 0 &&
		rectangle.Width > 0 &&
		rectangle.Height > 0 &&
		rectangle.X+rectangle.Width <= 1 &&
		rectangle.Y+rectangle.Height <= 1
}

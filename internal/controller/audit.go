package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/arena-trading-agent/arena-trading-agent/internal/domain"
	"github.com/arena-trading-agent/arena-trading-agent/internal/protocol"
)

func (s *Server) persistActionRequest(
	ctx context.Context,
	agentID string,
	request protocol.ActionRequest,
) error {
	const methodCtx = "controller.Server.persistActionRequest"

	if s.auditStore == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	actionPayload, err := json.Marshal(request.Action)
	if err != nil {
		return fmt.Errorf(
			"%s: %w: не удалось сериализовать данные ACTION_REQUEST %q: %v",
			methodCtx,
			ErrAuditPersistence,
			request.ID,
			err,
		)
	}
	frameBasis := request.FrameBasis
	if frameBasis == nil {
		frameBasis = []protocol.FrameRegionDigest{}
	}
	frameBasisPayload, err := json.Marshal(frameBasis)
	if err != nil {
		return fmt.Errorf(
			"%s: %w: не удалось сериализовать ROI-основание ACTION_REQUEST %q: %v",
			methodCtx,
			ErrAuditPersistence,
			request.ID,
			err,
		)
	}
	record := domain.ActionRecord{
		ID:                 request.ID,
		SessionID:          request.SessionID,
		AgentID:            agentID,
		BasedOnFrame:       request.BasedOnFrame,
		BasedOnCapturedAt:  cloneTime(request.BasedOnCapturedAt),
		BasedOnFrameDigest: request.BasedOnFrameDigest,
		FrameBasisPayload:  frameBasisPayload,
		BasedOnState:       request.BasedOnState,
		ExpectedState:      request.ExpectedState,
		MinConfidence:      request.MinVerificationConfidence,
		ExpectedWidth:      request.ExpectedWidth,
		ExpectedHeight:     request.ExpectedHeight,
		ExpectedDPIPercent: request.ExpectedDPIPercent,
		Deadline:           request.Deadline,
		Class:              string(request.Class),
		Kind:               request.Action.Kind,
		Point:              clonePoint(request.Action.Point),
		Value:              request.Action.Value,
		Delta:              request.Action.Delta,
		ActionPayload:      actionPayload,
		RequestedAt:        time.Now().UTC(),
	}
	if err := s.auditStore.SaveAction(ctx, record); err != nil {
		return fmt.Errorf(
			"%s: %w: не удалось сохранить ACTION_REQUEST %q: %v",
			methodCtx,
			ErrAuditPersistence,
			request.ID,
			err,
		)
	}
	return nil
}

func (s *Server) persistActionResult(
	agentID, correlationID string,
	envelope protocol.Envelope,
	result protocol.ActionResult,
) error {
	const methodCtx = "controller.Server.persistActionResult"

	if s.auditStore == nil {
		return nil
	}
	actionID := result.ID
	if actionID == "" {
		actionID = correlationID
	}
	record := domain.ActionResultRecord{
		ActionID:               actionID,
		MessageID:              envelope.MessageID,
		CorrelationID:          correlationID,
		AgentID:                agentID,
		Success:                result.Success,
		RetrySafe:              result.RetrySafe,
		ResultFrame:            result.ResultFrame,
		ResultState:            result.ResultState,
		VerificationConfidence: result.VerificationConfidence,
		Error:                  result.Error,
		CompletedAt:            result.CompletedAt,
		ReceivedAt:             time.Now().UTC(),
	}
	ctx, cancel := context.WithTimeout(context.Background(), s.auditTimeout)
	defer cancel()
	if err := s.auditStore.SaveActionResult(ctx, record); err != nil {
		return fmt.Errorf(
			"%s: %w: не удалось сохранить ACTION_RESULT действия %q, сообщение %q: %v",
			methodCtx,
			ErrAuditPersistence,
			actionID,
			envelope.MessageID,
			err,
		)
	}
	return nil
}

func (s *Server) persistUnsentActionResult(
	agentID string,
	request protocol.ActionRequest,
	cause error,
) error {
	const methodCtx = "controller.Server.persistUnsentActionResult"

	if s.auditStore == nil {
		return nil
	}
	now := time.Now().UTC()
	record := domain.ActionResultRecord{
		ActionID:      request.ID,
		MessageID:     request.ID + "-not-sent",
		CorrelationID: request.ID,
		AgentID:       agentID,
		Success:       false,
		NotSent:       true,
		RetrySafe:     true,
		ResultFrame:   request.BasedOnFrame,
		ResultState:   request.BasedOnState,
		Error: fmt.Sprintf(
			"%s: действие гарантированно не отправлено: %v",
			methodCtx,
			cause,
		),
		CompletedAt: now,
		ReceivedAt:  now,
	}
	ctx, cancel := context.WithTimeout(context.Background(), s.auditTimeout)
	defer cancel()
	if err := s.auditStore.SaveActionResult(ctx, record); err != nil {
		return fmt.Errorf(
			"%s: %w: не удалось сохранить отказ действия %q: %v",
			methodCtx,
			ErrAuditPersistence,
			request.ID,
			err,
		)
	}
	return nil
}

func (s *Server) persistAgentEvent(
	agentID string,
	envelope protocol.Envelope,
	event protocol.AgentEvent,
) error {
	return s.persistEventRecord(domain.AgentEventRecord{
		ID:        s.auditEventID(envelope.MessageID),
		SessionID: event.SessionID,
		AgentID:   agentID,
		Kind:      event.Kind,
		Severity:  event.Severity,
		Message:   event.Message,
		Payload:   append(json.RawMessage(nil), envelope.Payload...),
		CreatedAt: event.At,
	})
}

func (s *Server) persistEmergencyStop(
	agentID string,
	envelope protocol.Envelope,
	stop protocol.EmergencyStop,
) error {
	return s.persistEventRecord(domain.AgentEventRecord{
		ID:        s.auditEventID(envelope.MessageID),
		AgentID:   agentID,
		Kind:      string(protocol.MessageEmergencyStop),
		Severity:  "critical",
		Message:   stop.Reason,
		Payload:   append(json.RawMessage(nil), envelope.Payload...),
		CreatedAt: stop.At,
	})
}

func (s *Server) persistEventRecord(record domain.AgentEventRecord) error {
	const methodCtx = "controller.Server.persistEventRecord"

	if s.auditStore == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), s.auditTimeout)
	defer cancel()
	if err := s.auditStore.SaveEvent(ctx, record); err != nil {
		return fmt.Errorf(
			"%s: %w: не удалось сохранить событие %q агента %q типа %q: %v",
			methodCtx,
			ErrAuditPersistence,
			record.ID,
			record.AgentID,
			record.Kind,
			err,
		)
	}
	return nil
}

func (s *Server) auditEventID(messageID string) string {
	if messageID != "" {
		return messageID
	}
	return s.nextID("agent-event")
}

func clonePoint(point *domain.Point) *domain.Point {
	if point == nil {
		return nil
	}
	value := *point
	return &value
}

func cloneTime(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	cloned := value.Round(0).UTC()
	return &cloned
}

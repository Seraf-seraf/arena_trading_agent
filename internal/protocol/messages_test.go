package protocol_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/arena-trading-agent/arena-trading-agent/internal/domain"
	"github.com/arena-trading-agent/arena-trading-agent/internal/protocol"
)

func TestNewEnvelope(t *testing.T) {
	t.Parallel()

	envelope, err := protocol.NewEnvelope(protocol.MessageHello, "message-1", protocol.Hello{AgentID: "agent-1"})
	if err != nil {
		t.Fatalf("NewEnvelope() вернул ошибку: %v", err)
	}
	if envelope.Type != protocol.MessageHello {
		t.Fatalf("Type = %q, ожидался %q", envelope.Type, protocol.MessageHello)
	}

	var hello protocol.Hello
	if err := json.Unmarshal(envelope.Payload, &hello); err != nil {
		t.Fatalf("не удалось прочитать Payload: %v", err)
	}
	if hello.AgentID != "agent-1" {
		t.Errorf("AgentID = %q, ожидался agent-1", hello.AgentID)
	}
}

func TestNewEnvelopeRejectsUnsupportedPayload(t *testing.T) {
	t.Parallel()

	_, err := protocol.NewEnvelope(protocol.MessageAgentEvent, "message-1", make(chan int))
	if err == nil {
		t.Fatal("NewEnvelope() должен отклонить несериализуемую нагрузку")
	}
}

func TestNewCorrelatedEnvelope(t *testing.T) {
	t.Parallel()

	envelope, err := protocol.NewCorrelatedEnvelope(
		protocol.MessageWindowStatus,
		"response-1",
		"request-1",
		protocol.WindowStatus{Active: true},
	)
	if err != nil {
		t.Fatal(err)
	}
	if envelope.MessageID != "response-1" || envelope.CorrelationID != "request-1" {
		t.Fatalf("неверная корреляция: %+v", envelope)
	}
}

func TestFrameEnvelopeAddsAndValidatesContentDigest(t *testing.T) {
	t.Parallel()

	data := []byte("закодированный кадр")
	envelope, err := protocol.NewEnvelope(protocol.MessageFrame, "frame-1", protocol.Frame{
		ID:         1,
		CapturedAt: time.Now().UTC(),
		Encoding:   "png",
		Data:       data,
	})
	if err != nil {
		t.Fatal(err)
	}
	var frame protocol.Frame
	if err := json.Unmarshal(envelope.Payload, &frame); err != nil {
		t.Fatal(err)
	}
	if frame.ContentDigest != protocol.ComputeFrameDigest(data) ||
		!protocol.ValidFrameDigest(frame.ContentDigest) {
		t.Fatalf("получен некорректный digest: %q", frame.ContentDigest)
	}

	frame.ContentDigest = protocol.ComputeFrameDigest([]byte("другой кадр"))
	_, err = protocol.NewEnvelope(protocol.MessageFrame, "frame-2", frame)
	if err == nil || !strings.Contains(err.Error(), "digest") {
		t.Fatalf("подменённый digest не отклонён: %v", err)
	}
}

func TestFrameEnvelopeRejectsOversizedFrameBeforeHashing(t *testing.T) {
	data := make([]byte, protocol.MaxFrameDataBytes+1)
	_, err := protocol.NewEnvelope(
		protocol.MessageFrame,
		"oversized-frame",
		protocol.Frame{Data: data},
	)
	if err == nil || !strings.Contains(err.Error(), "превышает лимит") {
		t.Fatalf("кадр сверх лимита не отклонён: %v", err)
	}
}

func TestLegacyActionRequestJSONRemainsReadable(t *testing.T) {
	t.Parallel()

	var request protocol.ActionRequest
	err := json.Unmarshal([]byte(`{
		"id":"legacy-action",
		"session_id":"legacy-session",
		"based_on_frame":7,
		"expected_state":"MAIN_MENU",
		"class":"NAVIGATION",
		"deadline":"2026-07-30T12:00:00Z",
		"action":{"kind":"KEY","value":"ENTER"}
	}`), &request)
	if err != nil {
		t.Fatal(err)
	}
	if request.BasedOnCapturedAt != nil || request.BasedOnFrameDigest != "" ||
		request.BasedOnState != "" || request.MinVerificationConfidence != 0 ||
		request.ExpectedState != domain.StateMainMenu {
		t.Fatalf("legacy JSON декодирован несовместимо: %+v", request)
	}
}

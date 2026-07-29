package protocol_test

import (
	"encoding/json"
	"testing"

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

// Package protocol определяет сообщения WebSocket между процессами.
package protocol

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/arena-trading-agent/arena-trading-agent/internal/domain"
)

// MessageType определяет назначение сообщения транспорта.
type MessageType string

const (
	MessageHello         MessageType = "HELLO"
	MessageHeartbeat     MessageType = "HEARTBEAT"
	MessageWindowStatus  MessageType = "WINDOW_STATUS"
	MessageFrame         MessageType = "FRAME"
	MessageFrameRegion   MessageType = "FRAME_REGION"
	MessageActionRequest MessageType = "ACTION_REQUEST"
	MessageActionResult  MessageType = "ACTION_RESULT"
	MessageAgentEvent    MessageType = "AGENT_EVENT"
	MessageEmergencyStop MessageType = "EMERGENCY_STOP"
)

// Envelope отделяет маршрутизацию сообщения от его полезной нагрузки.
type Envelope struct {
	Type      MessageType     `json:"type"`
	MessageID string          `json:"message_id"`
	SentAt    time.Time       `json:"sent_at"`
	Payload   json.RawMessage `json:"payload,omitempty"`
}

// Hello сообщает контроллеру идентичность и возможности агента.
type Hello struct {
	AgentID  string   `json:"agent_id"`
	Version  string   `json:"version"`
	Features []string `json:"features"`
}

// Heartbeat подтверждает работоспособность соединения.
type Heartbeat struct {
	AgentID string    `json:"agent_id"`
	At      time.Time `json:"at"`
}

// Action описывает атомарное действие Windows Agent.
type Action struct {
	Kind  string        `json:"kind"`
	Point *domain.Point `json:"point,omitempty"`
	Value string        `json:"value,omitempty"`
}

// ActionRequest содержит условия безопасного исполнения действия.
type ActionRequest struct {
	ID            string             `json:"id"`
	SessionID     string             `json:"session_id"`
	BasedOnFrame  uint64             `json:"based_on_frame"`
	ExpectedState domain.ScreenState `json:"expected_state"`
	Deadline      time.Time          `json:"deadline"`
	Action        Action             `json:"action"`
}

// ActionResult фиксирует исход команды и кадр, на котором он проверен.
type ActionResult struct {
	ID          string             `json:"id"`
	Success     bool               `json:"success"`
	ResultFrame uint64             `json:"result_frame"`
	ResultState domain.ScreenState `json:"result_state"`
	Error       string             `json:"error,omitempty"`
	CompletedAt time.Time          `json:"completed_at"`
}

// WindowStatus сообщает условия, обязательные для безопасного ввода.
type WindowStatus struct {
	Active     bool `json:"active"`
	Minimized  bool `json:"minimized"`
	Width      int  `json:"width"`
	Height     int  `json:"height"`
	DPIPercent int  `json:"dpi_percent"`
}

// Frame содержит метаданные и закодированное изображение кадра.
type Frame struct {
	ID         uint64           `json:"id"`
	CapturedAt time.Time        `json:"captured_at"`
	Region     domain.Rectangle `json:"region"`
	Encoding   string           `json:"encoding"`
	Data       []byte           `json:"data"`
}

// NewEnvelope сериализует типизированную нагрузку в транспортный конверт.
func NewEnvelope(messageType MessageType, messageID string, payload any) (Envelope, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return Envelope{}, fmt.Errorf("не удалось сериализовать сообщение %s: %w", messageType, err)
	}
	return Envelope{Type: messageType, MessageID: messageID, SentAt: time.Now().UTC(), Payload: raw}, nil
}

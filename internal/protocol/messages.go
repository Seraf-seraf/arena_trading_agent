// Package protocol определяет сообщения WebSocket между процессами.
package protocol

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/arena-trading-agent/arena-trading-agent/internal/domain"
)

// MessageType определяет назначение сообщения транспорта.
type MessageType string

const (
	MessageHello               MessageType = "HELLO"
	MessageHeartbeat           MessageType = "HEARTBEAT"
	MessageWindowStatusRequest MessageType = "WINDOW_STATUS_REQUEST"
	MessageWindowStatus        MessageType = "WINDOW_STATUS"
	MessageFrameRequest        MessageType = "FRAME_REQUEST"
	MessageFrameRegionRequest  MessageType = "FRAME_REGION_REQUEST"
	MessageFrame               MessageType = "FRAME"
	MessageFrameRegion         MessageType = "FRAME_REGION"
	MessageActionRequest       MessageType = "ACTION_REQUEST"
	MessageActionResult        MessageType = "ACTION_RESULT"
	MessageAgentEvent          MessageType = "AGENT_EVENT"
	MessageEmergencyStop       MessageType = "EMERGENCY_STOP"
)

const (
	// MaxFrameDataBytes ограничивает уже декодированный PNG до вычисления
	// digest и повторной JSON-сериализации.
	MaxFrameDataBytes = 24 << 20
	// MaxTransportMessageBytes включает base64 expansion 24 MiB кадра и
	// небольшой запас на метаданные envelope.
	MaxTransportMessageBytes = 40 << 20
	// MaxFrameBasisRegions ограничивает число ROI, которые Windows Agent
	// обязан повторно хешировать непосредственно перед вводом.
	MaxFrameBasisRegions = 32
)

// Envelope отделяет маршрутизацию сообщения от его полезной нагрузки.
type Envelope struct {
	Type          MessageType     `json:"type"`
	MessageID     string          `json:"message_id"`
	CorrelationID string          `json:"correlation_id,omitempty"`
	SentAt        time.Time       `json:"sent_at"`
	Payload       json.RawMessage `json:"payload,omitempty"`
}

// Hello сообщает контроллеру идентичность и возможности агента.
type Hello struct {
	AgentID          string   `json:"agent_id"`
	Version          string   `json:"version"`
	Features         []string `json:"features"`
	AutomationPaused bool     `json:"automation_paused,omitempty"`
	EmergencyStopped bool     `json:"emergency_stopped,omitempty"`
	SafetyReason     string   `json:"safety_reason,omitempty"`
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
	Delta int           `json:"delta,omitempty"`
	Steps []Action      `json:"steps,omitempty"`
}

// ActionClass — семантическая метка безопасности, которую назначает контроллер,
// но никогда не модель зрения. SCAN принимает только NAVIGATION; классы,
// изменяющие инвентарь или деньги, требуют режима TRADE.
type ActionClass string

const (
	ActionNavigation ActionClass = "NAVIGATION"
	ActionPurchase   ActionClass = "PURCHASE"
	ActionBarter     ActionClass = "BARTER"
	ActionListing    ActionClass = "LISTING"
	ActionReprice    ActionClass = "REPRICE"
)

// ActionRequest содержит условия безопасного исполнения действия.
type ActionRequest struct {
	ID                        string              `json:"id"`
	SessionID                 string              `json:"session_id"`
	BasedOnFrame              uint64              `json:"based_on_frame"`
	BasedOnCapturedAt         *time.Time          `json:"based_on_captured_at,omitempty"`
	BasedOnFrameDigest        string              `json:"based_on_frame_digest,omitempty"`
	FrameBasis                []FrameRegionDigest `json:"frame_basis,omitempty"`
	BasedOnState              domain.ScreenState  `json:"based_on_state,omitempty"`
	ExpectedState             domain.ScreenState  `json:"expected_state"`
	MinVerificationConfidence float64             `json:"min_verification_confidence,omitempty"`
	ExpectedWidth             int                 `json:"expected_width,omitempty"`
	ExpectedHeight            int                 `json:"expected_height,omitempty"`
	ExpectedDPIPercent        int                 `json:"expected_dpi_percent,omitempty"`
	Class                     ActionClass         `json:"class"`
	Deadline                  time.Time           `json:"deadline"`
	Action                    Action              `json:"action"`
}

// ActionResult фиксирует исход команды и кадр, на котором он проверен.
type ActionResult struct {
	ID                     string             `json:"id"`
	Success                bool               `json:"success"`
	RetrySafe              bool               `json:"retry_safe,omitempty"`
	ResultFrame            uint64             `json:"result_frame"`
	ResultState            domain.ScreenState `json:"result_state"`
	VerificationConfidence float64            `json:"verification_confidence,omitempty"`
	Frame                  *Frame             `json:"frame,omitempty"`
	Error                  string             `json:"error,omitempty"`
	CompletedAt            time.Time          `json:"completed_at"`
}

// WindowStatus сообщает условия, обязательные для безопасного ввода.
type WindowStatus struct {
	Active     bool `json:"active"`
	Minimized  bool `json:"minimized"`
	Width      int  `json:"width"`
	Height     int  `json:"height"`
	DPIPercent int  `json:"dpi_percent"`
}

// WindowStatusRequest запрашивает немедленную проверку окна игры.
type WindowStatusRequest struct{}

// FrameRequest задаёт параметры захвата свежего кадра.
//
// AfterFrame позволяет контроллеру указать последний уже обработанный кадр.
// Агент всё равно возвращает свежий кадр и не переиспользует AfterFrame.
type FrameRequest struct {
	AfterFrame uint64            `json:"after_frame,omitempty"`
	Region     *domain.Rectangle `json:"region,omitempty"`
}

// Frame содержит метаданные и закодированное изображение кадра.
type Frame struct {
	ID            uint64           `json:"id"`
	CapturedAt    time.Time        `json:"captured_at"`
	ContentDigest string           `json:"content_digest,omitempty"`
	Region        domain.Rectangle `json:"region"`
	Encoding      string           `json:"encoding"`
	Data          []byte           `json:"data"`
}

// ComputeFrameDigest возвращает компактный URL-safe SHA-256 идентификатор
// именно закодированных байтов изображения. Функция не копирует Data.
func ComputeFrameDigest(data []byte) string {
	digest := sha256.Sum256(data)
	return base64.RawURLEncoding.EncodeToString(digest[:])
}

// ValidFrameDigest проверяет канонический формат идентификатора кадра.
func ValidFrameDigest(value string) bool {
	if value == "" || strings.TrimSpace(value) != value {
		return false
	}
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size
}

// NormalizeFrameDigest вычисляет отсутствующий digest и отклоняет подмену
// заявленного digest без дополнительной копии тяжёлого изображения.
func NormalizeFrameDigest(frame Frame) (Frame, error) {
	const methodCtx = "protocol.NormalizeFrameDigest"

	if len(frame.Data) == 0 {
		if frame.ContentDigest != "" {
			return Frame{}, fmt.Errorf("%s: пустой кадр содержит digest", methodCtx)
		}
		return frame, nil
	}
	if len(frame.Data) > MaxFrameDataBytes {
		return Frame{}, fmt.Errorf(
			"%s: размер кадра %d превышает лимит %d байт",
			methodCtx,
			len(frame.Data),
			MaxFrameDataBytes,
		)
	}
	expected := ComputeFrameDigest(frame.Data)
	if frame.ContentDigest != "" && frame.ContentDigest != expected {
		return Frame{}, fmt.Errorf("%s: digest кадра не соответствует содержимому", methodCtx)
	}
	frame.ContentDigest = expected
	return frame, nil
}

// AgentEvent сообщает контроллеру о неденежном событии локального runtime.
type AgentEvent struct {
	SessionID string            `json:"session_id,omitempty"`
	Kind      string            `json:"kind"`
	Severity  string            `json:"severity,omitempty"`
	Message   string            `json:"message,omitempty"`
	FrameID   uint64            `json:"frame_id,omitempty"`
	At        time.Time         `json:"at"`
	Details   map[string]string `json:"details,omitempty"`
}

// EmergencyStop останавливает ввод и объясняет причину остановки.
type EmergencyStop struct {
	Reason        string    `json:"reason"`
	At            time.Time `json:"at"`
	UserInitiated bool      `json:"user_initiated,omitempty"`
}

// NewEnvelope сериализует типизированную нагрузку в транспортный конверт.
func NewEnvelope(messageType MessageType, messageID string, payload any) (Envelope, error) {
	const methodCtx = "protocol.NewEnvelope"

	envelope, err := NewCorrelatedEnvelope(messageType, messageID, "", payload)
	if err != nil {
		return Envelope{}, fmt.Errorf("%s: не удалось создать транспортный конверт: %w", methodCtx, err)
	}
	return envelope, nil
}

// NewCorrelatedEnvelope сериализует ответ, связанный с исходным сообщением.
func NewCorrelatedEnvelope(messageType MessageType, messageID, correlationID string, payload any) (Envelope, error) {
	const methodCtx = "protocol.NewCorrelatedEnvelope"

	normalizedPayload, err := normalizeFramePayload(payload)
	if err != nil {
		return Envelope{}, fmt.Errorf("%s: полезная нагрузка %s содержит некорректный кадр: %w", methodCtx, messageType, err)
	}
	raw, err := json.Marshal(normalizedPayload)
	if err != nil {
		return Envelope{}, fmt.Errorf("%s: не удалось сериализовать сообщение %s: %w", methodCtx, messageType, err)
	}
	return Envelope{
		Type:          messageType,
		MessageID:     messageID,
		CorrelationID: correlationID,
		SentAt:        time.Now().UTC(),
		Payload:       raw,
	}, nil
}

func normalizeFramePayload(payload any) (any, error) {
	const methodCtx = "protocol.normalizeFramePayload"

	switch value := payload.(type) {
	case Frame:
		frame, err := NormalizeFrameDigest(value)
		if err != nil {
			return nil, fmt.Errorf("%s: кадр: %w", methodCtx, err)
		}
		return frame, nil
	case *Frame:
		if value == nil {
			return value, nil
		}
		frame, err := NormalizeFrameDigest(*value)
		if err != nil {
			return nil, fmt.Errorf("%s: кадр: %w", methodCtx, err)
		}
		return &frame, nil
	case ActionResult:
		result := value
		if result.Frame == nil {
			return result, nil
		}
		frame, err := NormalizeFrameDigest(*result.Frame)
		if err != nil {
			return nil, fmt.Errorf("%s: контрольный кадр: %w", methodCtx, err)
		}
		result.Frame = &frame
		return result, nil
	case *ActionResult:
		if value == nil || value.Frame == nil {
			return value, nil
		}
		result := *value
		frame, err := NormalizeFrameDigest(*result.Frame)
		if err != nil {
			return nil, fmt.Errorf("%s: контрольный кадр: %w", methodCtx, err)
		}
		result.Frame = &frame
		return &result, nil
	default:
		return payload, nil
	}
}

package domain

import (
	"encoding/json"
	"time"
)

// ActionRecord is the durable representation of a command sent to the
// Windows Agent. RequestedAt records when the controller persisted the
// command, independently from the transport envelope timestamp.
type ActionRecord struct {
	ID                 string          `json:"id"`
	SessionID          string          `json:"session_id"`
	AgentID            string          `json:"agent_id,omitempty"`
	BasedOnFrame       uint64          `json:"based_on_frame"`
	BasedOnCapturedAt  *time.Time      `json:"based_on_captured_at,omitempty"`
	BasedOnFrameDigest string          `json:"based_on_frame_digest,omitempty"`
	FrameBasisPayload  json.RawMessage `json:"frame_basis_payload,omitempty"`
	BasedOnState       ScreenState     `json:"based_on_state,omitempty"`
	ExpectedState      ScreenState     `json:"expected_state"`
	MinConfidence      float64         `json:"min_verification_confidence,omitempty"`
	ExpectedWidth      int             `json:"expected_width,omitempty"`
	ExpectedHeight     int             `json:"expected_height,omitempty"`
	ExpectedDPIPercent int             `json:"expected_dpi_percent,omitempty"`
	Deadline           time.Time       `json:"deadline"`
	Class              string          `json:"class"`
	Kind               string          `json:"kind"`
	Point              *Point          `json:"point,omitempty"`
	Value              string          `json:"value,omitempty"`
	Delta              int             `json:"delta,omitempty"`
	ActionPayload      json.RawMessage `json:"action_payload,omitempty"`
	RequestedAt        time.Time       `json:"requested_at"`
}

// ActionResultRecord is an immutable outcome received for an action. More
// than one result can be retained for an action so duplicate or conflicting
// agent responses remain auditable.
type ActionResultRecord struct {
	ActionID               string      `json:"action_id"`
	MessageID              string      `json:"message_id,omitempty"`
	CorrelationID          string      `json:"correlation_id,omitempty"`
	AgentID                string      `json:"agent_id,omitempty"`
	Success                bool        `json:"success"`
	NotSent                bool        `json:"not_sent,omitempty"`
	RetrySafe              bool        `json:"retry_safe,omitempty"`
	ResultFrame            uint64      `json:"result_frame"`
	ResultState            ScreenState `json:"result_state"`
	VerificationConfidence float64     `json:"verification_confidence,omitempty"`
	Error                  string      `json:"error,omitempty"`
	CompletedAt            time.Time   `json:"completed_at"`
	ReceivedAt             time.Time   `json:"received_at"`
}

// AgentEventRecord stores operational and safety events without coupling the
// domain package to the WebSocket protocol package.
type AgentEventRecord struct {
	ID        string          `json:"id"`
	SessionID string          `json:"session_id,omitempty"`
	AgentID   string          `json:"agent_id,omitempty"`
	Kind      string          `json:"kind"`
	Severity  string          `json:"severity,omitempty"`
	Message   string          `json:"message,omitempty"`
	Payload   json.RawMessage `json:"payload,omitempty"`
	CreatedAt time.Time       `json:"created_at"`
}

// ObservationFilter selects observations. Results are returned newest first.
type ObservationFilter struct {
	State  ScreenState
	Since  time.Time
	Until  time.Time
	Limit  int
	Offset int
}

// QuoteFilter selects market quote history. Results are returned newest first.
type QuoteFilter struct {
	ItemID string
	Since  time.Time
	Until  time.Time
	Limit  int
	Offset int
}

// TradeQuoteFilter selects complete economic quotes (including fees and
// liquidity) newest first.
type TradeQuoteFilter struct {
	ItemID string
	Since  time.Time
	Until  time.Time
	Limit  int
	Offset int
}

// RuntimeRecord stores versioned JSON snapshots for modules whose rich domain
// state does not fit the small legacy tables (recipes, inventory, orders and
// trading checkpoints).
type RuntimeRecord struct {
	Key       string          `json:"key"`
	Kind      string          `json:"kind"`
	Payload   json.RawMessage `json:"payload"`
	UpdatedAt time.Time       `json:"updated_at"`
}

// RuntimeRecordFilter selects snapshots by exact kind and/or key prefix.
type RuntimeRecordFilter struct {
	Kind      string
	KeyPrefix string
	Limit     int
	Offset    int
}

// ExecutionFilter selects current execution snapshots. Results are returned
// by their update time, newest first.
type ExecutionFilter struct {
	OpportunityID string
	Status        TradeExecutionStatus
	Since         time.Time
	Until         time.Time
	Limit         int
	Offset        int
}

// ActionFilter selects persisted action requests. Results are returned newest
// first. PendingOnly excludes actions that already have a result.
type ActionFilter struct {
	SessionID   string
	AgentID     string
	Kind        string
	PendingOnly bool
	Since       time.Time
	Until       time.Time
	Limit       int
	Offset      int
}

// EventFilter selects operational events. Results are returned newest first.
type EventFilter struct {
	SessionID string
	AgentID   string
	Kind      string
	Severity  string
	Since     time.Time
	Until     time.Time
	Limit     int
	Offset    int
}

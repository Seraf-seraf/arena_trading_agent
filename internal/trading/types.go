// Package trading coordinates deterministic trade planning and saga state.
//
// It deliberately has no dependency on the Windows Agent or the controller
// transport. The package only produces declarative directives and consumes
// verified facts, so it cannot click the game UI by itself.
package trading

import (
	"time"

	"github.com/arena-trading-agent/arena-trading-agent/internal/domain"
	"github.com/arena-trading-agent/arena-trading-agent/internal/economy"
)

// Config controls deterministic opportunity ranking.
type Config struct {
	ScoreWeights    economy.ScoreWeights
	ProfitScale     float64
	MaxStepAttempts int
	Clock           func() time.Time
}

// Candidate is an opportunity which passed all mandatory risk checks.
type Candidate struct {
	Opportunity domain.TradeOpportunity `json:"opportunity"`
	Score       float64                 `json:"score"`
}

// Rejection explains why one discovered opportunity cannot be started.
type Rejection struct {
	OpportunityID string `json:"opportunity_id"`
	Reason        string `json:"reason"`
}

// Evaluation is stable: candidates are ordered by score and rejections by ID.
type Evaluation struct {
	Candidates []Candidate `json:"candidates"`
	Rejected   []Rejection `json:"rejected"`
}

// SagaStatus is the persisted state of one trade workflow.
type SagaStatus string

const (
	SagaRunning           SagaStatus = "RUNNING"
	SagaRecovering        SagaStatus = "RECOVERING"
	SagaWaitingCooldown   SagaStatus = "WAITING_COOLDOWN"
	SagaWaitingMarketSlot SagaStatus = "WAITING_MARKET_SLOT"
	SagaAwaitingSale      SagaStatus = "AWAITING_SALE"
	SagaCompleted         SagaStatus = "COMPLETED"
	SagaCompletedMismatch SagaStatus = "COMPLETED_MISMATCH"
	SagaCompensated       SagaStatus = "COMPENSATED"
	SagaHeld              SagaStatus = "HELD"
	SagaFailed            SagaStatus = "FAILED"
)

// FailureReason is a machine-readable failure classification supplied by the
// observer after a failed, verified UI transition.
type FailureReason string

const (
	FailureUnknown               FailureReason = "UNKNOWN"
	FailurePriceChanged          FailureReason = "PRICE_CHANGED"
	FailureItemUnavailable       FailureReason = "ITEM_UNAVAILABLE"
	FailureInsufficientFunds     FailureReason = "INSUFFICIENT_FUNDS"
	FailureBarterCooldown        FailureReason = "BARTER_COOLDOWN"
	FailureBarterUnavailable     FailureReason = "BARTER_UNAVAILABLE"
	FailureMarketSlotUnavailable FailureReason = "MARKET_SLOT_UNAVAILABLE"
	FailureTransient             FailureReason = "TRANSIENT"
)

// CompensationKind maps a partial failure to the recovery table from the v1
// architecture. These are plans, never direct UI actions.
type CompensationKind string

const (
	CompensationNone            CompensationKind = "NONE"
	CompensationSellOrWait      CompensationKind = "SELL_OR_WAIT"
	CompensationWaitForCooldown CompensationKind = "WAIT_FOR_COOLDOWN"
	CompensationHoldResult      CompensationKind = "HOLD_RESULT"
	CompensationQueueForSlot    CompensationKind = "QUEUE_FOR_MARKET_SLOT"
)

// Holding attributes verified inventory changes to this saga.
type Holding struct {
	ItemID   string `json:"item_id"`
	Quantity int64  `json:"quantity"`
}

// Compensation is the deterministic recovery plan for a failed step.
type Compensation struct {
	Kind       CompensationKind `json:"kind"`
	FailedStep int              `json:"failed_step"`
	Reason     FailureReason    `json:"reason"`
	Message    string           `json:"message,omitempty"`
	Holdings   []Holding        `json:"holdings,omitempty"`
}

// Saga is a defensive snapshot of an in-memory workflow. Version increments
// exactly once per accepted, non-duplicate event.
type Saga struct {
	ID             string                       `json:"id"`
	Opportunity    domain.TradeOpportunity      `json:"opportunity"`
	Status         SagaStatus                   `json:"status"`
	CurrentStep    int                          `json:"current_step"`
	StepProgress   int64                        `json:"step_progress"`
	Attempt        int                          `json:"attempt"`
	ReservedBudget int64                        `json:"reserved_budget"`
	Actual         domain.TradeFinancials       `json:"actual"`
	MarketOrderID  string                       `json:"market_order_id,omitempty"`
	Holdings       []Holding                    `json:"holdings,omitempty"`
	Compensation   *Compensation                `json:"compensation,omitempty"`
	Reconciliation *domain.ReconciliationReport `json:"reconciliation,omitempty"`
	Failure        string                       `json:"failure,omitempty"`
	Version        uint64                       `json:"version"`
	StartedAt      time.Time                    `json:"started_at"`
	UpdatedAt      time.Time                    `json:"updated_at"`
}

// DirectiveKind describes what an external executor may do next.
type DirectiveKind string

const (
	DirectiveExecuteStep DirectiveKind = "EXECUTE_STEP"
	DirectiveRecover     DirectiveKind = "RECOVER"
	DirectiveMonitorSale DirectiveKind = "MONITOR_SALE"
	DirectiveDone        DirectiveKind = "DONE"
)

// Directive is declarative. IdempotencyKey stays stable while the same step
// attempt is outstanding and changes only after a verified transition.
type Directive struct {
	Kind           DirectiveKind     `json:"kind"`
	SagaID         string            `json:"saga_id"`
	IdempotencyKey string            `json:"idempotency_key"`
	StepIndex      int               `json:"step_index,omitempty"`
	Step           *domain.TradeStep `json:"step,omitempty"`
	Compensation   *Compensation     `json:"compensation,omitempty"`
}

// EventKind is the closed set of verified facts accepted by Apply.
type EventKind string

const (
	EventStepSucceeded    EventKind = "STEP_SUCCEEDED"
	EventStepFailed       EventKind = "STEP_FAILED"
	EventRecoveryResolved EventKind = "RECOVERY_RESOLVED"
	EventSaleSettled      EventKind = "SALE_SETTLED"
)

// RecoveryResolution records a policy decision after a compensation plan was
// presented. COMPENSATED requires verified deltas which dispose all holdings.
type RecoveryResolution string

const (
	RecoveryRetry       RecoveryResolution = "RETRY"
	RecoveryHold        RecoveryResolution = "HOLD"
	RecoveryCompensated RecoveryResolution = "COMPENSATED"
)

// StepOutcome contains only facts independently verified after an action.
// Inventory slot changes are explicit because stack rules must not be guessed.
type StepOutcome struct {
	CompletedQuantity int64                   `json:"completed_quantity"`
	PurchaseCost      int64                   `json:"purchase_cost"`
	Revenue           int64                   `json:"revenue"`
	Fees              int64                   `json:"fees"`
	MonetaryActionID  string                  `json:"monetary_action_id,omitempty"`
	MarketOrderID     string                  `json:"market_order_id,omitempty"`
	InventoryDeltas   []domain.InventoryDelta `json:"inventory_deltas,omitempty"`
}

// Event is idempotent by ID. Reusing an ID with different contents is rejected.
type Event struct {
	ID         string              `json:"id"`
	SagaID     string              `json:"saga_id"`
	Kind       EventKind           `json:"kind"`
	StepIndex  int                 `json:"step_index,omitempty"`
	Outcome    StepOutcome         `json:"outcome"`
	Reason     FailureReason       `json:"reason,omitempty"`
	Message    string              `json:"message,omitempty"`
	Resolution RecoveryResolution  `json:"resolution,omitempty"`
	Settlement *domain.TradeResult `json:"settlement,omitempty"`
	OccurredAt time.Time           `json:"occurred_at"`
}

// ProcessedEventCheckpoint preserves the exact idempotent reply associated
// with an accepted event. A duplicate event after process restart therefore
// receives the same saga snapshot as it did before the restart.
type ProcessedEventCheckpoint struct {
	Event    Event `json:"event"`
	Snapshot Saga  `json:"snapshot"`
}

// Checkpoint is the complete durable state required to resume one saga.
// Inventory is included because occupied slots cannot safely be reconstructed
// from item quantities.
type Checkpoint struct {
	SchemaVersion int                        `json:"schema_version"`
	Saga          Saga                       `json:"saga"`
	Limits        domain.RiskLimits          `json:"limits"`
	Inventory     domain.InventorySnapshot   `json:"inventory"`
	Processed     []ProcessedEventCheckpoint `json:"processed,omitempty"`
}

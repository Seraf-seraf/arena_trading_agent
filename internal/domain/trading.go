package domain

import "time"

// TradeStep kinds are stable values used by planners, the risk manager and
// action runners. LimitPrice on BUY is a per-unit ceiling; on LIST it is the
// minimum per-unit sale price.
const (
	TradeStepBuy    = "BUY"
	TradeStepBarter = "BARTER"
	TradeStepList   = "LIST"
)

// TradeQuote contains the complete, immutable market input required to plan a
// trade. Monetary fields are per item, except ListingFee which is charged once
// for the resulting listing.
type TradeQuote struct {
	ItemID          string    `json:"item_id"`
	PurchasePrice   int64     `json:"purchase_price"`
	SalePrice       int64     `json:"sale_price"`
	SaleCommission  int64     `json:"sale_commission"`
	ListingFee      int64     `json:"listing_fee"`
	ObservedAt      time.Time `json:"observed_at"`
	Confidence      float64   `json:"confidence"`
	LiquidityScore  float64   `json:"liquidity_score"`
	PriceVolatility float64   `json:"price_volatility"`
	ResaleKnown     bool      `json:"resale_known"`
}

// InventoryItem is an aggregate of equal items occupying one or more game
// inventory slots. ReservedQuantity is maintained by Inventory Tracker and is
// never greater than Quantity.
type InventoryItem struct {
	ItemID           string `json:"item_id"`
	Quantity         int64  `json:"quantity"`
	ReservedQuantity int64  `json:"reserved_quantity"`
	Slots            int    `json:"slots"`
}

// InventorySnapshot is a deterministic point-in-time view. Items are sorted by
// ItemID by the tracker.
type InventorySnapshot struct {
	CapacitySlots int             `json:"capacity_slots"`
	UsedSlots     int             `json:"used_slots"`
	Revision      uint64          `json:"revision"`
	Items         []InventoryItem `json:"items"`
}

// InventoryDelta is an atomic signed change received after a verified action.
// A caller must explicitly describe both quantity and occupied slot changes.
type InventoryDelta struct {
	ItemID        string `json:"item_id"`
	QuantityDelta int64  `json:"quantity_delta"`
	SlotsDelta    int    `json:"slots_delta"`
}

// TradeFinancials describes actual or expected cash flow. Profit is always
// derived as Revenue - Fees - InputCost rather than accepted from an observer.
type TradeFinancials struct {
	InputCost int64 `json:"input_cost"`
	Revenue   int64 `json:"revenue"`
	Fees      int64 `json:"fees"`
	Profit    int64 `json:"profit"`
}

// TradeResult contains verified facts used to reconcile an execution.
type TradeResult struct {
	ExecutionID    string    `json:"execution_id"`
	MarketOrderID  string    `json:"market_order_id"`
	InputCost      int64     `json:"input_cost"`
	Revenue        int64     `json:"revenue"`
	Fees           int64     `json:"fees"`
	ResultItemID   string    `json:"result_item_id"`
	ResultQuantity int64     `json:"result_quantity"`
	CompletedAt    time.Time `json:"completed_at"`
}

// ReconciliationMismatch identifies one exact expected/actual discrepancy.
// Values are strings so identifiers and integer money can share one stable
// representation without float conversion.
type ReconciliationMismatch struct {
	Field    string `json:"field"`
	Expected string `json:"expected"`
	Actual   string `json:"actual"`
}

// ReconciliationReport is the immutable outcome of comparing a planned trade
// with verified game state.
type ReconciliationReport struct {
	OpportunityID       string                   `json:"opportunity_id"`
	ExecutionID         string                   `json:"execution_id"`
	Expected            TradeFinancials          `json:"expected"`
	Actual              TradeFinancials          `json:"actual"`
	InputCostVariance   int64                    `json:"input_cost_variance"`
	RevenueVariance     int64                    `json:"revenue_variance"`
	FeesVariance        int64                    `json:"fees_variance"`
	ProfitVariance      int64                    `json:"profit_variance"`
	ExpectedResultItem  string                   `json:"expected_result_item"`
	ActualResultItem    string                   `json:"actual_result_item"`
	ExpectedResultCount int64                    `json:"expected_result_count"`
	ActualResultCount   int64                    `json:"actual_result_count"`
	Matched             bool                     `json:"matched"`
	Mismatches          []ReconciliationMismatch `json:"mismatches"`
}

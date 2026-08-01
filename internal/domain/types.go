// Package domain содержит общие доменные типы controller и Windows Agent.
package domain

import "time"

// Point задаёт нормализованную координату относительно клиентской области.
type Point struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
}

// Rectangle задаёт нормализованную прямоугольную область кадра.
type Rectangle struct {
	X      float64 `json:"x"`
	Y      float64 `json:"y"`
	Width  float64 `json:"width"`
	Height float64 `json:"height"`
}

// ScreenState определяет известное состояние интерфейса игры.
type ScreenState string

const (
	StateUnknown        ScreenState = "UNKNOWN"
	StateMainMenu       ScreenState = "MAIN_MENU"
	StateMarketHome     ScreenState = "MARKET_HOME"
	StateMarketSearch   ScreenState = "MARKET_SEARCH"
	StateMarketResults  ScreenState = "MARKET_RESULTS"
	StateItemCard       ScreenState = "ITEM_CARD"
	StatePurchaseDialog ScreenState = "PURCHASE_DIALOG"
	StateContacts       ScreenState = "CONTACTS"
	StateContactPage    ScreenState = "CONTACT_PAGE"
	StateContactBarter  ScreenState = "CONTACT_BARTER"
	StateBarterCard     ScreenState = "BARTER_CARD"
	StateInventory      ScreenState = "INVENTORY"
	StateSaleDialog     ScreenState = "SALE_DIALOG"
	StateConfirmation   ScreenState = "CONFIRMATION"
	StateErrorDialog    ScreenState = "ERROR_DIALOG"
)

// Value хранит распознанное значение вместе с его происхождением.
type Value struct {
	Raw        string    `json:"raw"`
	Normalized string    `json:"normalized"`
	Source     string    `json:"source"`
	Confidence float64   `json:"confidence"`
	Region     Rectangle `json:"region"`
}

// UIElement описывает распознанный элемент интерфейса.
type UIElement struct {
	Kind             string    `json:"kind"`
	Label            string    `json:"label"`
	Region           Rectangle `json:"region"`
	Confidence       float64   `json:"confidence"`
	GeometryAdjusted bool      `json:"geometry_adjusted,omitempty"`
}

// Observation представляет нормализованный результат анализа кадра.
type Observation struct {
	FrameID    uint64           `json:"frame_id"`
	State      ScreenState      `json:"state"`
	Elements   []UIElement      `json:"elements"`
	Values     map[string]Value `json:"values"`
	Confidence float64          `json:"confidence"`
	CreatedAt  time.Time        `json:"created_at"`
}

// AgentMode задаёт допустимый уровень автономности агента.
type AgentMode string

const (
	ModeObserve  AgentMode = "OBSERVE"
	ModeScan     AgentMode = "SCAN"
	ModeSimulate AgentMode = "SIMULATE"
	ModeTrade    AgentMode = "TRADE"
	ModePaused   AgentMode = "PAUSED"
)

// OpportunityKind задаёт способ получения торговой прибыли.
type OpportunityKind string

const (
	OpportunityDirectFlip     OpportunityKind = "DIRECT_FLIP"
	OpportunityContactBarter  OpportunityKind = "CONTACT_BARTER"
	OpportunityMultistepTrade OpportunityKind = "MULTISTEP_BARTER"
)

// Item представляет предмет с устойчивым идентификатором.
type Item struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// MarketQuote хранит наблюдаемую рыночную цену.
type MarketQuote struct {
	ItemID     string    `json:"item_id"`
	BuyPrice   int64     `json:"buy_price"`
	SalePrice  int64     `json:"sale_price"`
	ObservedAt time.Time `json:"observed_at"`
	Confidence float64   `json:"confidence"`
}

// BarterIngredient задаёт количество предмета в рецепте.
type BarterIngredient struct {
	ItemID   string `json:"item_id"`
	Quantity int64  `json:"quantity"`
}

// BarterRecipe описывает обмен у контакта.
type BarterRecipe struct {
	ID          string             `json:"id"`
	ContactID   string             `json:"contact_id"`
	Ingredients []BarterIngredient `json:"ingredients"`
	ResultItem  string             `json:"result_item"`
	ResultCount int64              `json:"result_count"`
	AvailableAt time.Time          `json:"available_at"`
}

// TradeStep описывает один проверяемый шаг торгового плана.
type TradeStep struct {
	Kind       string `json:"kind"`
	ItemID     string `json:"item_id,omitempty"`
	RecipeID   string `json:"recipe_id,omitempty"`
	Quantity   int64  `json:"quantity,omitempty"`
	LimitPrice int64  `json:"limit_price,omitempty"`
}

// TradeOpportunity содержит расчёт и план потенциальной сделки.
type TradeOpportunity struct {
	ID              string          `json:"id"`
	Kind            OpportunityKind `json:"kind"`
	InputCost       int64           `json:"input_cost"`
	ExpectedRevenue int64           `json:"expected_revenue"`
	ExpectedFees    int64           `json:"expected_fees"`
	ExpectedProfit  int64           `json:"expected_profit"`
	ROI             float64         `json:"roi"`
	Confidence      float64         `json:"confidence"`
	LiquidityScore  float64         `json:"liquidity_score"`
	PriceVolatility float64         `json:"price_volatility"`
	RequiredSlots   int             `json:"required_slots"`
	QuoteObservedAt time.Time       `json:"quote_observed_at"`
	ResaleKnown     bool            `json:"resale_known"`
	ResultItemID    string          `json:"result_item_id,omitempty"`
	ResultQuantity  int64           `json:"result_quantity,omitempty"`
	ExpiresAt       time.Time       `json:"expires_at"`
	Steps           []TradeStep     `json:"steps"`
}

// RiskLimits задаёт обязательные ограничения торговой сессии.
type RiskLimits struct {
	MaxBudget          int64         `json:"max_budget"`
	MaxItemPrice       int64         `json:"max_item_price"`
	MinProfit          int64         `json:"min_profit"`
	MinROI             float64       `json:"min_roi"`
	MinConfidence      float64       `json:"min_confidence"`
	MaxQuoteAge        time.Duration `json:"max_quote_age"`
	AvailableSlots     int           `json:"available_slots"`
	AllowUnknownResale bool          `json:"allow_unknown_resale"`
}

// TradeExecutionStatus отражает состояние транзакции или её восстановления.
type TradeExecutionStatus string

const (
	TradePending           TradeExecutionStatus = "PENDING"
	TradeRunning           TradeExecutionStatus = "RUNNING"
	TradeRecovering        TradeExecutionStatus = "RECOVERING"
	TradeCompleted         TradeExecutionStatus = "COMPLETED"
	TradeCompletedMismatch TradeExecutionStatus = "COMPLETED_MISMATCH"
	TradeCompensated       TradeExecutionStatus = "COMPENSATED"
	TradeHeld              TradeExecutionStatus = "HELD"
	TradeFailed            TradeExecutionStatus = "FAILED"
)

// TradeExecution хранит журнал исполняемой торговой saga.
type TradeExecution struct {
	ID            string               `json:"id"`
	OpportunityID string               `json:"opportunity_id"`
	Status        TradeExecutionStatus `json:"status"`
	CurrentStep   int                  `json:"current_step"`
	Reserved      int64                `json:"reserved"`
	StartedAt     time.Time            `json:"started_at"`
	UpdatedAt     time.Time            `json:"updated_at"`
	Failure       string               `json:"failure,omitempty"`
}

// Package config loads and validates the production runtime configuration.
//
// Detector calibration intentionally has its own format in internal/detection.
// Runtime only keeps the path to that file so UI fingerprints and business
// policy cannot accidentally be changed as one configuration object.
package config

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/arena-trading-agent/arena-trading-agent/internal/domain"
	"github.com/arena-trading-agent/arena-trading-agent/internal/economy"
	"github.com/arena-trading-agent/arena-trading-agent/internal/protocol"
)

const (
	// CurrentVersion is the only runtime configuration schema understood by
	// this version of the controller.
	CurrentVersion = 1

	// MaxConfigBytes keeps a malformed or accidental input from consuming
	// unbounded memory before JSON validation starts.
	MaxConfigBytes int64 = 4 << 20

	// MaxMoney is deliberately below int64 limits so downstream addition and
	// fee calculations retain ample overflow headroom.
	MaxMoney int64 = 1_000_000_000_000
)

const (
	ActionMove   = "MOVE"
	ActionClick  = "CLICK"
	ActionScroll = "SCROLL"
	ActionKey    = "KEY"
	ActionText   = "TEXT"
)

// Duration is a time.Duration serialized strictly as a Go duration string
// such as "30s" or "5m". Numeric nanosecond values are intentionally rejected.
type Duration time.Duration

// UnmarshalJSON parses a quoted duration.
func (d *Duration) UnmarshalJSON(data []byte) error {
	const methodCtx = "config.Duration.UnmarshalJSON"

	var value string
	if err := json.Unmarshal(data, &value); err != nil {
		return fmt.Errorf("%s: длительность должна быть строкой: %w", methodCtx, err)
	}
	if value == "" {
		return fmt.Errorf("%s: длительность не может быть пустой", methodCtx)
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return fmt.Errorf("%s: некорректная длительность %q: %w", methodCtx, value, err)
	}
	*d = Duration(parsed)
	return nil
}

// MarshalJSON emits a human-readable duration string.
func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

// Value returns the standard-library duration value.
func (d Duration) Value() time.Duration {
	return time.Duration(d)
}

// Runtime is the complete controller policy/configuration document.
type Runtime struct {
	Version            int         `json:"version"`
	DetectorConfigPath string      `json:"detector_config"`
	Risk               Risk        `json:"risk"`
	Watchlist          []WatchItem `json:"watchlist"`
	Recipes            []Recipe    `json:"recipes,omitempty"`
	Navigation         Navigation  `json:"navigation"`
	Scanners           Scanners    `json:"scanners"`
	Strategy           Strategy    `json:"strategy,omitempty"`
}

// Risk contains mandatory monetary, confidence and capacity limits.
type Risk struct {
	MaxBudget          int64    `json:"max_budget"`
	MaxItemPrice       int64    `json:"max_item_price"`
	MinProfit          int64    `json:"min_profit"`
	MinROI             float64  `json:"min_roi"`
	MinConfidence      float64  `json:"min_confidence"`
	MaxQuoteAge        Duration `json:"max_quote_age"`
	AvailableSlots     int      `json:"available_slots"`
	AllowUnknownResale bool     `json:"allow_unknown_resale"`
}

// Domain converts validated configuration to the economy/risk domain type.
func (r Risk) Domain() domain.RiskLimits {
	return domain.RiskLimits{
		MaxBudget:          r.MaxBudget,
		MaxItemPrice:       r.MaxItemPrice,
		MinProfit:          r.MinProfit,
		MinROI:             r.MinROI,
		MinConfidence:      r.MinConfidence,
		MaxQuoteAge:        r.MaxQuoteAge.Value(),
		AvailableSlots:     r.AvailableSlots,
		AllowUnknownResale: r.AllowUnknownResale,
	}
}

// WatchItem is one explicitly approved item for market/contact scanning.
type WatchItem struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Enabled      bool   `json:"enabled"`
	MaxBuyPrice  int64  `json:"max_buy_price"`
	MinSalePrice int64  `json:"min_sale_price"`
}

// Recipe is the calibrated barter identity used by Contact Scanner. The
// scanner still verifies the visible result and ingredient quantities; these
// values are not a substitute for observation.
type Recipe struct {
	ID             string             `json:"id"`
	ContactID      string             `json:"contact_id"`
	ContactName    string             `json:"contact_name"`
	ResultItemID   string             `json:"result_item_id"`
	ResultItemName string             `json:"result_item_name"`
	ResultCount    int64              `json:"result_count"`
	Ingredients    []RecipeIngredient `json:"ingredients"`
	Enabled        bool               `json:"enabled"`
}

// RecipeIngredient maps stable item IDs to localized UI names.
type RecipeIngredient struct {
	ItemID   string `json:"item_id"`
	Name     string `json:"name"`
	Quantity int64  `json:"quantity"`
}

// Navigation is the calibrated, deterministic UI state graph.
type Navigation struct {
	Transitions []Transition `json:"transitions"`
}

// Transition describes all input actions required for one verified UI edge.
// Action order is significant and is therefore preserved.
type Transition struct {
	ID       string               `json:"id"`
	From     domain.ScreenState   `json:"from"`
	To       domain.ScreenState   `json:"to"`
	Class    protocol.ActionClass `json:"class,omitempty"`
	Actions  []Action             `json:"actions"`
	Verify   Verification         `json:"verify"`
	MaxRetry int                  `json:"max_retry"`
}

// Action is a safe input template. Point and BBox are alternatives. A BBox is
// resolved to its exact center rather than to a random point.
type Action struct {
	ID    string            `json:"id"`
	Kind  string            `json:"kind"`
	Point *domain.Point     `json:"point,omitempty"`
	BBox  *domain.Rectangle `json:"bbox,omitempty"`
	Value string            `json:"value,omitempty"`
	Delta int               `json:"delta,omitempty"`
}

// Target resolves a normalized point or the deterministic center of a bbox.
func (a Action) Target() (domain.Point, bool) {
	if a.Point != nil {
		return *a.Point, true
	}
	if a.BBox != nil {
		return domain.Point{
			X: a.BBox.X + a.BBox.Width/2,
			Y: a.BBox.Y + a.BBox.Height/2,
		}, true
	}
	return domain.Point{}, false
}

// Commands expands a validated semantic action into protocol actions. Mouse
// clicks and wheel input always move to their configured target first.
func (a Action) Commands() []protocol.Action {
	target, hasTarget := a.Target()
	switch a.Kind {
	case ActionMove:
		return []protocol.Action{{Kind: ActionMove, Point: &target}}
	case ActionClick:
		command := protocol.Action{Kind: ActionClick, Value: a.Value}
		if hasTarget {
			command.Point = &target
		}
		return []protocol.Action{command}
	case ActionScroll:
		command := protocol.Action{Kind: ActionScroll, Delta: a.Delta}
		if hasTarget {
			command.Point = &target
		}
		return []protocol.Action{command}
	case ActionKey:
		return []protocol.Action{{Kind: ActionKey, Value: a.Value}}
	case ActionText:
		return []protocol.Action{{Kind: ActionText, Value: a.Value}}
	default:
		return nil
	}
}

// Verification describes the observation required after a transition.
// BBox is reserved for a future ROI verifier and is rejected until that
// verifier is implemented end-to-end.
type Verification struct {
	State         domain.ScreenState `json:"state"`
	MinConfidence float64            `json:"min_confidence"`
	Timeout       Duration           `json:"timeout"`
	BBox          *domain.Rectangle  `json:"bbox,omitempty"`
}

// Scanners groups independent scanner schedules.
type Scanners struct {
	Market   ScannerTiming `json:"market"`
	Contacts ScannerTiming `json:"contacts"`
	Orders   ScannerTiming `json:"orders"`
}

// ScannerTiming controls refresh cadence and maximum accepted data age.
type ScannerTiming struct {
	Interval  Duration `json:"interval"`
	Staleness Duration `json:"staleness"`
}

// Strategy contains deterministic planner and scoring policy.
type Strategy struct {
	ProfitWeight        float64 `json:"profit_weight"`
	ROIWeight           float64 `json:"roi_weight"`
	LiquidityWeight     float64 `json:"liquidity_weight"`
	ConfidenceWeight    float64 `json:"confidence_weight"`
	RiskWeight          float64 `json:"risk_weight"`
	SlotWeight          float64 `json:"slot_weight"`
	ProfitScale         float64 `json:"profit_scale"`
	MaxStepAttempts     int     `json:"max_step_attempts"`
	MaxMultistepDepth   int     `json:"max_multistep_depth"`
	MaxRecipeExpansions int     `json:"max_recipe_expansions"`
}

// ScoreWeights converts validated configuration to economy policy.
func (s Strategy) ScoreWeights() economy.ScoreWeights {
	return economy.ScoreWeights{
		Profit: s.ProfitWeight, ROI: s.ROIWeight,
		Liquidity: s.LiquidityWeight, Confidence: s.ConfidenceWeight,
		Risk: s.RiskWeight, Slots: s.SlotWeight,
	}
}

// TrackedItemIDs returns the deterministic union required by inventory
// reconciliation (watchlist, barter ingredients and barter results).
func (r Runtime) TrackedItemIDs() []string {
	ids := make(map[string]struct{}, len(r.Watchlist))
	for _, item := range r.Watchlist {
		if item.Enabled {
			ids[item.ID] = struct{}{}
		}
	}
	for _, recipe := range r.Recipes {
		if !recipe.Enabled {
			continue
		}
		ids[recipe.ResultItemID] = struct{}{}
		for _, ingredient := range recipe.Ingredients {
			ids[ingredient.ItemID] = struct{}{}
		}
	}
	result := make([]string, 0, len(ids))
	for itemID := range ids {
		result = append(result, itemID)
	}
	sort.Strings(result)
	return result
}

func canonicalKeyChord(value string) string {
	parts := strings.Split(value, "+")
	for index := range parts {
		parts[index] = strings.ToUpper(strings.TrimSpace(parts[index]))
	}
	return strings.Join(parts, "+")
}

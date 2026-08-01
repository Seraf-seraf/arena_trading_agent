package config_test

import (
	"encoding/json"
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/arena-trading-agent/arena-trading-agent/internal/config"
	"github.com/arena-trading-agent/arena-trading-agent/internal/domain"
	"github.com/arena-trading-agent/arena-trading-agent/internal/protocol"
)

func TestDecodeValidConfigNormalizesAndSorts(t *testing.T) {
	value := validRuntime()
	value.Watchlist = append(value.Watchlist, config.WatchItem{
		ID: "alpha", Name: "Alpha", Enabled: true, MaxBuyPrice: 40, MinSalePrice: 60,
	})
	value.Navigation.Transitions = append(value.Navigation.Transitions, config.Transition{
		ID:   "alpha-transition",
		From: domain.StateMarketHome,
		To:   domain.StateMarketSearch,
		Actions: []config.Action{{
			ID: "alpha-action", Kind: config.ActionKey, Value: " ctrl + a ",
		}},
		Verify: config.Verification{
			State: domain.StateMarketSearch, MinConfidence: .8,
			Timeout: config.Duration(2 * time.Second),
		},
		MaxRetry: 1,
	})

	data := marshalConfig(t, value)
	got, err := config.Decode(strings.NewReader(string(data)), "/opt/arena/config")
	if err != nil {
		t.Fatal(err)
	}
	if got.DetectorConfigPath != "/opt/arena/config/screens.json" {
		t.Fatalf("detector path = %q", got.DetectorConfigPath)
	}
	if got.Watchlist[0].ID != "alpha" || got.Watchlist[1].ID != "zulu" {
		t.Fatalf("watchlist is not sorted: %#v", got.Watchlist)
	}
	if got.Navigation.Transitions[0].ID != "alpha-transition" ||
		got.Navigation.Transitions[1].ID != "zulu-transition" {
		t.Fatalf("transitions are not sorted: %#v", got.Navigation.Transitions)
	}
	if got.Navigation.Transitions[0].Actions[0].Value != "CTRL+A" {
		t.Fatalf("key chord was not canonicalized: %q", got.Navigation.Transitions[0].Actions[0].Value)
	}
	if got.Risk.Domain().MaxQuoteAge != 2*time.Minute {
		t.Fatalf("domain max quote age = %s", got.Risk.Domain().MaxQuoteAge)
	}
}

func TestLoadExample(t *testing.T) {
	path := filepath.Join("..", "..", "configs", "runtime.example.json")
	value, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if value.Version != config.CurrentVersion {
		t.Fatalf("version = %d", value.Version)
	}
	if !filepath.IsAbs(value.DetectorConfigPath) {
		t.Fatalf("detector path is not absolute: %q", value.DetectorConfigPath)
	}
	if filepath.Base(value.DetectorConfigPath) != "screens.example.json" {
		t.Fatalf("unexpected detector path: %q", value.DetectorConfigPath)
	}
}

func TestActionTargetAndCommands(t *testing.T) {
	action := config.Action{
		Kind:  config.ActionClick,
		BBox:  &domain.Rectangle{X: .1, Y: .2, Width: .4, Height: .2},
		Value: "LEFT",
	}
	point, ok := action.Target()
	if !ok || math.Abs(point.X-.3) > 1e-12 || math.Abs(point.Y-.3) > 1e-12 {
		t.Fatalf("target = %#v, %v", point, ok)
	}
	commands := action.Commands()
	if len(commands) != 1 || commands[0].Kind != config.ActionClick ||
		commands[0].Point == nil || *commands[0].Point != point ||
		commands[0].Value != "LEFT" {
		t.Fatalf("commands = %#v", commands)
	}
}

func TestNavigationTransitionsCompilesActionSequence(t *testing.T) {
	value := validRuntime()
	value.Navigation.Transitions[0].Actions = []config.Action{
		{ID: "focus", Kind: config.ActionClick, Point: &domain.Point{X: .2, Y: .3}, Value: "LEFT"},
		{ID: "text", Kind: config.ActionText, Value: "item"},
		{ID: "submit", Kind: config.ActionKey, Value: "ENTER"},
	}
	transitions, err := value.NavigationTransitions()
	if err != nil {
		t.Fatal(err)
	}
	if len(transitions) != 1 || transitions[0].Action.Kind != "SEQUENCE" ||
		len(transitions[0].Action.Steps) != 3 {
		t.Fatalf("compiled transitions = %#v", transitions)
	}
	if transitions[0].Action.Steps[0].Point == nil ||
		transitions[0].Action.Steps[1].Value != "item" {
		t.Fatalf("compiled action sequence = %#v", transitions[0].Action)
	}
	if transitions[0].Verify.Timeout != 5*time.Second ||
		transitions[0].Verify.BBox != nil {
		t.Fatalf("compiled verification = %#v", transitions[0].Verify)
	}
}

func TestValidationRejectsUnsupportedVerificationBBox(t *testing.T) {
	value := validRuntime()
	value.Navigation.Transitions[0].Verify.BBox = &domain.Rectangle{
		X: .1, Y: .2, Width: .3, Height: .4,
	}

	assertErrorContains(t, value.Validate(), "bbox пока не поддерживается runtime-проверкой")
}

func TestDecodeRejectsUnknownFieldsAtEveryLevel(t *testing.T) {
	data := string(marshalConfig(t, validRuntime()))
	tests := map[string]string{
		"top-level": strings.Replace(data, `"version":1`, `"version":1,"mystery":true`, 1),
		"nested":    strings.Replace(data, `"max_budget":1000`, `"max_budget":1000,"mystery":true`, 1),
		"action":    strings.Replace(data, `"kind":"CLICK"`, `"kind":"CLICK","mystery":true`, 1),
	}
	for name, document := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := config.Decode(strings.NewReader(document), t.TempDir())
			assertErrorContains(t, err, "не удалось декодировать runtime-конфигурацию")
		})
	}
}

func TestDecodeRejectsTrailingDataAndNumericDuration(t *testing.T) {
	data := string(marshalConfig(t, validRuntime()))
	t.Run("trailing JSON", func(t *testing.T) {
		_, err := config.Decode(strings.NewReader(data+"\n{}"), t.TempDir())
		assertErrorContains(t, err, "лишнее JSON-значение")
	})
	t.Run("trailing garbage", func(t *testing.T) {
		_, err := config.Decode(strings.NewReader(data+"\ninvalid"), t.TempDir())
		assertErrorContains(t, err, "данные после runtime-конфигурации")
	})
	t.Run("numeric duration", func(t *testing.T) {
		document := strings.Replace(data, `"max_quote_age":"2m0s"`, `"max_quote_age":120000000000`, 1)
		_, err := config.Decode(strings.NewReader(document), t.TempDir())
		assertErrorContains(t, err, "длительность должна быть строкой")
	})
}

func TestDecodeRejectsOversizedConfig(t *testing.T) {
	_, err := config.Decode(
		strings.NewReader(strings.Repeat(" ", int(config.MaxConfigBytes)+1)),
		t.TempDir(),
	)
	assertErrorContains(t, err, "превышает")
}

func TestValidationRejectsDuplicateIDsAndRoutes(t *testing.T) {
	tests := []struct {
		name   string
		change func(*config.Runtime)
		want   string
	}{
		{
			name: "watch item",
			change: func(value *config.Runtime) {
				value.Watchlist = append(value.Watchlist, value.Watchlist[0])
			},
			want: "дублирует watchlist",
		},
		{
			name: "transition",
			change: func(value *config.Runtime) {
				duplicate := value.Navigation.Transitions[0]
				duplicate.From = domain.StateContacts
				duplicate.To = domain.StateContactPage
				duplicate.Verify.State = duplicate.To
				duplicate.Actions = []config.Action{{
					ID: "different-action", Kind: config.ActionClick,
					Point: &domain.Point{X: .2, Y: .2},
				}}
				value.Navigation.Transitions = append(value.Navigation.Transitions, duplicate)
			},
			want: "дублирует navigation.transitions",
		},
		{
			name: "action",
			change: func(value *config.Runtime) {
				value.Navigation.Transitions[0].Actions = append(
					value.Navigation.Transitions[0].Actions,
					value.Navigation.Transitions[0].Actions[0],
				)
			},
			want: "дублирует идентификатор действия",
		},
		{
			name: "route",
			change: func(value *config.Runtime) {
				duplicate := value.Navigation.Transitions[0]
				duplicate.ID = "different-transition"
				duplicate.Actions = []config.Action{{
					ID: "different-action", Kind: config.ActionClick,
					Point: &domain.Point{X: .2, Y: .2},
				}}
				value.Navigation.Transitions = append(value.Navigation.Transitions, duplicate)
			},
			want: "дублирует маршрут",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			value := validRuntime()
			test.change(&value)
			assertErrorContains(t, value.Validate(), test.want)
		})
	}
}

func TestValidationRejectsInvalidStatesAndVerification(t *testing.T) {
	tests := []struct {
		name   string
		change func(*config.Transition)
		want   string
	}{
		{
			name: "unknown source",
			change: func(value *config.Transition) {
				value.From = domain.StateUnknown
			},
			want: "неподдерживаемое состояние",
		},
		{
			name: "unsupported destination",
			change: func(value *config.Transition) {
				value.To = "SOMETHING_NEW"
			},
			want: "неподдерживаемое состояние",
		},
		{
			name: "self edge",
			change: func(value *config.Transition) {
				value.To = value.From
				value.Verify.State = value.To
			},
			want: "должен изменять состояние экрана",
		},
		{
			name: "verify mismatch",
			change: func(value *config.Transition) {
				value.Verify.State = domain.StateContacts
			},
			want: "должно совпадать с transition.to",
		},
		{
			name: "retry",
			change: func(value *config.Transition) {
				value.MaxRetry = 11
			},
			want: "max_retry",
		},
		{
			name: "no actions",
			change: func(value *config.Transition) {
				value.Actions = nil
			},
			want: "actions должен содержать",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			value := validRuntime()
			test.change(&value.Navigation.Transitions[0])
			assertErrorContains(t, value.Validate(), test.want)
		})
	}
}

func TestValidationRejectsInvalidActionShapes(t *testing.T) {
	tests := []struct {
		name   string
		action config.Action
		want   string
	}{
		{
			name: "unsupported kind",
			action: config.Action{
				ID: "action", Kind: "DOUBLE_CLICK",
			},
			want: "неподдерживаемый тип действия",
		},
		{
			name: "click without target",
			action: config.Action{
				ID: "action", Kind: config.ActionClick,
			},
			want: "требует point или bbox",
		},
		{
			name: "point and bbox",
			action: config.Action{
				ID: "action", Kind: config.ActionClick,
				Point: &domain.Point{X: .1, Y: .1},
				BBox:  &domain.Rectangle{Width: .2, Height: .2},
			},
			want: "не должно одновременно задавать",
		},
		{
			name: "point outside",
			action: config.Action{
				ID: "action", Kind: config.ActionMove,
				Point: &domain.Point{X: 1.01, Y: .1},
			},
			want: "нормализованной точкой",
		},
		{
			name: "bbox outside",
			action: config.Action{
				ID: "action", Kind: config.ActionClick,
				BBox: &domain.Rectangle{X: .9, Width: .2, Height: .2},
			},
			want: "нормализованным прямоугольником",
		},
		{
			name: "scroll unit",
			action: config.Action{
				ID: "action", Kind: config.ActionScroll,
				Point: &domain.Point{X: .5, Y: .5}, Delta: 1,
			},
			want: "кратным 120",
		},
		{
			name: "unknown key",
			action: config.Action{
				ID: "action", Kind: config.ActionKey, Value: "CTRL+NOPE",
			},
			want: "неподдерживаемая клавиша",
		},
		{
			name: "text with target",
			action: config.Action{
				ID: "action", Kind: config.ActionText, Value: "query",
				Point: &domain.Point{X: .5, Y: .5},
			},
			want: "не принимает",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			value := validRuntime()
			value.Navigation.Transitions[0].Actions = []config.Action{test.action}
			assertErrorContains(t, value.Validate(), test.want)
		})
	}
}

func TestValidationRejectsNonFiniteGeometry(t *testing.T) {
	value := validRuntime()
	value.Navigation.Transitions[0].Actions[0].Point.X = math.NaN()
	assertErrorContains(t, value.Validate(), "нормализованной точкой")
}

func TestValidationRejectsMoneyConfidenceAndTimingRanges(t *testing.T) {
	tests := []struct {
		name   string
		change func(*config.Runtime)
		want   string
	}{
		{
			name: "zero budget",
			change: func(value *config.Runtime) {
				value.Risk.MaxBudget = 0
			},
			want: "max_budget",
		},
		{
			name: "item price above budget",
			change: func(value *config.Runtime) {
				value.Risk.MaxItemPrice = value.Risk.MaxBudget + 1
			},
			want: "не должно превышать max_budget",
		},
		{
			name: "watch price",
			change: func(value *config.Runtime) {
				value.Watchlist[0].MaxBuyPrice = value.Risk.MaxItemPrice + 1
			},
			want: "превышает risk.max_item_price",
		},
		{
			name: "confidence",
			change: func(value *config.Runtime) {
				value.Risk.MinConfidence = 1.01
			},
			want: "min_confidence",
		},
		{
			name: "quote age",
			change: func(value *config.Runtime) {
				value.Risk.MaxQuoteAge = config.Duration(time.Millisecond)
			},
			want: "max_quote_age",
		},
		{
			name: "scanner interval",
			change: func(value *config.Runtime) {
				value.Scanners.Market.Interval = config.Duration(time.Millisecond)
			},
			want: "scanners.market.interval",
		},
		{
			name: "scanner staleness",
			change: func(value *config.Runtime) {
				value.Scanners.Contacts.Staleness = config.Duration(time.Second)
			},
			want: "staleness не должно быть короче interval",
		},
		{
			name: "verify timeout",
			change: func(value *config.Runtime) {
				value.Navigation.Transitions[0].Verify.Timeout = config.Duration(time.Millisecond)
			},
			want: "verify.timeout",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			value := validRuntime()
			test.change(&value)
			assertErrorContains(t, value.Validate(), test.want)
		})
	}
}

func TestLoadErrorsForMissingFile(t *testing.T) {
	_, err := config.Load(filepath.Join(t.TempDir(), "missing.json"))
	assertErrorContains(t, err, "открыть runtime-конфигурацию")
}

func TestDurationJSONRoundTrip(t *testing.T) {
	value := config.Duration(90 * time.Second)
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != `"1m30s"` {
		t.Fatalf("duration JSON = %s", data)
	}
	var decoded config.Duration
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Value() != 90*time.Second {
		t.Fatalf("duration = %s", decoded.Value())
	}
}

func TestMoneyTransitionRequiresSinglePrimaryClick(t *testing.T) {
	value := validRuntime()
	transition := &value.Navigation.Transitions[0]
	transition.Class = protocol.ActionPurchase
	transition.MaxRetry = 0
	transition.Actions[0].Value = "LEFT"
	transition.Actions = append(transition.Actions, config.Action{
		ID:    "second-money-action",
		Kind:  config.ActionKey,
		Value: "ENTER",
	})
	assertErrorContains(t, value.Validate(), "ровно один финальный CLICK")

	value = validRuntime()
	transition = &value.Navigation.Transitions[0]
	transition.Class = protocol.ActionPurchase
	transition.MaxRetry = 0
	transition.Actions[0].Value = "RIGHT"
	assertErrorContains(t, value.Validate(), "только CLICK LEFT/PRIMARY")
}

func validRuntime() config.Runtime {
	return config.Runtime{
		Version:            config.CurrentVersion,
		DetectorConfigPath: "screens.json",
		Risk: config.Risk{
			MaxBudget: 1000, MaxItemPrice: 500, MinProfit: 10,
			MinROI: .1, MinConfidence: .8,
			MaxQuoteAge:    config.Duration(2 * time.Minute),
			AvailableSlots: 5,
		},
		Watchlist: []config.WatchItem{{
			ID: "zulu", Name: "Zulu", Enabled: true, MaxBuyPrice: 100, MinSalePrice: 150,
		}},
		Navigation: config.Navigation{Transitions: []config.Transition{{
			ID: "zulu-transition", From: domain.StateMainMenu, To: domain.StateMarketHome,
			Actions: []config.Action{{
				ID: "zulu-action", Kind: config.ActionClick,
				Point: &domain.Point{X: .5, Y: .5},
			}},
			Verify: config.Verification{
				State: domain.StateMarketHome, MinConfidence: .9,
				Timeout: config.Duration(5 * time.Second),
			},
			MaxRetry: 2,
		}}},
		Scanners: config.Scanners{
			Market: config.ScannerTiming{
				Interval: config.Duration(30 * time.Second), Staleness: config.Duration(2 * time.Minute),
			},
			Contacts: config.ScannerTiming{
				Interval: config.Duration(10 * time.Minute), Staleness: config.Duration(30 * time.Minute),
			},
			Orders: config.ScannerTiming{
				Interval: config.Duration(time.Minute), Staleness: config.Duration(5 * time.Minute),
			},
		},
	}
}

func marshalConfig(t *testing.T, value config.Runtime) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func assertErrorContains(t *testing.T, err error, text string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), text) {
		t.Fatalf("error = %v, want substring %q", err, text)
	}
}

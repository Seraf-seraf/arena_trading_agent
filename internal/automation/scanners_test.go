package automation

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	appconfig "github.com/arena-trading-agent/arena-trading-agent/internal/config"
	"github.com/arena-trading-agent/arena-trading-agent/internal/domain"
	"github.com/arena-trading-agent/arena-trading-agent/internal/navigation"
	"github.com/arena-trading-agent/arena-trading-agent/internal/protocol"
	"github.com/arena-trading-agent/arena-trading-agent/internal/repository"
)

type marketRouteCall struct {
	target    domain.ScreenState
	variables map[string]string
}

type marketScannerRouter struct {
	calls   []marketRouteCall
	handler func(domain.ScreenState, map[string]string) (navigation.Result, error)
}

func (r *marketScannerRouter) Navigate(
	_ context.Context,
	_ string,
	_ string,
	target domain.ScreenState,
	variables map[string]string,
) (navigation.Result, error) {
	copied := make(map[string]string, len(variables))
	for key, value := range variables {
		copied[key] = value
	}
	r.calls = append(r.calls, marketRouteCall{target: target, variables: copied})
	if r.handler == nil {
		return navigation.Result{}, nil
	}
	return r.handler(target, copied)
}

func TestMarketScanItemsReturnsSortedRecipeUnionWithoutDuplicates(t *testing.T) {
	watchlist := []appconfig.WatchItem{
		{ID: "zulu", Name: "Зулу", Enabled: true, MaxBuyPrice: 10, MinSalePrice: 20},
		{ID: "shared", Name: "Общий", Enabled: true, MaxBuyPrice: 30, MinSalePrice: 40},
		{ID: "disabled", Name: "Отключённый", Enabled: false},
	}
	recipes := []appconfig.Recipe{
		{
			ID: "recipe-b", Enabled: true,
			ResultItemID: "result", ResultItemName: "Результат",
			Ingredients: []appconfig.RecipeIngredient{
				{ItemID: "shared", Name: "Общий", Quantity: 1},
				{ItemID: "alpha", Name: "Альфа", Quantity: 2},
			},
		},
		{
			ID: "recipe-a", Enabled: true,
			ResultItemID: "alpha", ResultItemName: "Альфа",
			Ingredients: []appconfig.RecipeIngredient{
				{ItemID: "result", Name: "Результат", Quantity: 1},
			},
		},
		{
			ID: "disabled-recipe", Enabled: false,
			ResultItemID: "ignored", ResultItemName: "Игнорируемый",
		},
	}

	items, err := marketScanItems(watchlist, recipes)
	if err != nil {
		t.Fatalf("marketScanItems() error = %v", err)
	}
	gotIDs := make([]string, 0, len(items))
	for _, item := range items {
		gotIDs = append(gotIDs, item.ID)
	}
	wantIDs := []string{"alpha", "result", "shared", "zulu"}
	if !reflect.DeepEqual(gotIDs, wantIDs) {
		t.Fatalf("marketScanItems() IDs = %v, want %v", gotIDs, wantIDs)
	}
	if items[2].MaxBuyPrice != 30 || items[2].MinSalePrice != 40 {
		t.Fatalf("watchlist limits were lost: %#v", items[2])
	}
}

func TestMarketScanItemsRejectsIncompleteRecipeIdentity(t *testing.T) {
	tests := []struct {
		name    string
		recipes []appconfig.Recipe
		want    string
	}{
		{
			name: "empty result ID",
			recipes: []appconfig.Recipe{{
				Enabled: true, ResultItemName: "Результат",
			}},
			want: "пустой идентификатор",
		},
		{
			name: "empty ingredient name",
			recipes: []appconfig.Recipe{{
				Enabled: true, ResultItemID: "result", ResultItemName: "Результат",
				Ingredients: []appconfig.RecipeIngredient{{ItemID: "ingredient"}},
			}},
			want: "не содержит имени",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := marketScanItems(nil, test.recipes)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("marketScanItems() error = %v, want substring %q", err, test.want)
			}
		})
	}
}

func TestMarketScannerScanProcessesRecipeUnionInIDOrder(t *testing.T) {
	startedAt := time.Date(2026, 7, 30, 10, 0, 0, 0, time.UTC)
	router := &marketScannerRouter{handler: successfulMarketRoute(startedAt)}
	store := repository.NewMemory()
	scanner, err := NewMarketScannerWithRecipes(
		router,
		store,
		[]appconfig.WatchItem{
			{ID: "zulu", Name: "Зулу", Enabled: true, MaxBuyPrice: 10, MinSalePrice: 20},
			{ID: "shared", Name: "Общий", Enabled: true, MaxBuyPrice: 30, MinSalePrice: 40},
		},
		[]appconfig.Recipe{{
			ID: "recipe", Enabled: true,
			ResultItemID: "result", ResultItemName: "Результат",
			Ingredients: []appconfig.RecipeIngredient{
				{ItemID: "alpha", Name: "Альфа", Quantity: 1},
				{ItemID: "shared", Name: "Общий", Quantity: 2},
			},
		}},
		0.90,
	)
	if err != nil {
		t.Fatalf("NewMarketScannerWithRecipes() error = %v", err)
	}

	report := scanner.Scan(context.Background(), "agent", "session")
	if len(report.Errors) != 0 || report.Scanned != 4 {
		t.Fatalf("Scan() report = %#v", report)
	}
	var scannedIDs []string
	for _, call := range router.calls {
		if call.target == domain.StateMarketHome {
			scannedIDs = append(scannedIDs, call.variables["item.id"])
		}
	}
	wantIDs := []string{"alpha", "result", "shared", "zulu"}
	if !reflect.DeepEqual(scannedIDs, wantIDs) {
		t.Fatalf("Scan() item order = %v, want %v", scannedIDs, wantIDs)
	}
	for _, itemID := range wantIDs {
		if _, err := store.LatestTradeQuote(context.Background(), itemID); err != nil {
			t.Fatalf("LatestTradeQuote(%q) error = %v", itemID, err)
		}
	}
}

func TestMarketScannerScanItemUsesFreshCardAndPersistsCompleteQuote(t *testing.T) {
	startedAt := time.Date(2026, 7, 30, 10, 0, 0, 0, time.UTC)
	router := &marketScannerRouter{}
	router.handler = successfulMarketRoute(startedAt)
	store := repository.NewMemory()
	scanner, err := NewMarketScannerWithRecipes(router, store, nil, nil, 0.90)
	if err != nil {
		t.Fatalf("NewMarketScannerWithRecipes() error = %v", err)
	}
	item := appconfig.WatchItem{
		ID: "item", Name: "Предмет", Enabled: true,
		MaxBuyPrice: 110, MinSalePrice: 140,
	}

	quote, err := scanner.ScanItem(context.Background(), "agent", "session", item)
	if err != nil {
		t.Fatalf("ScanItem() error = %v", err)
	}
	wantTargets := []domain.ScreenState{
		domain.StateMarketHome,
		domain.StateMarketResults,
		domain.StateItemCard,
	}
	gotTargets := make([]domain.ScreenState, 0, len(router.calls))
	for _, call := range router.calls {
		gotTargets = append(gotTargets, call.target)
		if call.variables["item.id"] != item.ID || call.variables["item.name"] != item.Name {
			t.Fatalf("Navigate() variables = %#v", call.variables)
		}
	}
	if !reflect.DeepEqual(gotTargets, wantTargets) {
		t.Fatalf("Navigate() targets = %v, want %v", gotTargets, wantTargets)
	}
	if quote.ItemID != item.ID || quote.PurchasePrice != 100 ||
		quote.SalePrice != 150 || quote.SaleCommission != 15 ||
		quote.ListingFee != 5 {
		t.Fatalf("ScanItem() quote = %#v", quote)
	}
	if quote.ResaleKnown || quote.LiquidityScore != 0 || quote.PriceVolatility != 1 {
		t.Fatalf("первая котировка получила небезопасные рыночные метрики: %#v", quote)
	}
	if !quote.ObservedAt.Equal(startedAt.Add(time.Millisecond)) {
		t.Fatalf("quote.ObservedAt = %s", quote.ObservedAt)
	}
	if quote.Confidence != 0.94 {
		t.Fatalf("quote.Confidence = %.3f, want 0.940", quote.Confidence)
	}
	saved, err := store.LatestTradeQuote(context.Background(), item.ID)
	if err != nil {
		t.Fatalf("LatestTradeQuote() error = %v", err)
	}
	if !reflect.DeepEqual(saved, quote) {
		t.Fatalf("saved quote = %#v, want %#v", saved, quote)
	}
	compact, err := store.LatestQuote(context.Background(), item.ID)
	if err != nil {
		t.Fatalf("LatestQuote() error = %v", err)
	}
	if compact.BuyPrice != quote.PurchasePrice || compact.SalePrice != quote.SalePrice {
		t.Fatalf("saved compact quote = %#v", compact)
	}
}

func TestMarketScannerScanItemDerivesMetricsFromHistoryBeforeSaving(t *testing.T) {
	startedAt := time.Date(2026, 7, 30, 10, 0, 0, 0, time.UTC)
	router := &marketScannerRouter{handler: successfulMarketRoute(startedAt)}
	store := repository.NewMemory()
	for hoursAgo := 3; hoursAgo >= 1; hoursAgo-- {
		if err := store.SaveTradeQuote(
			context.Background(),
			metricTestQuote(
				"item",
				startedAt.Add(-time.Duration(hoursAgo)*time.Hour),
				100,
				150,
			),
		); err != nil {
			t.Fatalf("SaveTradeQuote() вернул ошибку: %v", err)
		}
	}
	scanner, err := NewMarketScannerWithRecipes(router, store, nil, nil, 0.90)
	if err != nil {
		t.Fatalf("NewMarketScannerWithRecipes() вернул ошибку: %v", err)
	}

	quote, err := scanner.ScanItem(
		context.Background(),
		"agent",
		"session",
		appconfig.WatchItem{ID: "item", Name: "Предмет"},
	)
	if err != nil {
		t.Fatalf("ScanItem() вернул ошибку: %v", err)
	}
	if !quote.ResaleKnown || quote.PriceVolatility != 0 ||
		quote.LiquidityScore != marketMetricsLiquidityCap {
		t.Fatalf("котировка не получила метрики из истории: %#v", quote)
	}
	saved, err := store.LatestTradeQuote(context.Background(), "item")
	if err != nil {
		t.Fatalf("LatestTradeQuote() вернул ошибку: %v", err)
	}
	if !reflect.DeepEqual(saved, quote) {
		t.Fatalf("метрики не сохранены вместе с котировкой: got=%#v want=%#v", saved, quote)
	}
}

func TestMarketScannerScanItemFailsClosedOnIncompleteCard(t *testing.T) {
	startedAt := time.Date(2026, 7, 30, 10, 0, 0, 0, time.UTC)
	router := &marketScannerRouter{}
	handler := successfulMarketRoute(startedAt)
	router.handler = func(
		target domain.ScreenState,
		variables map[string]string,
	) (navigation.Result, error) {
		result, err := handler(target, variables)
		if target == domain.StateItemCard {
			delete(result.Observation.Values, valueListingFee)
		}
		return result, err
	}
	store := repository.NewMemory()
	scanner, err := NewMarketScannerWithRecipes(router, store, nil, nil, 0.90)
	if err != nil {
		t.Fatalf("NewMarketScannerWithRecipes() error = %v", err)
	}

	quote, err := scanner.ScanItem(context.Background(), "agent", "session", appconfig.WatchItem{
		ID: "item", Name: "Предмет",
	})
	if err == nil || !strings.Contains(err.Error(), "listing_fee") {
		t.Fatalf("ScanItem() error = %v", err)
	}
	if quote != (domain.TradeQuote{}) {
		t.Fatalf("ScanItem() returned partial quote: %#v", quote)
	}
	_, lookupErr := store.LatestTradeQuote(context.Background(), "item")
	if !errors.Is(lookupErr, repository.ErrNotFound) {
		t.Fatalf("LatestTradeQuote() error = %v, want ErrNotFound", lookupErr)
	}
}

func TestMarketScannerScanItemRejectsUnverifiedOrContradictoryCard(t *testing.T) {
	startedAt := time.Date(2026, 7, 30, 10, 0, 0, 0, time.UTC)
	tests := []struct {
		name   string
		mutate func(*navigation.Result)
		want   string
	}{
		{
			name: "wrong state",
			mutate: func(result *navigation.Result) {
				result.Observation.State = domain.StateMarketResults
			},
			want: "ожидалось состояние ITEM_CARD",
		},
		{
			name: "stale frame",
			mutate: func(result *navigation.Result) {
				result.Frame.ID = 10
				result.Observation.FrameID = 10
			},
			want: "не новее результатов рынка",
		},
		{
			name: "changed price",
			mutate: func(result *navigation.Result) {
				result.Observation.Values[valuePurchasePrice] = observedValue("101", 0.99)
			},
			want: "изменилось между результатами",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			router := &marketScannerRouter{}
			handler := successfulMarketRoute(startedAt)
			router.handler = func(
				target domain.ScreenState,
				variables map[string]string,
			) (navigation.Result, error) {
				result, err := handler(target, variables)
				if target == domain.StateItemCard {
					test.mutate(&result)
				}
				return result, err
			}
			store := repository.NewMemory()
			scanner, err := NewMarketScannerWithRecipes(router, store, nil, nil, 0.90)
			if err != nil {
				t.Fatalf("NewMarketScannerWithRecipes() error = %v", err)
			}
			_, err = scanner.ScanItem(context.Background(), "agent", "session", appconfig.WatchItem{
				ID: "item", Name: "Предмет",
			})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("ScanItem() error = %v, want substring %q", err, test.want)
			}
			_, lookupErr := store.LatestTradeQuote(context.Background(), "item")
			if !errors.Is(lookupErr, repository.ErrNotFound) {
				t.Fatalf("LatestTradeQuote() error = %v, want ErrNotFound", lookupErr)
			}
		})
	}
}

func successfulMarketRoute(
	startedAt time.Time,
) func(domain.ScreenState, map[string]string) (navigation.Result, error) {
	return func(
		target domain.ScreenState,
		_ map[string]string,
	) (navigation.Result, error) {
		switch target {
		case domain.StateMarketHome:
			return navigation.Result{}, nil
		case domain.StateMarketResults:
			return marketNavigationResult(
				10,
				startedAt,
				target,
				map[string]domain.Value{
					valuePurchasePrice: observedValue("100", 0.99),
					valueSalePrice:     observedValue("150", 0.98),
				},
				0.98,
			), nil
		case domain.StateItemCard:
			return marketNavigationResult(
				11,
				startedAt.Add(time.Second),
				target,
				map[string]domain.Value{
					valueSaleCommission: observedValue("15", 0.94),
					valueListingFee:     observedValue("5", 0.99),
				},
				0.97,
			), nil
		default:
			return navigation.Result{}, fmt.Errorf("неожиданная цель %s", target)
		}
	}
}

func marketNavigationResult(
	frameID uint64,
	capturedAt time.Time,
	state domain.ScreenState,
	values map[string]domain.Value,
	confidence float64,
) navigation.Result {
	return navigation.Result{
		Frame: protocol.Frame{
			ID: frameID, CapturedAt: capturedAt,
			Encoding: "png", Data: []byte{1},
		},
		Observation: domain.Observation{
			FrameID: frameID, State: state, Values: values,
			Confidence: confidence, CreatedAt: capturedAt.Add(time.Millisecond),
		},
	}
}

func observedValue(value string, confidence float64) domain.Value {
	return domain.Value{
		Raw: value, Normalized: value, Source: "OCR", Confidence: confidence,
	}
}

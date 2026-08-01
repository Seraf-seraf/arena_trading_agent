package opportunity_test

import (
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/arena-trading-agent/arena-trading-agent/internal/domain"
	"github.com/arena-trading-agent/arena-trading-agent/internal/opportunity"
)

func TestFinderBuildsDirectFlip(t *testing.T) {
	t.Parallel()
	observedAt := time.Date(2026, 7, 29, 10, 0, 0, 0, time.UTC)
	finder := mustFinder(t, opportunity.Config{QuoteTTL: 5 * time.Minute})
	values, err := finder.Find(opportunity.Input{Quotes: []domain.TradeQuote{{
		ItemID:          "item",
		PurchasePrice:   9_070,
		SalePrice:       30_000,
		SaleCommission:  2_000,
		ListingFee:      500,
		ObservedAt:      observedAt,
		Confidence:      .91,
		LiquidityScore:  .7,
		PriceVolatility: .2,
		ResaleKnown:     true,
	}}})
	if err != nil {
		t.Fatal(err)
	}
	direct := findByID(t, values, "direct:item")
	if direct.ExpectedProfit != 18_430 || direct.ROI != float64(18_430)/9_070 {
		t.Fatalf("unexpected direct financials: %+v", direct)
	}
	if direct.ExpiresAt != observedAt.Add(5*time.Minute) ||
		direct.QuoteObservedAt != observedAt ||
		!direct.ResaleKnown {
		t.Fatalf("unexpected direct metadata: %+v", direct)
	}
	if got := []string{direct.Steps[0].Kind, direct.Steps[1].Kind}; !reflect.DeepEqual(
		got,
		[]string{domain.TradeStepBuy, domain.TradeStepList},
	) {
		t.Fatalf("step kinds = %v", got)
	}
}

func TestFinderBuildsContactBarterWithAggregatedSortedInputs(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 29, 10, 0, 0, 0, time.UTC)
	finder := mustFinder(t, opportunity.Config{})
	values, err := finder.Find(opportunity.Input{
		AsOf: now,
		Quotes: []domain.TradeQuote{
			quote("result", 500, 100, now.Add(-time.Minute), .95),
			quote("b", 25, 30, now.Add(-3*time.Minute), .80),
			quote("a", 10, 15, now.Add(-2*time.Minute), .90),
		},
		Recipes: []domain.BarterRecipe{{
			ID:        "recipe",
			ContactID: "contact",
			Ingredients: []domain.BarterIngredient{
				{ItemID: "b", Quantity: 1},
				{ItemID: "a", Quantity: 3},
				{ItemID: "a", Quantity: 2},
			},
			ResultItem:  "result",
			ResultCount: 2,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	barter := findByID(t, values, "barter:recipe")
	// result quote: sale 100/unit, commission 10/unit, listing fee 1.
	if barter.InputCost != 75 || barter.ExpectedRevenue != 200 ||
		barter.ExpectedFees != 21 || barter.ExpectedProfit != 104 {
		t.Fatalf("unexpected barter financials: %+v", barter)
	}
	if barter.Confidence != .80 || barter.QuoteObservedAt != now.Add(-3*time.Minute) {
		t.Fatalf("unexpected aggregate metadata: %+v", barter)
	}
	if barter.RequiredSlots != 2 {
		t.Fatalf("required slots = %d, want 2", barter.RequiredSlots)
	}
	if len(barter.Steps) != 4 ||
		barter.Steps[0].ItemID != "a" || barter.Steps[0].Quantity != 5 ||
		barter.Steps[1].ItemID != "b" || barter.Steps[1].Quantity != 1 ||
		barter.Steps[2].RecipeID != "recipe" ||
		barter.Steps[3].Kind != domain.TradeStepList {
		t.Fatalf("unexpected deterministic steps: %+v", barter.Steps)
	}
}

func TestFinderBuildsCheapestBoundedMultistepBarter(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 29, 10, 0, 0, 0, time.UTC)
	input := opportunity.Input{
		AsOf: now,
		Quotes: []domain.TradeQuote{
			quote("ore", 10, 10, now, .9),
			quote("part", 100, 100, now, .9),
			quote("result", 1_000, 300, now, .9),
		},
		Recipes: []domain.BarterRecipe{
			{
				ID:          "make-part",
				ContactID:   "contact",
				Ingredients: []domain.BarterIngredient{{ItemID: "ore", Quantity: 2}},
				ResultItem:  "part",
				ResultCount: 1,
			},
			{
				ID:          "make-result",
				ContactID:   "contact",
				Ingredients: []domain.BarterIngredient{{ItemID: "part", Quantity: 2}},
				ResultItem:  "result",
				ResultCount: 1,
			},
		},
	}
	finder := mustFinder(t, opportunity.Config{MaxMultistepDepth: 2})
	values, err := finder.Find(input)
	if err != nil {
		t.Fatal(err)
	}
	multistep := findByID(t, values, "multistep:make-result")
	if multistep.InputCost != 40 || multistep.ExpectedProfit != 249 {
		t.Fatalf("unexpected multistep financials: %+v", multistep)
	}
	wantKinds := []string{
		domain.TradeStepBuy,
		domain.TradeStepBarter,
		domain.TradeStepBarter,
		domain.TradeStepList,
	}
	var gotKinds []string
	for _, step := range multistep.Steps {
		gotKinds = append(gotKinds, step.Kind)
	}
	if !reflect.DeepEqual(gotKinds, wantKinds) ||
		multistep.Steps[0].Quantity != 4 ||
		multistep.Steps[1].Quantity != 2 {
		t.Fatalf("unexpected multistep plan: %+v", multistep.Steps)
	}

	shallow := mustFinder(t, opportunity.Config{MaxMultistepDepth: 1})
	shallowValues, err := shallow.Find(input)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range shallowValues {
		if value.Kind == domain.OpportunityMultistepTrade {
			t.Fatalf("depth-one finder returned multistep plan: %+v", value)
		}
	}
}

func TestFinderIsDeterministicForShuffledInputs(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 29, 10, 0, 0, 0, time.UTC)
	quotes := []domain.TradeQuote{
		quote("z", 4, 15, now, .9),
		quote("a", 3, 8, now, .8),
		quote("result", 50, 100, now, .95),
	}
	recipes := []domain.BarterRecipe{{
		ID:          "recipe",
		ContactID:   "contact",
		Ingredients: []domain.BarterIngredient{{ItemID: "z", Quantity: 1}, {ItemID: "a", Quantity: 1}},
		ResultItem:  "result",
		ResultCount: 1,
	}}
	finder := mustFinder(t, opportunity.Config{})
	first, err := finder.Find(opportunity.Input{AsOf: now, Quotes: quotes, Recipes: recipes})
	if err != nil {
		t.Fatal(err)
	}
	second, err := finder.Find(opportunity.Input{
		AsOf:    now,
		Quotes:  []domain.TradeQuote{quotes[2], quotes[0], quotes[1]},
		Recipes: append([]domain.BarterRecipe(nil), recipes...),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("finder output depends on input order:\nfirst=%+v\nsecond=%+v", first, second)
	}
}

func TestFinderSkipsRecipeOnCooldownAndRejectsOverflow(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 29, 10, 0, 0, 0, time.UTC)
	finder := mustFinder(t, opportunity.Config{})
	values, err := finder.Find(opportunity.Input{
		AsOf: now,
		Quotes: []domain.TradeQuote{
			quote("input", 1, 1, now, 1),
			quote("result", 1, 10, now, 1),
		},
		Recipes: []domain.BarterRecipe{{
			ID:          "cooldown",
			ContactID:   "contact",
			Ingredients: []domain.BarterIngredient{{ItemID: "input", Quantity: 1}},
			ResultItem:  "result",
			ResultCount: 1,
			AvailableAt: now.Add(time.Minute),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range values {
		if strings.Contains(value.ID, "cooldown") {
			t.Fatalf("cooldown recipe produced opportunity %+v", value)
		}
	}

	_, err = finder.Find(opportunity.Input{Quotes: []domain.TradeQuote{{
		ItemID:         "overflow",
		PurchasePrice:  1,
		SalePrice:      1,
		SaleCommission: math.MaxInt64,
		ListingFee:     1,
		Confidence:     1,
		LiquidityScore: 1,
		ResaleKnown:    true,
	}}})
	if err == nil {
		t.Fatal("finder accepted overflowing fees")
	}
}

func mustFinder(t *testing.T, config opportunity.Config) *opportunity.Finder {
	t.Helper()
	finder, err := opportunity.NewFinder(config)
	if err != nil {
		t.Fatal(err)
	}
	return finder
}

func quote(itemID string, purchase, sale int64, observedAt time.Time, confidence float64) domain.TradeQuote {
	return domain.TradeQuote{
		ItemID:          itemID,
		PurchasePrice:   purchase,
		SalePrice:       sale,
		SaleCommission:  10,
		ListingFee:      1,
		ObservedAt:      observedAt,
		Confidence:      confidence,
		LiquidityScore:  .5,
		PriceVolatility: .1,
		ResaleKnown:     true,
	}
}

func findByID(t *testing.T, values []domain.TradeOpportunity, id string) domain.TradeOpportunity {
	t.Helper()
	for _, value := range values {
		if value.ID == id {
			return value
		}
	}
	t.Fatalf("opportunity %q not found in %+v", id, values)
	return domain.TradeOpportunity{}
}

// Package opportunity builds deterministic, executable trade opportunities
// from an immutable set of quotes and barter recipes.
package opportunity

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/arena-trading-agent/arena-trading-agent/internal/domain"
	"github.com/arena-trading-agent/arena-trading-agent/internal/economy"
	"github.com/arena-trading-agent/arena-trading-agent/internal/money"
)

var errUnavailable = errors.New("обязательные рыночные данные или данные обмена недоступны")

// Config bounds recursive barter planning. MaxMultistepDepth counts the root
// barter too, so a value below two disables MULTISTEP_BARTER opportunities.
type Config struct {
	MaxMultistepDepth   int
	MaxRecipeExpansions int
	QuoteTTL            time.Duration
}

// Input is a consistent planning snapshot. AsOf is used only for recipe
// cooldown checks; a zero value deliberately disables those checks.
type Input struct {
	AsOf    time.Time
	Quotes  []domain.TradeQuote
	Recipes []domain.BarterRecipe
}

// Finder is immutable and safe for concurrent use.
type Finder struct {
	config Config
}

// NewFinder validates and applies conservative finite defaults.
func NewFinder(config Config) (*Finder, error) {
	const methodCtx = "opportunity.NewFinder"

	if config.MaxMultistepDepth < 0 {
		return nil, fmt.Errorf("%s: максимальная глубина многоступенчатого обмена не может быть отрицательной", methodCtx)
	}
	if config.MaxRecipeExpansions < 0 {
		return nil, fmt.Errorf("%s: максимальное число раскрытий рецептов не может быть отрицательным", methodCtx)
	}
	if config.QuoteTTL < 0 {
		return nil, fmt.Errorf("%s: срок жизни котировки не может быть отрицательным", methodCtx)
	}
	if config.MaxMultistepDepth == 0 {
		config.MaxMultistepDepth = 3
	}
	if config.MaxRecipeExpansions == 0 {
		config.MaxRecipeExpansions = 10_000
	}
	return &Finder{config: config}, nil
}

// Find creates direct flips, direct-input contact barters and the cheapest
// bounded multistep barter plan for every sellable recipe result.
func (f *Finder) Find(input Input) ([]domain.TradeOpportunity, error) {
	const methodCtx = "opportunity.Finder.Find"

	if f == nil {
		return nil, fmt.Errorf("%s: поиск возможностей не задан", methodCtx)
	}
	quotes, quoteIDs, err := indexQuotes(input.Quotes)
	if err != nil {
		return nil, fmt.Errorf("%s: не удалось индексировать котировки: %w", methodCtx, err)
	}
	recipes, recipesByResult, err := indexRecipes(input.Recipes)
	if err != nil {
		return nil, fmt.Errorf("%s: не удалось индексировать рецепты: %w", methodCtx, err)
	}

	result := make([]domain.TradeOpportunity, 0, len(quotes)+len(recipes)*2)
	for _, itemID := range quoteIDs {
		candidate, err := f.directOpportunity(quotes[itemID])
		if err != nil {
			return nil, fmt.Errorf("%s: не удалось построить прямую перепродажу %q: %w", methodCtx, itemID, err)
		}
		result = append(result, candidate)
	}

	for _, recipe := range recipes {
		if !recipeAvailable(recipe, input.AsOf) {
			continue
		}
		candidate, err := f.contactOpportunity(recipe, quotes)
		switch {
		case err == nil:
			result = append(result, candidate)
		case errors.Is(err, errUnavailable):
		default:
			return nil, fmt.Errorf("%s: не удалось построить контактный обмен %q: %w", methodCtx, recipe.ID, err)
		}
	}

	if f.config.MaxMultistepDepth >= 2 {
		expansions := 0
		for _, recipe := range recipes {
			if !recipeAvailable(recipe, input.AsOf) {
				continue
			}
			candidate, err := f.multistepOpportunity(
				recipe,
				input.AsOf,
				quotes,
				recipesByResult,
				&expansions,
			)
			switch {
			case err == nil:
				result = append(result, candidate)
			case errors.Is(err, errUnavailable):
			default:
				return nil, fmt.Errorf("%s: не удалось построить многоступенчатый обмен %q: %w", methodCtx, recipe.ID, err)
			}
		}
	}

	sort.Slice(result, func(i, j int) bool {
		if result[i].ExpectedProfit != result[j].ExpectedProfit {
			return result[i].ExpectedProfit > result[j].ExpectedProfit
		}
		if result[i].InputCost != result[j].InputCost {
			return result[i].InputCost < result[j].InputCost
		}
		if result[i].Kind != result[j].Kind {
			return result[i].Kind < result[j].Kind
		}
		return result[i].ID < result[j].ID
	})
	return result, nil
}

func indexQuotes(values []domain.TradeQuote) (map[string]domain.TradeQuote, []string, error) {
	const methodCtx = "opportunity.indexQuotes"

	result := make(map[string]domain.TradeQuote, len(values))
	ids := make([]string, 0, len(values))
	for _, quote := range values {
		if err := validateQuote(quote); err != nil {
			return nil, nil, fmt.Errorf("%s: некорректная котировка: %w", methodCtx, err)
		}
		if _, exists := result[quote.ItemID]; exists {
			return nil, nil, fmt.Errorf("%s: котировка предмета %q продублирована", methodCtx, quote.ItemID)
		}
		result[quote.ItemID] = quote
		ids = append(ids, quote.ItemID)
	}
	sort.Strings(ids)
	return result, ids, nil
}

func validateQuote(quote domain.TradeQuote) error {
	const methodCtx = "opportunity.validateQuote"

	if quote.ItemID == "" {
		return fmt.Errorf("%s: идентификатор предмета котировки пуст", methodCtx)
	}
	if quote.PurchasePrice < 0 || quote.SalePrice < 0 ||
		quote.SaleCommission < 0 || quote.ListingFee < 0 {
		return fmt.Errorf("%s: котировка предмета %q содержит отрицательные денежные значения", methodCtx, quote.ItemID)
	}
	if !finiteUnit(quote.Confidence) {
		return fmt.Errorf("%s: котировка предмета %q содержит некорректную уверенность", methodCtx, quote.ItemID)
	}
	if !finiteUnit(quote.LiquidityScore) {
		return fmt.Errorf("%s: котировка предмета %q содержит некорректную ликвидность", methodCtx, quote.ItemID)
	}
	if !finiteNonNegative(quote.PriceVolatility) {
		return fmt.Errorf("%s: котировка предмета %q содержит некорректную волатильность", methodCtx, quote.ItemID)
	}
	return nil
}

func indexRecipes(values []domain.BarterRecipe) (
	[]domain.BarterRecipe,
	map[string][]domain.BarterRecipe,
	error,
) {
	const methodCtx = "opportunity.indexRecipes"

	recipes := append([]domain.BarterRecipe(nil), values...)
	sort.Slice(recipes, func(i, j int) bool { return recipes[i].ID < recipes[j].ID })
	seen := make(map[string]struct{}, len(recipes))
	byResult := make(map[string][]domain.BarterRecipe)
	for _, recipe := range recipes {
		if err := validateRecipe(recipe); err != nil {
			return nil, nil, fmt.Errorf("%s: некорректный рецепт: %w", methodCtx, err)
		}
		if _, exists := seen[recipe.ID]; exists {
			return nil, nil, fmt.Errorf("%s: рецепт обмена %q продублирован", methodCtx, recipe.ID)
		}
		seen[recipe.ID] = struct{}{}
		byResult[recipe.ResultItem] = append(byResult[recipe.ResultItem], recipe)
	}
	return recipes, byResult, nil
}

func validateRecipe(recipe domain.BarterRecipe) error {
	const methodCtx = "opportunity.validateRecipe"

	if recipe.ID == "" || recipe.ContactID == "" || recipe.ResultItem == "" {
		return fmt.Errorf("%s: рецепт обмена содержит пустой идентификатор", methodCtx)
	}
	if recipe.ResultCount <= 0 {
		return fmt.Errorf("%s: рецепт обмена %q содержит некорректное количество результата", methodCtx, recipe.ID)
	}
	if len(recipe.Ingredients) == 0 {
		return fmt.Errorf("%s: рецепт обмена %q не содержит ингредиентов", methodCtx, recipe.ID)
	}
	for _, ingredient := range recipe.Ingredients {
		if ingredient.ItemID == "" || ingredient.Quantity <= 0 {
			return fmt.Errorf("%s: рецепт обмена %q содержит некорректный ингредиент", methodCtx, recipe.ID)
		}
	}
	_, err := aggregateIngredients(recipe.Ingredients, 1)
	if err != nil {
		return fmt.Errorf("%s: не удалось агрегировать ингредиенты рецепта %q: %w", methodCtx, recipe.ID, err)
	}
	return nil
}

func (f *Finder) directOpportunity(quote domain.TradeQuote) (domain.TradeOpportunity, error) {
	const methodCtx = "opportunity.Finder.directOpportunity"

	fees, err := money.Add(quote.SaleCommission, quote.ListingFee)
	if err != nil {
		return domain.TradeOpportunity{}, fmt.Errorf("%s: переполнение комиссий предмета %q: %w", methodCtx, quote.ItemID, err)
	}
	candidate := domain.TradeOpportunity{
		ID:              "direct:" + quote.ItemID,
		Kind:            domain.OpportunityDirectFlip,
		InputCost:       quote.PurchasePrice,
		ExpectedRevenue: quote.SalePrice,
		ExpectedFees:    fees,
		Confidence:      quote.Confidence,
		LiquidityScore:  quote.LiquidityScore,
		PriceVolatility: quote.PriceVolatility,
		RequiredSlots:   1,
		QuoteObservedAt: quote.ObservedAt,
		ResaleKnown:     quote.ResaleKnown,
		ResultItemID:    quote.ItemID,
		ResultQuantity:  1,
		ExpiresAt:       f.expiresAt(quote.ObservedAt),
		Steps: []domain.TradeStep{
			{Kind: domain.TradeStepBuy, ItemID: quote.ItemID, Quantity: 1, LimitPrice: quote.PurchasePrice},
			{Kind: domain.TradeStepList, ItemID: quote.ItemID, Quantity: 1, LimitPrice: quote.SalePrice},
		},
	}
	return complete(candidate)
}

func (f *Finder) contactOpportunity(
	recipe domain.BarterRecipe,
	quotes map[string]domain.TradeQuote,
) (domain.TradeOpportunity, error) {
	const methodCtx = "opportunity.Finder.contactOpportunity"

	output, ok := quotes[recipe.ResultItem]
	if !ok {
		return domain.TradeOpportunity{}, errUnavailable
	}
	ingredients, err := aggregateIngredients(recipe.Ingredients, 1)
	if err != nil {
		return domain.TradeOpportunity{}, fmt.Errorf("%s: не удалось агрегировать ингредиенты: %w", methodCtx, err)
	}
	var cost int64
	meta := acquisitionMeta{}
	steps := make([]domain.TradeStep, 0, len(ingredients)+2)
	for _, ingredient := range ingredients {
		quote, ok := quotes[ingredient.ItemID]
		if !ok {
			return domain.TradeOpportunity{}, errUnavailable
		}
		line, err := money.Multiply(quote.PurchasePrice, ingredient.Quantity)
		if err != nil {
			return domain.TradeOpportunity{}, fmt.Errorf("%s: не удалось рассчитать стоимость ингредиента %q: %w", methodCtx, ingredient.ItemID, err)
		}
		cost, err = money.Add(cost, line)
		if err != nil {
			return domain.TradeOpportunity{}, fmt.Errorf("%s: переполнение стоимости обмена: %w", methodCtx, err)
		}
		meta.addQuote(quote)
		steps = append(steps, domain.TradeStep{
			Kind:       domain.TradeStepBuy,
			ItemID:     ingredient.ItemID,
			Quantity:   ingredient.Quantity,
			LimitPrice: quote.PurchasePrice,
		})
	}
	steps = append(steps,
		domain.TradeStep{
			Kind:     domain.TradeStepBarter,
			ItemID:   recipe.ResultItem,
			RecipeID: recipe.ID,
			Quantity: 1,
		},
		domain.TradeStep{
			Kind:       domain.TradeStepList,
			ItemID:     recipe.ResultItem,
			Quantity:   recipe.ResultCount,
			LimitPrice: output.SalePrice,
		},
	)
	revenue, fees, err := saleFinancials(output, recipe.ResultCount)
	if err != nil {
		return domain.TradeOpportunity{}, fmt.Errorf("%s: не удалось рассчитать параметры продажи: %w", methodCtx, err)
	}
	meta.addQuote(output)
	candidate := domain.TradeOpportunity{
		ID:              "barter:" + recipe.ID,
		Kind:            domain.OpportunityContactBarter,
		InputCost:       cost,
		ExpectedRevenue: revenue,
		ExpectedFees:    fees,
		Confidence:      meta.confidence,
		LiquidityScore:  output.LiquidityScore,
		PriceVolatility: meta.volatility,
		RequiredSlots:   max(1, len(ingredients)),
		QuoteObservedAt: meta.observedAt,
		ResaleKnown:     output.ResaleKnown,
		ResultItemID:    recipe.ResultItem,
		ResultQuantity:  recipe.ResultCount,
		ExpiresAt:       f.expiresAt(meta.observedAt),
		Steps:           steps,
	}
	return complete(candidate)
}

func (f *Finder) multistepOpportunity(
	root domain.BarterRecipe,
	asOf time.Time,
	quotes map[string]domain.TradeQuote,
	recipesByResult map[string][]domain.BarterRecipe,
	expansions *int,
) (domain.TradeOpportunity, error) {
	const methodCtx = "opportunity.Finder.multistepOpportunity"

	output, ok := quotes[root.ResultItem]
	if !ok {
		return domain.TradeOpportunity{}, errUnavailable
	}
	plan, err := f.acquireWithRecipe(
		root,
		root.ResultCount,
		f.config.MaxMultistepDepth,
		asOf,
		quotes,
		recipesByResult,
		map[string]bool{},
		expansions,
	)
	if err != nil {
		return domain.TradeOpportunity{}, fmt.Errorf("%s: не удалось построить план получения результата: %w", methodCtx, err)
	}
	if plan.recipeCount < 2 {
		return domain.TradeOpportunity{}, errUnavailable
	}
	revenue, fees, err := saleFinancials(output, root.ResultCount)
	if err != nil {
		return domain.TradeOpportunity{}, fmt.Errorf("%s: не удалось рассчитать параметры продажи: %w", methodCtx, err)
	}
	plan.meta.addQuote(output)
	steps := append([]domain.TradeStep(nil), plan.steps...)
	steps = append(steps, domain.TradeStep{
		Kind:       domain.TradeStepList,
		ItemID:     root.ResultItem,
		Quantity:   root.ResultCount,
		LimitPrice: output.SalePrice,
	})
	candidate := domain.TradeOpportunity{
		ID:              "multistep:" + root.ID,
		Kind:            domain.OpportunityMultistepTrade,
		InputCost:       plan.cost,
		ExpectedRevenue: revenue,
		ExpectedFees:    fees,
		Confidence:      plan.meta.confidence,
		LiquidityScore:  output.LiquidityScore,
		PriceVolatility: plan.meta.volatility,
		RequiredSlots:   requiredSlots(steps),
		QuoteObservedAt: plan.meta.observedAt,
		ResaleKnown:     output.ResaleKnown,
		ResultItemID:    root.ResultItem,
		ResultQuantity:  root.ResultCount,
		ExpiresAt:       f.expiresAt(plan.meta.observedAt),
		Steps:           steps,
	}
	return complete(candidate)
}

type acquisition struct {
	cost        int64
	steps       []domain.TradeStep
	meta        acquisitionMeta
	recipeCount int
	signature   string
}

type acquisitionMeta struct {
	confidence float64
	observedAt time.Time
	volatility float64
	quoteCount int
}

func (m *acquisitionMeta) addQuote(quote domain.TradeQuote) {
	if m.quoteCount == 0 {
		m.confidence = quote.Confidence
		m.observedAt = quote.ObservedAt
		m.volatility = quote.PriceVolatility
		m.quoteCount = 1
		return
	}
	m.confidence = min(m.confidence, quote.Confidence)
	m.observedAt = oldestObservation(m.observedAt, quote.ObservedAt)
	m.volatility = max(m.volatility, quote.PriceVolatility)
	m.quoteCount++
}

func (m *acquisitionMeta) merge(other acquisitionMeta) {
	if other.quoteCount == 0 {
		return
	}
	if m.quoteCount == 0 {
		*m = other
		return
	}
	m.confidence = min(m.confidence, other.confidence)
	m.observedAt = oldestObservation(m.observedAt, other.observedAt)
	m.volatility = max(m.volatility, other.volatility)
	m.quoteCount += other.quoteCount
}

func (f *Finder) bestAcquisition(
	itemID string,
	quantity int64,
	depth int,
	asOf time.Time,
	quotes map[string]domain.TradeQuote,
	recipesByResult map[string][]domain.BarterRecipe,
	visiting map[string]bool,
	expansions *int,
) (acquisition, error) {
	const methodCtx = "opportunity.Finder.bestAcquisition"

	var candidates []acquisition
	if quote, ok := quotes[itemID]; ok {
		cost, err := money.Multiply(quote.PurchasePrice, quantity)
		if err != nil {
			return acquisition{}, fmt.Errorf("%s: не удалось рассчитать рыночную покупку %q: %w", methodCtx, itemID, err)
		}
		meta := acquisitionMeta{}
		meta.addQuote(quote)
		candidates = append(candidates, acquisition{
			cost: cost,
			steps: []domain.TradeStep{{
				Kind:       domain.TradeStepBuy,
				ItemID:     itemID,
				Quantity:   quantity,
				LimitPrice: quote.PurchasePrice,
			}},
			meta:      meta,
			signature: fmt.Sprintf("M(%s:%d)", itemID, quantity),
		})
	}
	if depth > 0 {
		for _, recipe := range recipesByResult[itemID] {
			if visiting[recipe.ID] || !recipeAvailable(recipe, asOf) {
				continue
			}
			candidate, err := f.acquireWithRecipe(
				recipe,
				quantity,
				depth,
				asOf,
				quotes,
				recipesByResult,
				visiting,
				expansions,
			)
			if err == nil {
				candidates = append(candidates, candidate)
			} else if !errors.Is(err, errUnavailable) {
				return acquisition{}, fmt.Errorf("%s: не удалось построить дочерний план получения: %w", methodCtx, err)
			}
		}
	}
	if len(candidates) == 0 {
		return acquisition{}, errUnavailable
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].cost != candidates[j].cost {
			return candidates[i].cost < candidates[j].cost
		}
		return candidates[i].signature < candidates[j].signature
	})
	return candidates[0], nil
}

func (f *Finder) acquireWithRecipe(
	recipe domain.BarterRecipe,
	quantity int64,
	depth int,
	asOf time.Time,
	quotes map[string]domain.TradeQuote,
	recipesByResult map[string][]domain.BarterRecipe,
	visiting map[string]bool,
	expansions *int,
) (acquisition, error) {
	const methodCtx = "opportunity.Finder.acquireWithRecipe"

	if depth <= 0 || visiting[recipe.ID] || !recipeAvailable(recipe, asOf) {
		return acquisition{}, errUnavailable
	}
	*expansions++
	if *expansions > f.config.MaxRecipeExpansions {
		return acquisition{}, fmt.Errorf(
			"%s: превышен лимит раскрытия рецептов %d",
			methodCtx,
			f.config.MaxRecipeExpansions,
		)
	}
	runs, err := money.CeilDivPositive(quantity, recipe.ResultCount)
	if err != nil {
		return acquisition{}, fmt.Errorf("%s: не удалось рассчитать число выполнений рецепта %q: %w", methodCtx, recipe.ID, err)
	}
	ingredients, err := aggregateIngredients(recipe.Ingredients, runs)
	if err != nil {
		return acquisition{}, fmt.Errorf("%s: не удалось агрегировать ингредиенты рецепта %q: %w", methodCtx, recipe.ID, err)
	}
	visiting[recipe.ID] = true
	defer delete(visiting, recipe.ID)

	result := acquisition{signature: "R(" + recipe.ID}
	signatures := make([]string, 0, len(ingredients))
	for _, ingredient := range ingredients {
		child, err := f.bestAcquisition(
			ingredient.ItemID,
			ingredient.Quantity,
			depth-1,
			asOf,
			quotes,
			recipesByResult,
			visiting,
			expansions,
		)
		if err != nil {
			return acquisition{}, fmt.Errorf("%s: не удалось получить ингредиент %q: %w", methodCtx, ingredient.ItemID, err)
		}
		result.cost, err = money.Add(result.cost, child.cost)
		if err != nil {
			return acquisition{}, fmt.Errorf("%s: переполнение входной стоимости рецепта %q: %w", methodCtx, recipe.ID, err)
		}
		result.steps = append(result.steps, child.steps...)
		result.meta.merge(child.meta)
		result.recipeCount += child.recipeCount
		signatures = append(signatures, child.signature)
	}
	result.steps = append(result.steps, domain.TradeStep{
		Kind:     domain.TradeStepBarter,
		ItemID:   recipe.ResultItem,
		RecipeID: recipe.ID,
		Quantity: runs,
	})
	result.recipeCount++
	result.signature += ":" + strings.Join(signatures, ",") + fmt.Sprintf(")*%d", runs)
	return result, nil
}

func aggregateIngredients(
	ingredients []domain.BarterIngredient,
	multiplier int64,
) ([]domain.BarterIngredient, error) {
	const methodCtx = "opportunity.aggregateIngredients"

	quantities := make(map[string]int64, len(ingredients))
	for _, ingredient := range ingredients {
		quantity, err := money.Multiply(ingredient.Quantity, multiplier)
		if err != nil {
			return nil, fmt.Errorf("%s: переполнение количества ингредиента %q: %w", methodCtx, ingredient.ItemID, err)
		}
		quantities[ingredient.ItemID], err = money.Add(quantities[ingredient.ItemID], quantity)
		if err != nil {
			return nil, fmt.Errorf("%s: переполнение суммарного количества ингредиента %q: %w", methodCtx, ingredient.ItemID, err)
		}
	}
	ids := make([]string, 0, len(quantities))
	for itemID := range quantities {
		ids = append(ids, itemID)
	}
	sort.Strings(ids)
	result := make([]domain.BarterIngredient, 0, len(ids))
	for _, itemID := range ids {
		result = append(result, domain.BarterIngredient{
			ItemID:   itemID,
			Quantity: quantities[itemID],
		})
	}
	return result, nil
}

func saleFinancials(quote domain.TradeQuote, quantity int64) (int64, int64, error) {
	const methodCtx = "opportunity.saleFinancials"

	revenue, err := money.Multiply(quote.SalePrice, quantity)
	if err != nil {
		return 0, 0, fmt.Errorf("%s: переполнение выручки от предмета %q: %w", methodCtx, quote.ItemID, err)
	}
	commission, err := money.Multiply(quote.SaleCommission, quantity)
	if err != nil {
		return 0, 0, fmt.Errorf("%s: переполнение комиссии продажи предмета %q: %w", methodCtx, quote.ItemID, err)
	}
	fees, err := money.Add(commission, quote.ListingFee)
	if err != nil {
		return 0, 0, fmt.Errorf("%s: переполнение сборов продажи предмета %q: %w", methodCtx, quote.ItemID, err)
	}
	return revenue, fees, nil
}

func complete(value domain.TradeOpportunity) (domain.TradeOpportunity, error) {
	const methodCtx = "opportunity.complete"

	result, err := economy.Complete(value)
	if err != nil {
		return domain.TradeOpportunity{}, fmt.Errorf("%s: не удалось завершить расчёт возможности: %w", methodCtx, err)
	}
	return result, nil
}

func (f *Finder) expiresAt(observedAt time.Time) time.Time {
	if observedAt.IsZero() || f.config.QuoteTTL <= 0 {
		return time.Time{}
	}
	return observedAt.Add(f.config.QuoteTTL)
}

func recipeAvailable(recipe domain.BarterRecipe, asOf time.Time) bool {
	return asOf.IsZero() || recipe.AvailableAt.IsZero() || !recipe.AvailableAt.After(asOf)
}

func requiredSlots(steps []domain.TradeStep) int {
	items := make(map[string]struct{})
	for _, step := range steps {
		if step.Kind == domain.TradeStepBuy {
			items[step.ItemID] = struct{}{}
		}
	}
	return max(1, len(items))
}

func oldestObservation(left, right time.Time) time.Time {
	if left.IsZero() || right.IsZero() {
		return time.Time{}
	}
	if left.Before(right) {
		return left
	}
	return right
}

func finiteUnit(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value >= 0 && value <= 1
}

func finiteNonNegative(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value >= 0
}

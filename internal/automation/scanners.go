package automation

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	appconfig "github.com/arena-trading-agent/arena-trading-agent/internal/config"
	"github.com/arena-trading-agent/arena-trading-agent/internal/domain"
	"github.com/arena-trading-agent/arena-trading-agent/internal/navigation"
	"github.com/arena-trading-agent/arena-trading-agent/internal/repository"
)

const (
	recordKindAccount = "account"
	recordKindRecipe  = "barter_recipe"
)

// ScanReport is persisted in the engine snapshot and exposed to dashboard.
type ScanReport struct {
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
	Scanned    int       `json:"scanned"`
	Errors     []string  `json:"errors,omitempty"`
}

type MarketScanner struct {
	router        RouteNavigator
	store         repository.Store
	items         []marketScanItem
	minConfidence float64
}

type marketScanItem struct {
	ID           string
	Name         string
	MaxBuyPrice  int64
	MinSalePrice int64
}

// NewMarketScanner сохраняет совместимость со старым вызовом, который
// сканирует только watchlist. Production-runtime должен использовать
// NewMarketScannerWithRecipes, чтобы включить зависимости обменов.
func NewMarketScanner(
	router RouteNavigator,
	store repository.Store,
	items []appconfig.WatchItem,
	minConfidence float64,
) (*MarketScanner, error) {
	const methodCtx = "automation.NewMarketScanner"

	result, err := NewMarketScannerWithRecipes(
		router,
		store,
		items,
		nil,
		minConfidence,
	)
	if err != nil {
		return nil, fmt.Errorf("%s: не удалось создать сканер без рецептов: %w", methodCtx, err)
	}
	return result, nil
}

// NewMarketScannerWithRecipes строит детерминированное множество всех цен,
// необходимых стратегии: watchlist, ингредиенты и результаты обменов.
func NewMarketScannerWithRecipes(
	router RouteNavigator,
	store repository.Store,
	items []appconfig.WatchItem,
	recipes []appconfig.Recipe,
	minConfidence float64,
) (*MarketScanner, error) {
	const methodCtx = "automation.NewMarketScannerWithRecipes"

	if router == nil || store == nil {
		return nil, fmt.Errorf("%s: сканеру рынка требуются маршрутизатор и репозиторий", methodCtx)
	}
	if !finiteConfidence(minConfidence) || minConfidence <= 0 {
		return nil, fmt.Errorf(
			"%s: минимальная уверенность %.3f должна быть в диапазоне (0, 1]",
			methodCtx,
			minConfidence,
		)
	}
	targets, err := marketScanItems(items, recipes)
	if err != nil {
		return nil, fmt.Errorf("%s: не удалось собрать список предметов: %w", methodCtx, err)
	}
	return &MarketScanner{
		router: router, store: store, items: targets,
		minConfidence: minConfidence,
	}, nil
}

func (s *MarketScanner) Scan(
	ctx context.Context,
	agentID string,
	sessionID string,
) ScanReport {
	const methodCtx = "automation.MarketScanner.Scan"

	report := ScanReport{StartedAt: time.Now().UTC()}
	defer func() { report.FinishedAt = time.Now().UTC() }()
	if ctx == nil {
		report.Errors = append(report.Errors, fmt.Sprintf("%s: контекст не задан", methodCtx))
		return report
	}
	for _, item := range s.items {
		if err := ctx.Err(); err != nil {
			report.Errors = append(report.Errors, fmt.Sprintf("%s: контекст завершён: %v", methodCtx, err))
			return report
		}
		if _, err := s.scanItem(ctx, agentID, sessionID, item); err != nil {
			report.Errors = append(report.Errors, fmt.Sprintf("%s: предмет %s: %v", methodCtx, item.ID, err))
			return report
		}
		report.Scanned++
	}
	return report
}

func (s *MarketScanner) ScanItem(
	ctx context.Context,
	agentID string,
	sessionID string,
	item appconfig.WatchItem,
) (domain.TradeQuote, error) {
	const methodCtx = "automation.MarketScanner.ScanItem"

	target, err := marketScanItemFromWatch(item)
	if err != nil {
		return domain.TradeQuote{}, fmt.Errorf("%s: некорректный предмет: %w", methodCtx, err)
	}
	quote, err := s.scanItem(ctx, agentID, sessionID, target)
	if err != nil {
		return domain.TradeQuote{}, fmt.Errorf("%s: сканирование предмета %q завершилось ошибкой: %w", methodCtx, target.ID, err)
	}
	return quote, nil
}

func (s *MarketScanner) scanItem(
	ctx context.Context,
	agentID string,
	sessionID string,
	item marketScanItem,
) (domain.TradeQuote, error) {
	const methodCtx = "automation.MarketScanner.scanItem"

	if ctx == nil {
		return domain.TradeQuote{}, fmt.Errorf("%s: контекст не задан", methodCtx)
	}
	if err := ctx.Err(); err != nil {
		return domain.TradeQuote{}, fmt.Errorf("%s: контекст завершён: %w", methodCtx, err)
	}
	variables := map[string]string{
		"item.id": item.ID, "item.name": item.Name,
		"item.max_buy_price":  strconv.FormatInt(item.MaxBuyPrice, 10),
		"item.min_sale_price": strconv.FormatInt(item.MinSalePrice, 10),
	}
	if _, err := s.router.Navigate(
		ctx,
		agentID,
		sessionID,
		domain.StateMarketHome,
		variables,
	); err != nil {
		return domain.TradeQuote{}, fmt.Errorf("%s: не удалось открыть рынок: %w", methodCtx, err)
	}
	results, err := s.router.Navigate(
		ctx,
		agentID,
		sessionID,
		domain.StateMarketResults,
		variables,
	)
	if err != nil {
		return domain.TradeQuote{}, fmt.Errorf("%s: не удалось найти предмет: %w", methodCtx, err)
	}
	if err := validateMarketObservation(
		results,
		domain.StateMarketResults,
		s.minConfidence,
	); err != nil {
		return domain.TradeQuote{}, fmt.Errorf("%s: результаты рынка не прошли проверку: %w", methodCtx, err)
	}
	card, err := s.router.Navigate(
		ctx,
		agentID,
		sessionID,
		domain.StateItemCard,
		variables,
	)
	if err != nil {
		return domain.TradeQuote{}, fmt.Errorf("%s: не удалось открыть карточку предмета: %w", methodCtx, err)
	}
	if err := validateMarketObservation(
		card,
		domain.StateItemCard,
		s.minConfidence,
	); err != nil {
		return domain.TradeQuote{}, fmt.Errorf("%s: карточка предмета не прошла проверку: %w", methodCtx, err)
	}
	if card.Frame.ID <= results.Frame.ID ||
		card.Frame.CapturedAt.Before(results.Frame.CapturedAt) ||
		card.Observation.CreatedAt.Before(results.Observation.CreatedAt) {
		return domain.TradeQuote{}, fmt.Errorf(
			"%s: карточка предмета не новее результатов рынка: кадры %d и %d",
			methodCtx,
			results.Frame.ID,
			card.Frame.ID,
		)
	}
	observation, err := mergeMarketObservations(results.Observation, card.Observation)
	if err != nil {
		return domain.TradeQuote{}, fmt.Errorf("%s: не удалось объединить наблюдения рынка: %w", methodCtx, err)
	}
	quote, err := tradeQuoteFromObservation(item.ID, observation, s.minConfidence)
	if err != nil {
		return domain.TradeQuote{}, fmt.Errorf("%s: не удалось построить котировку: %w", methodCtx, err)
	}
	if err := s.store.WithinTransaction(ctx, func(store repository.Store) error {
		const methodCtx = "automation.MarketScanner.scanItem.transaction"

		enrichedQuote, err := tradeQuoteWithMarketMetrics(ctx, store, quote)
		if err != nil {
			return fmt.Errorf("%s: не удалось рассчитать рыночные метрики: %w", methodCtx, err)
		}
		quote = enrichedQuote
		if err := store.SaveTradeQuote(ctx, quote); err != nil {
			return fmt.Errorf("%s: не удалось сохранить полную котировку: %w", methodCtx, err)
		}
		if err := store.SaveQuote(ctx, domain.MarketQuote{
			ItemID: item.ID, BuyPrice: quote.PurchasePrice, SalePrice: quote.SalePrice,
			ObservedAt: quote.ObservedAt, Confidence: quote.Confidence,
		}); err != nil {
			return fmt.Errorf("%s: не удалось сохранить рыночную котировку: %w", methodCtx, err)
		}
		return nil
	}); err != nil {
		return domain.TradeQuote{}, fmt.Errorf("%s: транзакция сохранения котировки завершилась ошибкой: %w", methodCtx, err)
	}
	return quote, nil
}

func marketScanItems(
	watchlist []appconfig.WatchItem,
	recipes []appconfig.Recipe,
) ([]marketScanItem, error) {
	const methodCtx = "automation.marketScanItems"

	items := make(map[string]marketScanItem, len(watchlist))
	add := func(candidate marketScanItem, source string) error {
		const methodCtx = "automation.marketScanItems.add"

		candidate.ID = strings.TrimSpace(candidate.ID)
		candidate.Name = strings.TrimSpace(candidate.Name)
		if candidate.ID == "" {
			return fmt.Errorf("%s: источник %s содержит пустой идентификатор предмета", methodCtx, source)
		}
		if candidate.Name == "" {
			return fmt.Errorf("%s: предмет %q из источника %s не содержит имени", methodCtx, candidate.ID, source)
		}
		existing, exists := items[candidate.ID]
		if !exists {
			items[candidate.ID] = candidate
			return nil
		}
		if existing.Name != candidate.Name {
			return fmt.Errorf(
				"%s: предмет %q имеет противоречащие имена %q и %q",
				methodCtx,
				candidate.ID,
				existing.Name,
				candidate.Name,
			)
		}
		if candidate.MaxBuyPrice > 0 {
			if existing.MaxBuyPrice > 0 && existing.MaxBuyPrice != candidate.MaxBuyPrice {
				return fmt.Errorf("%s: предмет %q имеет разные лимиты покупки", methodCtx, candidate.ID)
			}
			existing.MaxBuyPrice = candidate.MaxBuyPrice
		}
		if candidate.MinSalePrice > 0 {
			if existing.MinSalePrice > 0 && existing.MinSalePrice != candidate.MinSalePrice {
				return fmt.Errorf("%s: предмет %q имеет разные лимиты продажи", methodCtx, candidate.ID)
			}
			existing.MinSalePrice = candidate.MinSalePrice
		}
		items[candidate.ID] = existing
		return nil
	}
	for index, item := range watchlist {
		if !item.Enabled {
			continue
		}
		candidate, err := marketScanItemFromWatch(item)
		if err != nil {
			return nil, fmt.Errorf("%s: некорректный предмет watchlist[%d]: %w", methodCtx, index, err)
		}
		if err := add(candidate, fmt.Sprintf("watchlist[%d]", index)); err != nil {
			return nil, fmt.Errorf("%s: не удалось добавить watchlist[%d]: %w", methodCtx, index, err)
		}
	}
	for recipeIndex, recipe := range recipes {
		if !recipe.Enabled {
			continue
		}
		if err := add(
			marketScanItem{ID: recipe.ResultItemID, Name: recipe.ResultItemName},
			fmt.Sprintf("recipes[%d].result", recipeIndex),
		); err != nil {
			return nil, fmt.Errorf("%s: не удалось добавить результат recipes[%d]: %w", methodCtx, recipeIndex, err)
		}
		for ingredientIndex, ingredient := range recipe.Ingredients {
			if err := add(
				marketScanItem{ID: ingredient.ItemID, Name: ingredient.Name},
				fmt.Sprintf("recipes[%d].ingredients[%d]", recipeIndex, ingredientIndex),
			); err != nil {
				return nil, fmt.Errorf(
					"%s: не удалось добавить ингредиент recipes[%d].ingredients[%d]: %w",
					methodCtx,
					recipeIndex,
					ingredientIndex,
					err,
				)
			}
		}
	}
	result := make([]marketScanItem, 0, len(items))
	for _, item := range items {
		result = append(result, item)
	}
	sort.Slice(result, func(left, right int) bool {
		return result[left].ID < result[right].ID
	})
	return result, nil
}

func marketScanItemFromWatch(item appconfig.WatchItem) (marketScanItem, error) {
	const methodCtx = "automation.marketScanItemFromWatch"

	result := marketScanItem{
		ID: strings.TrimSpace(item.ID), Name: strings.TrimSpace(item.Name),
		MaxBuyPrice: item.MaxBuyPrice, MinSalePrice: item.MinSalePrice,
	}
	if result.ID == "" {
		return marketScanItem{}, fmt.Errorf("%s: идентификатор предмета не задан", methodCtx)
	}
	if result.Name == "" {
		return marketScanItem{}, fmt.Errorf("%s: имя предмета %q не задано", methodCtx, result.ID)
	}
	if result.MaxBuyPrice < 0 || result.MinSalePrice < 0 {
		return marketScanItem{}, fmt.Errorf("%s: лимиты предмета %q не могут быть отрицательными", methodCtx, result.ID)
	}
	return result, nil
}

func validateMarketObservation(
	result navigation.Result,
	expected domain.ScreenState,
	minConfidence float64,
) error {
	const methodCtx = "automation.validateMarketObservation"

	if result.Observation.State != expected {
		return fmt.Errorf(
			"%s: ожидалось состояние %s, получено %s",
			methodCtx,
			expected,
			result.Observation.State,
		)
	}
	if !finiteConfidence(result.Observation.Confidence) ||
		result.Observation.Confidence < minConfidence {
		return fmt.Errorf(
			"%s: уверенность наблюдения %.3f ниже %.3f",
			methodCtx,
			result.Observation.Confidence,
			minConfidence,
		)
	}
	if result.Frame.ID == 0 || result.Observation.FrameID != result.Frame.ID {
		return fmt.Errorf(
			"%s: идентификаторы кадра %d и наблюдения %d не согласованы",
			methodCtx,
			result.Frame.ID,
			result.Observation.FrameID,
		)
	}
	if result.Frame.CapturedAt.IsZero() || result.Observation.CreatedAt.IsZero() {
		return fmt.Errorf("%s: отсутствует временная метка кадра или наблюдения", methodCtx)
	}
	if result.Observation.CreatedAt.Before(result.Frame.CapturedAt) {
		return fmt.Errorf("%s: наблюдение создано раньше кадра", methodCtx)
	}
	if strings.TrimSpace(result.Frame.Encoding) == "" || len(result.Frame.Data) == 0 {
		return fmt.Errorf("%s: кадр не содержит закодированного изображения", methodCtx)
	}
	if result.Observation.Values == nil {
		return fmt.Errorf("%s: наблюдение не содержит карту значений", methodCtx)
	}
	return nil
}

func mergeMarketObservations(
	results domain.Observation,
	card domain.Observation,
) (domain.Observation, error) {
	const methodCtx = "automation.mergeMarketObservations"

	for _, key := range []string{valuePurchasePrice, valueSalePrice} {
		if _, exists := results.Values[key]; !exists {
			return domain.Observation{}, fmt.Errorf(
				"%s: результаты рынка не содержат обязательное значение %q",
				methodCtx,
				key,
			)
		}
	}
	for _, key := range []string{valueSaleCommission, valueListingFee} {
		if _, exists := card.Values[key]; !exists {
			return domain.Observation{}, fmt.Errorf(
				"%s: карточка предмета не содержит обязательное значение %q",
				methodCtx,
				key,
			)
		}
	}
	merged := domain.Observation{
		FrameID: card.FrameID, State: card.State,
		Elements:   append([]domain.UIElement(nil), card.Elements...),
		Values:     make(map[string]domain.Value, len(results.Values)+len(card.Values)),
		Confidence: min(results.Confidence, card.Confidence),
		// Котировка считается наблюдавшейся в момент самого старого входа,
		// чтобы проверка устаревания не получала искусственно свежую дату.
		CreatedAt: results.CreatedAt,
	}
	for key, value := range results.Values {
		merged.Values[key] = value
	}
	for key, value := range card.Values {
		if previous, exists := merged.Values[key]; exists {
			previousText := normalizedObservedText(previous)
			valueText := normalizedObservedText(value)
			if previousText != valueText {
				return domain.Observation{}, fmt.Errorf(
					"%s: значение %q изменилось между результатами %q и карточкой %q",
					methodCtx,
					key,
					previousText,
					valueText,
				)
			}
			value.Confidence = min(previous.Confidence, value.Confidence)
		}
		merged.Values[key] = value
	}
	return merged, nil
}

func normalizedObservedText(value domain.Value) string {
	normalized := strings.TrimSpace(value.Normalized)
	if normalized != "" {
		return normalized
	}
	return strings.TrimSpace(value.Raw)
}

type ContactScanner struct {
	router        RouteNavigator
	store         repository.Store
	recipes       []appconfig.Recipe
	minConfidence float64
}

func NewContactScanner(
	router RouteNavigator,
	store repository.Store,
	recipes []appconfig.Recipe,
	minConfidence float64,
) (*ContactScanner, error) {
	const methodCtx = "automation.NewContactScanner"

	if router == nil || store == nil {
		return nil, fmt.Errorf("%s: сканеру контактов требуются маршрутизатор и репозиторий", methodCtx)
	}
	return &ContactScanner{
		router: router, store: store, recipes: append([]appconfig.Recipe(nil), recipes...),
		minConfidence: minConfidence,
	}, nil
}

func (s *ContactScanner) Scan(
	ctx context.Context,
	agentID string,
	sessionID string,
) ScanReport {
	const methodCtx = "automation.ContactScanner.Scan"

	report := ScanReport{StartedAt: time.Now().UTC()}
	defer func() { report.FinishedAt = time.Now().UTC() }()
	for _, configured := range s.recipes {
		if !configured.Enabled {
			continue
		}
		if err := s.scanRecipe(ctx, agentID, sessionID, configured); err != nil {
			report.Errors = append(report.Errors, fmt.Sprintf("%s: рецепт %s: %v", methodCtx, configured.ID, err))
			return report
		}
		report.Scanned++
	}
	return report
}

func (s *ContactScanner) scanRecipe(
	ctx context.Context,
	agentID string,
	sessionID string,
	configured appconfig.Recipe,
) error {
	const methodCtx = "automation.ContactScanner.scanRecipe"

	variables := map[string]string{
		"recipe.id":  configured.ID,
		"contact.id": configured.ContactID, "contact.name": configured.ContactName,
		"result.id": configured.ResultItemID, "result.name": configured.ResultItemName,
	}
	if _, err := s.router.Navigate(ctx, agentID, sessionID, domain.StateContacts, variables); err != nil {
		return fmt.Errorf("%s: не удалось открыть контакты: %w", methodCtx, err)
	}
	result, err := s.router.Navigate(ctx, agentID, sessionID, domain.StateBarterCard, variables)
	if err != nil {
		return fmt.Errorf("%s: не удалось открыть рецепт: %w", methodCtx, err)
	}
	observedResult, confidence, err := requiredInt(
		result.Observation,
		valueResultQuantity,
		1,
		1_000_000,
	)
	if err != nil {
		return fmt.Errorf("%s: не удалось прочитать количество результата: %w", methodCtx, err)
	}
	if observedResult != configured.ResultCount {
		return fmt.Errorf(
			"%s: количество результата %d, ожидалось %d",
			methodCtx,
			observedResult,
			configured.ResultCount,
		)
	}
	ingredients := make([]domain.BarterIngredient, 0, len(configured.Ingredients))
	for _, ingredient := range configured.Ingredients {
		key := "ingredient." + ingredient.ItemID + ".quantity"
		quantity, valueConfidence, err := requiredInt(result.Observation, key, 1, 1_000_000)
		if err != nil {
			return fmt.Errorf("%s: не удалось прочитать ингредиент %q: %w", methodCtx, ingredient.ItemID, err)
		}
		if quantity != ingredient.Quantity {
			return fmt.Errorf("%s: значение %s равно %d, ожидалось %d", methodCtx, key, quantity, ingredient.Quantity)
		}
		confidence = min(confidence, valueConfidence)
		ingredients = append(ingredients, domain.BarterIngredient{
			ItemID: ingredient.ItemID, Quantity: quantity,
		})
	}
	cooldown, cooldownConfidence, err := requiredInt(
		result.Observation,
		valueCooldownSeconds,
		0,
		int64((30*24*time.Hour)/time.Second),
	)
	if err != nil {
		return fmt.Errorf("%s: не удалось прочитать время ожидания обмена: %w", methodCtx, err)
	}
	confidence = min(confidence, cooldownConfidence, result.Observation.Confidence)
	if confidence < s.minConfidence {
		return fmt.Errorf(
			"%s: уверенность рецепта %.3f ниже %.3f",
			methodCtx,
			confidence,
			s.minConfidence,
		)
	}
	recipe := domain.BarterRecipe{
		ID: configured.ID, ContactID: configured.ContactID, Ingredients: ingredients,
		ResultItem: configured.ResultItemID, ResultCount: configured.ResultCount,
	}
	if cooldown > 0 {
		recipe.AvailableAt = result.Observation.CreatedAt.Add(time.Duration(cooldown) * time.Second)
	}
	if err := saveJSON(ctx, s.store, "recipe/"+recipe.ID, recordKindRecipe, recipe, result.Observation.CreatedAt); err != nil {
		return fmt.Errorf("%s: не удалось сохранить рецепт: %w", methodCtx, err)
	}
	return nil
}

type AccountScanner struct {
	router        RouteNavigator
	store         repository.Store
	minConfidence float64
	trackedItems  []string
}

func NewAccountScanner(
	router RouteNavigator,
	store repository.Store,
	minConfidence float64,
	trackedItems []string,
) (*AccountScanner, error) {
	const methodCtx = "automation.NewAccountScanner"

	if router == nil || store == nil {
		return nil, fmt.Errorf("%s: сканеру аккаунта требуются маршрутизатор и репозиторий", methodCtx)
	}
	items := append([]string(nil), trackedItems...)
	return &AccountScanner{
		router: router, store: store, minConfidence: minConfidence,
		trackedItems: items,
	}, nil
}

func (s *AccountScanner) Scan(
	ctx context.Context,
	agentID string,
	sessionID string,
) (AccountSnapshot, error) {
	const methodCtx = "automation.AccountScanner.Scan"

	mainMenu, err := s.router.Navigate(ctx, agentID, sessionID, domain.StateMainMenu, nil)
	if err != nil {
		return AccountSnapshot{}, fmt.Errorf("%s: не удалось синхронизировать главное меню: %w", methodCtx, err)
	}
	balance, balanceConfidence, err := requiredInt(
		mainMenu.Observation,
		valueBalance,
		0,
		maximumObservedMoney,
	)
	if err != nil {
		return AccountSnapshot{}, fmt.Errorf("%s: не удалось прочитать баланс: %w", methodCtx, err)
	}
	inventory, err := s.router.Navigate(ctx, agentID, sessionID, domain.StateInventory, nil)
	if err != nil {
		return AccountSnapshot{}, fmt.Errorf("%s: не удалось синхронизировать инвентарь: %w", methodCtx, err)
	}
	freeInventory, inventoryConfidence, err := requiredInt(
		inventory.Observation,
		valueFreeInventorySlots,
		0,
		1_000_000,
	)
	if err != nil {
		return AccountSnapshot{}, fmt.Errorf("%s: не удалось прочитать свободные слоты инвентаря: %w", methodCtx, err)
	}
	inventoryItems := make([]domain.InventoryItem, 0, len(s.trackedItems))
	usedInventorySlots := 0
	for _, itemID := range s.trackedItems {
		item, itemConfidence, err := func() (domain.InventoryItem, float64, error) {
			observed, err := readObservedItem(inventory.Observation, itemID)
			if err != nil {
				return domain.InventoryItem{}, 0, err
			}
			return domain.InventoryItem{
				ItemID: itemID, Quantity: observed.Quantity, Slots: observed.Slots,
			}, observed.Confidence, nil
		}()
		if err != nil {
			return AccountSnapshot{}, fmt.Errorf("%s: не удалось прочитать предмет %q: %w", methodCtx, itemID, err)
		}
		inventoryConfidence = min(inventoryConfidence, itemConfidence)
		if item.Quantity == 0 {
			continue
		}
		inventoryItems = append(inventoryItems, item)
		usedInventorySlots += item.Slots
	}
	market, err := s.router.Navigate(ctx, agentID, sessionID, domain.StateMarketHome, nil)
	if err != nil {
		return AccountSnapshot{}, fmt.Errorf("%s: не удалось синхронизировать рынок: %w", methodCtx, err)
	}
	freeMarket, marketConfidence, err := requiredInt(
		market.Observation,
		valueFreeMarketSlots,
		0,
		1_000_000,
	)
	if err != nil {
		return AccountSnapshot{}, fmt.Errorf("%s: не удалось прочитать свободные рыночные слоты: %w", methodCtx, err)
	}
	confidence := min(
		balanceConfidence,
		inventoryConfidence,
		marketConfidence,
		mainMenu.Observation.Confidence,
		inventory.Observation.Confidence,
		market.Observation.Confidence,
	)
	if confidence < s.minConfidence {
		return AccountSnapshot{}, fmt.Errorf(
			"%s: уверенность снимка аккаунта %.3f ниже %.3f",
			methodCtx,
			confidence,
			s.minConfidence,
		)
	}
	value := AccountSnapshot{
		Balance: balance, FreeInventorySlots: int(freeInventory),
		FreeMarketSlots: int(freeMarket), Confidence: confidence,
		Inventory: domain.InventorySnapshot{
			CapacitySlots: usedInventorySlots + int(freeInventory),
			UsedSlots:     usedInventorySlots,
			Items:         inventoryItems,
		},
		ObservedAt: market.Observation.CreatedAt,
	}
	if err := saveJSON(
		ctx,
		s.store,
		"account/latest",
		recordKindAccount,
		value,
		value.ObservedAt,
	); err != nil {
		return AccountSnapshot{}, fmt.Errorf("%s: не удалось сохранить снимок аккаунта: %w", methodCtx, err)
	}
	return value, nil
}

func saveJSON(
	ctx context.Context,
	store repository.Store,
	key string,
	kind string,
	value any,
	at time.Time,
) error {
	const methodCtx = "automation.saveJSON"

	payload, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("%s: не удалось сериализовать запись %s: %w", methodCtx, key, err)
	}
	if err := store.SaveRuntimeRecord(ctx, domain.RuntimeRecord{
		Key: key, Kind: kind, Payload: payload, UpdatedAt: at,
	}); err != nil {
		return fmt.Errorf("%s: не удалось сохранить runtime-запись %s: %w", methodCtx, key, err)
	}
	return nil
}

func loadJSON[T any](
	ctx context.Context,
	store repository.Store,
	key string,
) (T, error) {
	const methodCtx = "automation.loadJSON"

	var value T
	record, err := store.RuntimeRecord(ctx, key)
	if err != nil {
		return value, fmt.Errorf("%s: не удалось получить runtime-запись %q: %w", methodCtx, key, err)
	}
	if err := json.Unmarshal(record.Payload, &value); err != nil {
		return value, fmt.Errorf("%s: не удалось декодировать runtime-запись %q: %w", methodCtx, key, err)
	}
	return value, nil
}

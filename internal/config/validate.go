package config

import (
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/arena-trading-agent/arena-trading-agent/internal/domain"
	"github.com/arena-trading-agent/arena-trading-agent/internal/protocol"
)

const (
	maxWatchItems        = 10_000
	maxRecipes           = 10_000
	maxRecipeIngredients = 64
	maxItemQuantity      = 1_000_000_000
	maxTransitions       = 10_000
	maxActionsPerEdge    = 64
	maxRetries           = 10
	maxAvailableSlots    = 10_000
	minScannerInterval   = 250 * time.Millisecond
	maxScannerInterval   = 24 * time.Hour
	maxScannerStaleness  = 7 * 24 * time.Hour
	minVerifyTimeout     = 100 * time.Millisecond
	maxVerifyTimeout     = 60 * time.Second
	minQuoteAge          = time.Second
	maxQuoteAge          = 24 * time.Hour
	maxScrollDelta       = 120 * 100
)

var supportedStates = map[domain.ScreenState]struct{}{
	domain.StateUnknown:        {},
	domain.StateMainMenu:       {},
	domain.StateMarketHome:     {},
	domain.StateMarketSearch:   {},
	domain.StateMarketResults:  {},
	domain.StateItemCard:       {},
	domain.StatePurchaseDialog: {},
	domain.StateContacts:       {},
	domain.StateContactPage:    {},
	domain.StateContactBarter:  {},
	domain.StateBarterCard:     {},
	domain.StateInventory:      {},
	domain.StateSaleDialog:     {},
	domain.StateConfirmation:   {},
	domain.StateErrorDialog:    {},
}

var supportedMouseButtons = map[string]struct{}{
	"LEFT": {}, "PRIMARY": {}, "RIGHT": {}, "SECONDARY": {},
	"MIDDLE": {}, "X1": {}, "X2": {},
}

var supportedNamedKeys = func() map[string]struct{} {
	result := map[string]struct{}{
		"BACKSPACE": {}, "TAB": {}, "ENTER": {}, "RETURN": {}, "SHIFT": {},
		"CTRL": {}, "CONTROL": {}, "ALT": {}, "MENU": {}, "PAUSE": {},
		"CAPSLOCK": {}, "ESC": {}, "ESCAPE": {}, "SPACE": {}, "PAGEUP": {},
		"PAGEDOWN": {}, "END": {}, "HOME": {}, "LEFT": {}, "UP": {},
		"RIGHT": {}, "DOWN": {}, "INSERT": {}, "DELETE": {}, "LWIN": {},
		"RWIN": {}, "MULTIPLY": {}, "ADD": {}, "SUBTRACT": {}, "DECIMAL": {},
		"DIVIDE": {}, "NUMLOCK": {}, "SCROLLLOCK": {}, "OEM_PLUS": {},
		"OEM_MINUS": {},
	}
	for key := '0'; key <= '9'; key++ {
		result[string(key)] = struct{}{}
	}
	for key := 'A'; key <= 'Z'; key++ {
		result[string(key)] = struct{}{}
	}
	for number := 0; number <= 9; number++ {
		result[fmt.Sprintf("NUMPAD%d", number)] = struct{}{}
	}
	for number := 1; number <= 24; number++ {
		result[fmt.Sprintf("F%d", number)] = struct{}{}
	}
	return result
}()

func (r *Runtime) validate(baseDir string) error {
	const methodCtx = "config.Runtime.validate"

	if r == nil {
		return fmt.Errorf("%s: runtime-конфигурация не задана", methodCtx)
	}
	var problems []error
	if r.Version != CurrentVersion {
		problems = append(problems, fmt.Errorf("получена версия %d, требуется %d", r.Version, CurrentVersion))
	}

	r.DetectorConfigPath = strings.TrimSpace(r.DetectorConfigPath)
	switch {
	case r.DetectorConfigPath == "":
		problems = append(problems, fmt.Errorf("поле detector_config обязательно"))
	case strings.IndexByte(r.DetectorConfigPath, 0) >= 0:
		problems = append(problems, fmt.Errorf("поле detector_config содержит байт NUL"))
	default:
		path := r.DetectorConfigPath
		if baseDir == "" {
			r.DetectorConfigPath = filepath.Clean(path)
		} else {
			if !filepath.IsAbs(path) {
				path = filepath.Join(baseDir, path)
			}
			absolute, err := filepath.Abs(path)
			if err != nil {
				problems = append(problems, fmt.Errorf("не удалось обработать detector_config: %w", err))
			} else {
				r.DetectorConfigPath = filepath.Clean(absolute)
			}
		}
	}

	problems = append(problems, validateRisk(r.Risk)...)
	problems = append(problems, r.validateWatchlist()...)
	problems = append(problems, r.validateRecipes()...)
	problems = append(problems, r.validateNavigation()...)
	problems = append(problems, validateScanner("scanners.market", r.Scanners.Market)...)
	problems = append(problems, validateScanner("scanners.contacts", r.Scanners.Contacts)...)
	problems = append(problems, validateScanner("scanners.orders", r.Scanners.Orders)...)
	problems = append(problems, validateStrategy(&r.Strategy)...)

	sort.Slice(r.Watchlist, func(left, right int) bool {
		return r.Watchlist[left].ID < r.Watchlist[right].ID
	})
	sort.Slice(r.Recipes, func(left, right int) bool {
		return r.Recipes[left].ID < r.Recipes[right].ID
	})
	sort.Slice(r.Navigation.Transitions, func(left, right int) bool {
		return r.Navigation.Transitions[left].ID < r.Navigation.Transitions[right].ID
	})
	if err := errors.Join(problems...); err != nil {
		return fmt.Errorf("%s: обнаружены ошибки проверки: %w", methodCtx, err)
	}
	return nil
}

func validateStrategy(value *Strategy) []error {
	const methodCtx = "config.validateStrategy"

	if value == nil {
		return contextualizeValidationErrors(methodCtx, []error{fmt.Errorf("конфигурация strategy обязательна")})
	}
	if *value == (Strategy{}) {
		*value = Strategy{
			ProfitWeight: 1, ROIWeight: 1, LiquidityWeight: .5,
			ConfidenceWeight: .5, RiskWeight: 1, SlotWeight: .1,
			ProfitScale: 10_000, MaxStepAttempts: 3,
			MaxMultistepDepth: 3, MaxRecipeExpansions: 10_000,
		}
	}
	var problems []error
	weights := map[string]float64{
		"profit_weight": value.ProfitWeight, "roi_weight": value.ROIWeight,
		"liquidity_weight":  value.LiquidityWeight,
		"confidence_weight": value.ConfidenceWeight,
		"risk_weight":       value.RiskWeight,
		"slot_weight":       value.SlotWeight,
	}
	positive := false
	for name, weight := range weights {
		if !finite(weight) || weight < 0 || weight > 1_000_000 {
			problems = append(problems, fmt.Errorf(
				"значение strategy.%s должно быть конечным и находиться в диапазоне 0..1000000",
				name,
			))
		}
		if weight > 0 {
			positive = true
		}
	}
	if !positive {
		problems = append(problems, fmt.Errorf("strategy должна содержать хотя бы один положительный вес"))
	}
	if !finite(value.ProfitScale) || value.ProfitScale <= 0 || value.ProfitScale > float64(MaxMoney) {
		problems = append(problems, fmt.Errorf("значение strategy.profit_scale должно быть конечным и положительным"))
	}
	if value.MaxStepAttempts < 1 || value.MaxStepAttempts > 100 {
		problems = append(problems, fmt.Errorf("значение strategy.max_step_attempts должно быть в диапазоне 1..100"))
	}
	if value.MaxMultistepDepth < 1 || value.MaxMultistepDepth > 10 {
		problems = append(problems, fmt.Errorf("значение strategy.max_multistep_depth должно быть в диапазоне 1..10"))
	}
	if value.MaxRecipeExpansions < 1 || value.MaxRecipeExpansions > 1_000_000 {
		problems = append(problems, fmt.Errorf("значение strategy.max_recipe_expansions должно быть в диапазоне 1..1000000"))
	}
	return contextualizeValidationErrors(methodCtx, problems)
}

func (r *Runtime) validateRecipes() []error {
	const methodCtx = "config.Runtime.validateRecipes"

	if len(r.Recipes) > maxRecipes {
		return contextualizeValidationErrors(methodCtx, []error{fmt.Errorf("список recipes содержит больше %d записей", maxRecipes)})
	}
	var problems []error
	seen := make(map[string]int, len(r.Recipes))
	for index := range r.Recipes {
		recipe := &r.Recipes[index]
		field := fmt.Sprintf("recipes[%d]", index)
		recipe.ID = strings.TrimSpace(recipe.ID)
		recipe.ContactID = strings.TrimSpace(recipe.ContactID)
		recipe.ContactName = strings.TrimSpace(recipe.ContactName)
		recipe.ResultItemID = strings.TrimSpace(recipe.ResultItemID)
		recipe.ResultItemName = strings.TrimSpace(recipe.ResultItemName)
		if err := validateID(recipe.ID); err != nil {
			problems = append(problems, fmt.Errorf("некорректное поле %s.id: %w", field, err))
		} else if previous, exists := seen[recipe.ID]; exists {
			problems = append(problems, fmt.Errorf("поле %s.id дублирует recipes[%d].id", field, previous))
		} else {
			seen[recipe.ID] = index
		}
		for name, value := range map[string]string{
			"contact_id": recipe.ContactID, "result_item_id": recipe.ResultItemID,
		} {
			if err := validateID(value); err != nil {
				problems = append(problems, fmt.Errorf("некорректное поле %s.%s: %w", field, name, err))
			}
		}
		if recipe.ContactName == "" || utf8.RuneCountInString(recipe.ContactName) > 256 {
			problems = append(problems, fmt.Errorf("поле %s.contact_name должно содержать от 1 до 256 символов", field))
		}
		if recipe.ResultItemName == "" || utf8.RuneCountInString(recipe.ResultItemName) > 256 {
			problems = append(problems, fmt.Errorf("поле %s.result_item_name должно содержать от 1 до 256 символов", field))
		}
		if recipe.ResultCount <= 0 || recipe.ResultCount > maxItemQuantity {
			problems = append(problems, fmt.Errorf(
				"поле %s.result_count должно быть в диапазоне 1..%d",
				field,
				maxItemQuantity,
			))
		}
		if len(recipe.Ingredients) == 0 || len(recipe.Ingredients) > maxRecipeIngredients {
			problems = append(problems, fmt.Errorf(
				"поле %s.ingredients должно содержать от 1 до %d записей",
				field,
				maxRecipeIngredients,
			))
		}
		ingredientIDs := make(map[string]int, len(recipe.Ingredients))
		for ingredientIndex := range recipe.Ingredients {
			ingredient := &recipe.Ingredients[ingredientIndex]
			ingredientField := fmt.Sprintf("%s.ingredients[%d]", field, ingredientIndex)
			ingredient.ItemID = strings.TrimSpace(ingredient.ItemID)
			ingredient.Name = strings.TrimSpace(ingredient.Name)
			if err := validateID(ingredient.ItemID); err != nil {
				problems = append(problems, fmt.Errorf("некорректное поле %s.item_id: %w", ingredientField, err))
			} else if previous, exists := ingredientIDs[ingredient.ItemID]; exists {
				problems = append(problems, fmt.Errorf(
					"поле %s.item_id дублирует ингредиент %d",
					ingredientField,
					previous,
				))
			} else {
				ingredientIDs[ingredient.ItemID] = ingredientIndex
			}
			if ingredient.Name == "" || utf8.RuneCountInString(ingredient.Name) > 256 {
				problems = append(problems, fmt.Errorf("поле %s.name должно содержать от 1 до 256 символов", ingredientField))
			}
			if ingredient.ItemID != "" && ingredient.ItemID == recipe.ResultItemID {
				problems = append(problems, fmt.Errorf(
					"поле %s.item_id должно отличаться от результата рецепта",
					ingredientField,
				))
			}
			if ingredient.Quantity <= 0 || ingredient.Quantity > maxItemQuantity {
				problems = append(problems, fmt.Errorf(
					"поле %s.quantity должно быть в диапазоне 1..%d",
					ingredientField,
					maxItemQuantity,
				))
			}
		}
		sort.Slice(recipe.Ingredients, func(left, right int) bool {
			return recipe.Ingredients[left].ItemID < recipe.Ingredients[right].ItemID
		})
	}
	return contextualizeValidationErrors(methodCtx, problems)
}

func validateRisk(value Risk) []error {
	const methodCtx = "config.validateRisk"

	var problems []error
	if value.MaxBudget <= 0 || value.MaxBudget > MaxMoney {
		problems = append(problems, fmt.Errorf("значение risk.max_budget должно быть в диапазоне 1..%d", MaxMoney))
	}
	if value.MaxItemPrice <= 0 || value.MaxItemPrice > MaxMoney {
		problems = append(problems, fmt.Errorf("значение risk.max_item_price должно быть в диапазоне 1..%d", MaxMoney))
	} else if value.MaxBudget > 0 && value.MaxItemPrice > value.MaxBudget {
		problems = append(problems, fmt.Errorf("значение risk.max_item_price не должно превышать max_budget"))
	}
	if value.MinProfit < 0 || value.MinProfit > MaxMoney {
		problems = append(problems, fmt.Errorf("значение risk.min_profit должно быть в диапазоне 0..%d", MaxMoney))
	}
	if !finite(value.MinROI) || value.MinROI < 0 || value.MinROI > 100 {
		problems = append(problems, fmt.Errorf("значение risk.min_roi должно быть конечным и находиться в диапазоне 0..100"))
	}
	if !finite(value.MinConfidence) || value.MinConfidence <= 0 || value.MinConfidence > 1 {
		problems = append(problems, fmt.Errorf("значение risk.min_confidence должно быть конечным и находиться в диапазоне (0,1]"))
	}
	if duration := value.MaxQuoteAge.Value(); duration < minQuoteAge || duration > maxQuoteAge {
		problems = append(problems, fmt.Errorf(
			"значение risk.max_quote_age должно быть в диапазоне %s..%s",
			minQuoteAge,
			maxQuoteAge,
		))
	}
	if value.AvailableSlots < 0 || value.AvailableSlots > maxAvailableSlots {
		problems = append(problems, fmt.Errorf(
			"значение risk.available_slots должно быть в диапазоне 0..%d",
			maxAvailableSlots,
		))
	}
	return contextualizeValidationErrors(methodCtx, problems)
}

func (r *Runtime) validateWatchlist() []error {
	const methodCtx = "config.Runtime.validateWatchlist"

	if len(r.Watchlist) == 0 {
		return contextualizeValidationErrors(methodCtx, []error{fmt.Errorf("список watchlist должен содержать хотя бы один предмет")})
	}
	if len(r.Watchlist) > maxWatchItems {
		return contextualizeValidationErrors(methodCtx, []error{fmt.Errorf("список watchlist содержит больше %d предметов", maxWatchItems)})
	}

	var problems []error
	seen := make(map[string]int, len(r.Watchlist))
	for index := range r.Watchlist {
		item := &r.Watchlist[index]
		item.ID = strings.TrimSpace(item.ID)
		item.Name = strings.TrimSpace(item.Name)
		field := fmt.Sprintf("watchlist[%d]", index)
		if err := validateID(item.ID); err != nil {
			problems = append(problems, fmt.Errorf("некорректное поле %s.id: %w", field, err))
		} else if previous, exists := seen[item.ID]; exists {
			problems = append(problems, fmt.Errorf(
				"поле %s.id дублирует watchlist[%d].id %q",
				field,
				previous,
				item.ID,
			))
		} else {
			seen[item.ID] = index
		}
		if item.Name == "" || utf8.RuneCountInString(item.Name) > 256 {
			problems = append(problems, fmt.Errorf("поле %s.name должно содержать от 1 до 256 символов", field))
		}
		if item.MaxBuyPrice <= 0 || item.MaxBuyPrice > MaxMoney {
			problems = append(problems, fmt.Errorf("поле %s.max_buy_price должно быть в диапазоне 1..%d", field, MaxMoney))
		} else if r.Risk.MaxItemPrice > 0 && item.MaxBuyPrice > r.Risk.MaxItemPrice {
			problems = append(problems, fmt.Errorf("поле %s.max_buy_price превышает risk.max_item_price", field))
		}
		if item.MinSalePrice <= 0 || item.MinSalePrice > MaxMoney {
			problems = append(problems, fmt.Errorf("поле %s.min_sale_price должно быть в диапазоне 1..%d", field, MaxMoney))
		}
	}
	return contextualizeValidationErrors(methodCtx, problems)
}

func (r *Runtime) validateNavigation() []error {
	const methodCtx = "config.Runtime.validateNavigation"

	transitions := r.Navigation.Transitions
	if len(transitions) == 0 {
		return contextualizeValidationErrors(methodCtx, []error{fmt.Errorf("список navigation.transitions не должен быть пустым")})
	}
	if len(transitions) > maxTransitions {
		return contextualizeValidationErrors(methodCtx, []error{fmt.Errorf("список navigation.transitions содержит больше %d записей", maxTransitions)})
	}

	var problems []error
	transitionIDs := make(map[string]int, len(transitions))
	actionIDs := make(map[string]string)
	routes := make(map[string]string)
	for index := range transitions {
		transition := &transitions[index]
		transition.ID = strings.TrimSpace(transition.ID)
		transition.From = domain.ScreenState(strings.TrimSpace(string(transition.From)))
		transition.To = domain.ScreenState(strings.TrimSpace(string(transition.To)))
		transition.Class = protocol.ActionClass(strings.ToUpper(strings.TrimSpace(string(transition.Class))))
		if transition.Class == "" {
			transition.Class = protocol.ActionNavigation
		}
		transition.Verify.State = domain.ScreenState(strings.TrimSpace(string(transition.Verify.State)))
		field := fmt.Sprintf("navigation.transitions[%d]", index)

		if err := validateID(transition.ID); err != nil {
			problems = append(problems, fmt.Errorf("некорректное поле %s.id: %w", field, err))
		} else if previous, exists := transitionIDs[transition.ID]; exists {
			problems = append(problems, fmt.Errorf(
				"поле %s.id дублирует navigation.transitions[%d].id %q",
				field,
				previous,
				transition.ID,
			))
		} else {
			transitionIDs[transition.ID] = index
		}

		if !validTransitionState(transition.From) {
			problems = append(problems, fmt.Errorf("поле %s.from содержит неподдерживаемое состояние %q", field, transition.From))
		}
		if !validTransitionState(transition.To) {
			problems = append(problems, fmt.Errorf("поле %s.to содержит неподдерживаемое состояние %q", field, transition.To))
		}
		if transition.From != "" && transition.From == transition.To {
			problems = append(problems, fmt.Errorf("переход %s должен изменять состояние экрана", field))
		}
		switch transition.Class {
		case protocol.ActionNavigation, protocol.ActionPurchase, protocol.ActionBarter,
			protocol.ActionListing, protocol.ActionReprice:
		default:
			problems = append(problems, fmt.Errorf("поле %s.class содержит неподдерживаемое значение %q", field, transition.Class))
		}
		route := string(transition.From) + "\x00" + string(transition.To)
		if previous, exists := routes[route]; exists {
			problems = append(problems, fmt.Errorf(
				"переход %s дублирует маршрут %s -> %s из перехода %q",
				field,
				transition.From,
				transition.To,
				previous,
			))
		} else {
			routes[route] = transition.ID
		}

		if len(transition.Actions) == 0 || len(transition.Actions) > maxActionsPerEdge {
			problems = append(problems, fmt.Errorf(
				"список %s.actions должен содержать от 1 до %d записей",
				field,
				maxActionsPerEdge,
			))
		}
		for actionIndex := range transition.Actions {
			action := &transition.Actions[actionIndex]
			action.ID = strings.TrimSpace(action.ID)
			action.Kind = strings.TrimSpace(action.Kind)
			actionField := fmt.Sprintf("%s.actions[%d]", field, actionIndex)
			if err := validateID(action.ID); err != nil {
				problems = append(problems, fmt.Errorf("некорректное поле %s.id: %w", actionField, err))
			} else if previous, exists := actionIDs[action.ID]; exists {
				problems = append(problems, fmt.Errorf(
					"поле %s.id дублирует идентификатор действия %q из %s",
					actionField,
					action.ID,
					previous,
				))
			} else {
				actionIDs[action.ID] = actionField
			}
			problems = append(problems, validateAction(actionField, action)...)
		}

		problems = append(problems, validateVerification(field+".verify", transition.To, transition.Verify)...)
		if transition.MaxRetry < 0 || transition.MaxRetry > maxRetries {
			problems = append(problems, fmt.Errorf("поле %s.max_retry должно быть в диапазоне 0..%d", field, maxRetries))
		}
		if transition.Class != protocol.ActionNavigation && transition.MaxRetry != 0 {
			problems = append(problems, fmt.Errorf("денежный переход %s должен иметь max_retry=0", field))
		}
		if transition.Class != protocol.ActionNavigation {
			if len(transition.Actions) != 1 {
				problems = append(problems, fmt.Errorf(
					"денежный переход %s должен содержать ровно один финальный CLICK",
					field,
				))
			} else {
				action := transition.Actions[0]
				button := strings.ToUpper(strings.TrimSpace(action.Value))
				if action.Kind != ActionClick ||
					(action.Point == nil && action.BBox == nil) ||
					(button != "LEFT" && button != "PRIMARY") {
					problems = append(problems, fmt.Errorf(
						"денежный переход %s должен содержать только CLICK LEFT/PRIMARY по откалиброванной точке",
						field,
					))
				}
			}
		}
	}
	return contextualizeValidationErrors(methodCtx, problems)
}

func validateAction(field string, value *Action) []error {
	const methodCtx = "config.validateAction"

	var problems []error
	hasPoint, hasBBox := value.Point != nil, value.BBox != nil
	if hasPoint && !validPoint(*value.Point) {
		problems = append(problems, fmt.Errorf("поле %s.point не является нормализованной точкой", field))
	}
	if hasBBox && !validBBox(*value.BBox) {
		problems = append(problems, fmt.Errorf("поле %s.bbox не является нормализованным прямоугольником", field))
	}
	if hasPoint && hasBBox {
		problems = append(problems, fmt.Errorf("действие %s не должно одновременно задавать point и bbox", field))
	}
	hasTarget := hasPoint || hasBBox

	switch value.Kind {
	case ActionMove:
		if !hasTarget {
			problems = append(problems, fmt.Errorf("действие %s MOVE требует point или bbox", field))
		}
		if value.Value != "" || value.Delta != 0 {
			problems = append(problems, fmt.Errorf("действие %s MOVE не принимает value или delta", field))
		}
	case ActionClick:
		if !hasTarget {
			problems = append(problems, fmt.Errorf("действие %s CLICK требует point или bbox", field))
		}
		value.Value = strings.ToUpper(strings.TrimSpace(value.Value))
		if value.Value == "" {
			value.Value = "LEFT"
		}
		if _, exists := supportedMouseButtons[value.Value]; !exists {
			problems = append(problems, fmt.Errorf("действие %s CLICK содержит неподдерживаемую кнопку %q", field, value.Value))
		}
		if value.Delta != 0 {
			problems = append(problems, fmt.Errorf("действие %s CLICK не принимает delta", field))
		}
	case ActionScroll:
		if !hasTarget {
			problems = append(problems, fmt.Errorf("действие %s SCROLL требует point или bbox", field))
		}
		if value.Value != "" {
			problems = append(problems, fmt.Errorf("действие %s SCROLL не принимает value", field))
		}
		if value.Delta == 0 || value.Delta < -maxScrollDelta || value.Delta > maxScrollDelta ||
			value.Delta%120 != 0 {
			problems = append(problems, fmt.Errorf(
				"delta действия %s SCROLL должна быть ненулевым кратным 120 в диапазоне %d..%d",
				field,
				-maxScrollDelta,
				maxScrollDelta,
			))
		}
	case ActionKey:
		if hasTarget || value.Delta != 0 {
			problems = append(problems, fmt.Errorf("действие %s KEY не принимает point, bbox или delta", field))
		}
		value.Value = canonicalKeyChord(value.Value)
		if err := validateKeyChord(value.Value); err != nil {
			problems = append(problems, fmt.Errorf("некорректное поле %s.value: %w", field, err))
		}
	case ActionText:
		if hasTarget || value.Delta != 0 {
			problems = append(problems, fmt.Errorf("действие %s TEXT не принимает point, bbox или delta", field))
		}
		if value.Value == "" || utf8.RuneCountInString(value.Value) > 256 ||
			strings.IndexByte(value.Value, 0) >= 0 {
			problems = append(problems, fmt.Errorf("значение действия %s TEXT должно содержать от 1 до 256 символов без NUL", field))
		}
	default:
		problems = append(problems, fmt.Errorf("поле %s содержит неподдерживаемый тип действия %q", field, value.Kind))
	}
	return contextualizeValidationErrors(methodCtx, problems)
}

func validateVerification(field string, destination domain.ScreenState, value Verification) []error {
	const methodCtx = "config.validateVerification"

	var problems []error
	if !validTransitionState(value.State) {
		problems = append(problems, fmt.Errorf("поле %s.state содержит неподдерживаемое состояние %q", field, value.State))
	} else if value.State != destination {
		problems = append(problems, fmt.Errorf("поле %s.state должно совпадать с transition.to", field))
	}
	if !finite(value.MinConfidence) || value.MinConfidence <= 0 || value.MinConfidence > 1 {
		problems = append(problems, fmt.Errorf("поле %s.min_confidence должно быть конечным и находиться в диапазоне (0,1]", field))
	}
	if timeout := value.Timeout.Value(); timeout < minVerifyTimeout || timeout > maxVerifyTimeout {
		problems = append(problems, fmt.Errorf(
			"поле %s.timeout должно быть в диапазоне %s..%s",
			field,
			minVerifyTimeout,
			maxVerifyTimeout,
		))
	}
	if value.BBox != nil {
		problems = append(problems, fmt.Errorf(
			"поле %s.bbox пока не поддерживается runtime-проверкой и должно быть пустым",
			field,
		))
	}
	return contextualizeValidationErrors(methodCtx, problems)
}

func validateScanner(field string, value ScannerTiming) []error {
	const methodCtx = "config.validateScanner"

	var problems []error
	interval := value.Interval.Value()
	staleness := value.Staleness.Value()
	if interval < minScannerInterval || interval > maxScannerInterval {
		problems = append(problems, fmt.Errorf(
			"поле %s.interval должно быть в диапазоне %s..%s",
			field,
			minScannerInterval,
			maxScannerInterval,
		))
	}
	if staleness < minScannerInterval || staleness > maxScannerStaleness {
		problems = append(problems, fmt.Errorf(
			"поле %s.staleness должно быть в диапазоне %s..%s",
			field,
			minScannerInterval,
			maxScannerStaleness,
		))
	} else if interval > 0 && staleness < interval {
		problems = append(problems, fmt.Errorf("поле %s.staleness не должно быть короче interval", field))
	}
	return contextualizeValidationErrors(methodCtx, problems)
}

func validateID(value string) error {
	const methodCtx = "config.validateID"

	if len(value) == 0 || len(value) > 128 {
		return fmt.Errorf("%s: значение должно содержать от 1 до 128 символов ASCII", methodCtx)
	}
	for index, character := range []byte(value) {
		allowed := character >= 'a' && character <= 'z' ||
			character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' ||
			(index > 0 && strings.ContainsRune("._:-", rune(character)))
		if !allowed {
			return fmt.Errorf("%s: значение содержит неподдерживаемый символ %q", methodCtx, character)
		}
	}
	return nil
}

func validateKeyChord(value string) error {
	const methodCtx = "config.validateKeyChord"

	parts := strings.Split(value, "+")
	if len(parts) == 0 || len(parts) > 4 {
		return fmt.Errorf("%s: сочетание клавиш должно содержать от 1 до 4 клавиш", methodCtx)
	}
	seen := make(map[string]struct{}, len(parts))
	for _, part := range parts {
		if part == "" {
			return fmt.Errorf("%s: сочетание клавиш содержит пустую клавишу", methodCtx)
		}
		if _, exists := supportedNamedKeys[part]; !exists {
			return fmt.Errorf("%s: неподдерживаемая клавиша %q", methodCtx, part)
		}
		if _, exists := seen[part]; exists {
			return fmt.Errorf("%s: сочетание клавиш повторяет клавишу %q", methodCtx, part)
		}
		seen[part] = struct{}{}
	}
	return nil
}

func contextualizeValidationErrors(callerCtx string, problems []error) []error {
	const methodCtx = "config.contextualizeValidationErrors"

	for index, err := range problems {
		if err != nil {
			problems[index] = fmt.Errorf("%s: вызывающий метод %s: ошибка проверки: %w", methodCtx, callerCtx, err)
		}
	}
	return problems
}

func validTransitionState(value domain.ScreenState) bool {
	_, exists := supportedStates[value]
	return exists && value != domain.StateUnknown
}

func validPoint(value domain.Point) bool {
	return finite(value.X) && finite(value.Y) &&
		value.X >= 0 && value.X <= 1 &&
		value.Y >= 0 && value.Y <= 1
}

func validBBox(value domain.Rectangle) bool {
	return finite(value.X) && finite(value.Y) &&
		finite(value.Width) && finite(value.Height) &&
		value.X >= 0 && value.Y >= 0 &&
		value.Width > 0 && value.Height > 0 &&
		value.X+value.Width <= 1 &&
		value.Y+value.Height <= 1
}

func finite(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0)
}

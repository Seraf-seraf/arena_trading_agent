package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/arena-trading-agent/arena-trading-agent/internal/domain"
)

// Memory provides a thread-safe Store for simulation and tests.
type Memory struct {
	mu      sync.RWMutex
	txState *transactionState

	observations     map[uint64]domain.Observation
	quotes           []domain.MarketQuote
	tradeQuotes      []domain.TradeQuote
	executions       map[string]domain.TradeExecution
	executionHistory map[string][]domain.TradeExecution
	actions          map[string]domain.ActionRecord
	actionResults    map[string][]domain.ActionResultRecord
	events           map[string]domain.AgentEventRecord
	runtimeRecords   map[string]domain.RuntimeRecord
}

// NewMemory creates an empty repository.
func NewMemory() *Memory {
	return &Memory{
		observations:     make(map[uint64]domain.Observation),
		executions:       make(map[string]domain.TradeExecution),
		executionHistory: make(map[string][]domain.TradeExecution),
		actions:          make(map[string]domain.ActionRecord),
		actionResults:    make(map[string][]domain.ActionResultRecord),
		events:           make(map[string]domain.AgentEventRecord),
		runtimeRecords:   make(map[string]domain.RuntimeRecord),
	}
}

// SaveObservation persists the latest normalized observation for a frame.
func (m *Memory) SaveObservation(ctx context.Context, value domain.Observation) error {
	const methodCtx = "repository.Memory.SaveObservation"

	if err := checkContext(ctx); err != nil {
		return fmt.Errorf("%s: не удалось проверить контекст: %w", methodCtx, err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := checkContext(ctx); err != nil {
		return fmt.Errorf("%s: контекст завершён во время ожидания блокировки записи: %w", methodCtx, err)
	}
	if existing, exists := m.observations[value.FrameID]; exists {
		if observationsEqual(existing, value) {
			return nil
		}
		return fmt.Errorf(
			"%s: наблюдение кадра %d конфликтует с сохранённой записью: %w",
			methodCtx,
			value.FrameID,
			conflict("наблюдение", frameIDString(value.FrameID)),
		)
	}
	m.observations[value.FrameID] = cloneObservation(value)
	return nil
}

// Observation returns an observation by frame identifier.
func (m *Memory) Observation(ctx context.Context, frameID uint64) (domain.Observation, error) {
	const methodCtx = "repository.Memory.Observation"

	if err := checkContext(ctx); err != nil {
		return domain.Observation{}, fmt.Errorf("%s: не удалось проверить контекст: %w", methodCtx, err)
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	value, ok := m.observations[frameID]
	if !ok {
		return domain.Observation{}, fmt.Errorf(
			"%s: наблюдение для кадра %d не найдено: %w",
			methodCtx,
			frameID,
			notFound("наблюдение", frameIDString(frameID)),
		)
	}
	return cloneObservation(value), nil
}

// ListObservations returns matching observations newest first.
func (m *Memory) ListObservations(ctx context.Context, filter domain.ObservationFilter) ([]domain.Observation, error) {
	const methodCtx = "repository.Memory.ListObservations"

	if err := checkContext(ctx); err != nil {
		return nil, fmt.Errorf("%s: не удалось проверить контекст: %w", methodCtx, err)
	}
	m.mu.RLock()
	values := make([]domain.Observation, 0, len(m.observations))
	for _, value := range m.observations {
		if filter.State != "" && value.State != filter.State {
			continue
		}
		if !filter.Since.IsZero() && value.CreatedAt.Before(filter.Since) {
			continue
		}
		if !filter.Until.IsZero() && value.CreatedAt.After(filter.Until) {
			continue
		}
		values = append(values, cloneObservation(value))
	}
	m.mu.RUnlock()
	sort.Slice(values, func(i, j int) bool {
		if values[i].CreatedAt.Equal(values[j].CreatedAt) {
			return values[i].FrameID > values[j].FrameID
		}
		return values[i].CreatedAt.After(values[j].CreatedAt)
	})
	start, end, err := pageBounds(len(values), filter.Limit, filter.Offset)
	if err != nil {
		return nil, fmt.Errorf("%s: не удалось вычислить границы страницы: %w", methodCtx, err)
	}
	return values[start:end], nil
}

// SaveQuote appends a quote to the market history.
func (m *Memory) SaveQuote(ctx context.Context, value domain.MarketQuote) error {
	const methodCtx = "repository.Memory.SaveQuote"

	if err := checkContext(ctx); err != nil {
		return fmt.Errorf("%s: не удалось проверить контекст: %w", methodCtx, err)
	}
	if err := requireID("предмет", value.ItemID); err != nil {
		return fmt.Errorf("%s: некорректная котировка: %w", methodCtx, err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := checkContext(ctx); err != nil {
		return fmt.Errorf("%s: контекст завершён во время ожидания блокировки записи: %w", methodCtx, err)
	}
	m.quotes = append(m.quotes, value)
	return nil
}

// LatestQuote returns the newest quote for an item.
func (m *Memory) LatestQuote(ctx context.Context, itemID string) (domain.MarketQuote, error) {
	const methodCtx = "repository.Memory.LatestQuote"

	if err := checkContext(ctx); err != nil {
		return domain.MarketQuote{}, fmt.Errorf("%s: не удалось проверить контекст: %w", methodCtx, err)
	}
	if err := requireID("предмет", itemID); err != nil {
		return domain.MarketQuote{}, fmt.Errorf("%s: некорректный запрос котировки: %w", methodCtx, err)
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	var latest domain.MarketQuote
	found := false
	for _, value := range m.quotes {
		if value.ItemID != itemID {
			continue
		}
		if !found || value.ObservedAt.After(latest.ObservedAt) ||
			value.ObservedAt.Equal(latest.ObservedAt) {
			latest = value
			found = true
		}
	}
	if !found {
		return domain.MarketQuote{}, fmt.Errorf(
			"%s: котировка предмета %q не найдена: %w",
			methodCtx,
			itemID,
			notFound("котировка предмета", itemID),
		)
	}
	return latest, nil
}

// ListQuotes returns quote history newest first.
func (m *Memory) ListQuotes(ctx context.Context, filter domain.QuoteFilter) ([]domain.MarketQuote, error) {
	const methodCtx = "repository.Memory.ListQuotes"

	if err := checkContext(ctx); err != nil {
		return nil, fmt.Errorf("%s: не удалось проверить контекст: %w", methodCtx, err)
	}
	m.mu.RLock()
	values := make([]domain.MarketQuote, 0, len(m.quotes))
	// Walk backwards so equal timestamps keep the same newest-write-first
	// ordering as SQLite's sequence DESC tie-breaker.
	for index := len(m.quotes) - 1; index >= 0; index-- {
		value := m.quotes[index]
		if filter.ItemID != "" && value.ItemID != filter.ItemID {
			continue
		}
		if !filter.Since.IsZero() && value.ObservedAt.Before(filter.Since) {
			continue
		}
		if !filter.Until.IsZero() && value.ObservedAt.After(filter.Until) {
			continue
		}
		values = append(values, value)
	}
	m.mu.RUnlock()
	sort.SliceStable(values, func(i, j int) bool {
		return values[i].ObservedAt.After(values[j].ObservedAt)
	})
	start, end, err := pageBounds(len(values), filter.Limit, filter.Offset)
	if err != nil {
		return nil, fmt.Errorf("%s: не удалось вычислить границы страницы: %w", methodCtx, err)
	}
	return values[start:end], nil
}

// SaveTradeQuote appends a complete economic quote.
func (m *Memory) SaveTradeQuote(ctx context.Context, value domain.TradeQuote) error {
	const methodCtx = "repository.Memory.SaveTradeQuote"

	if err := checkContext(ctx); err != nil {
		return fmt.Errorf("%s: не удалось проверить контекст: %w", methodCtx, err)
	}
	if err := requireID("предмет", value.ItemID); err != nil {
		return fmt.Errorf("%s: некорректная торговая котировка: %w", methodCtx, err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := checkContext(ctx); err != nil {
		return fmt.Errorf("%s: контекст завершён во время ожидания блокировки записи: %w", methodCtx, err)
	}
	m.tradeQuotes = append(m.tradeQuotes, value)
	return nil
}

// LatestTradeQuote returns the newest complete quote for an item.
func (m *Memory) LatestTradeQuote(ctx context.Context, itemID string) (domain.TradeQuote, error) {
	const methodCtx = "repository.Memory.LatestTradeQuote"

	values, err := m.ListTradeQuotes(ctx, domain.TradeQuoteFilter{ItemID: itemID, Limit: 1})
	if err != nil {
		return domain.TradeQuote{}, fmt.Errorf("%s: не удалось получить торговые котировки: %w", methodCtx, err)
	}
	if len(values) == 0 {
		return domain.TradeQuote{}, fmt.Errorf(
			"%s: торговая котировка предмета %q не найдена: %w",
			methodCtx,
			itemID,
			notFound("торговая котировка предмета", itemID),
		)
	}
	return values[0], nil
}

// ListTradeQuotes returns complete quotes newest first.
func (m *Memory) ListTradeQuotes(
	ctx context.Context,
	filter domain.TradeQuoteFilter,
) ([]domain.TradeQuote, error) {
	const methodCtx = "repository.Memory.ListTradeQuotes"

	if err := checkContext(ctx); err != nil {
		return nil, fmt.Errorf("%s: не удалось проверить контекст: %w", methodCtx, err)
	}
	m.mu.RLock()
	values := make([]domain.TradeQuote, 0, len(m.tradeQuotes))
	for index := len(m.tradeQuotes) - 1; index >= 0; index-- {
		value := m.tradeQuotes[index]
		if filter.ItemID != "" && value.ItemID != filter.ItemID {
			continue
		}
		if !filter.Since.IsZero() && value.ObservedAt.Before(filter.Since) {
			continue
		}
		if !filter.Until.IsZero() && value.ObservedAt.After(filter.Until) {
			continue
		}
		values = append(values, value)
	}
	m.mu.RUnlock()
	sort.SliceStable(values, func(i, j int) bool {
		return values[i].ObservedAt.After(values[j].ObservedAt)
	})
	start, end, err := pageBounds(len(values), filter.Limit, filter.Offset)
	if err != nil {
		return nil, fmt.Errorf("%s: не удалось вычислить границы страницы: %w", methodCtx, err)
	}
	return values[start:end], nil
}

// SaveExecution atomically updates the current snapshot and appends history.
func (m *Memory) SaveExecution(ctx context.Context, value domain.TradeExecution) error {
	const methodCtx = "repository.Memory.SaveExecution"

	if err := checkContext(ctx); err != nil {
		return fmt.Errorf("%s: не удалось проверить контекст: %w", methodCtx, err)
	}
	if err := requireID("исполнение сделки", value.ID); err != nil {
		return fmt.Errorf("%s: некорректное исполнение сделки: %w", methodCtx, err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := checkContext(ctx); err != nil {
		return fmt.Errorf("%s: контекст завершён во время ожидания блокировки записи: %w", methodCtx, err)
	}
	m.executions[value.ID] = value
	m.executionHistory[value.ID] = append(m.executionHistory[value.ID], value)
	return nil
}

// Execution returns the current execution snapshot.
func (m *Memory) Execution(ctx context.Context, id string) (domain.TradeExecution, error) {
	const methodCtx = "repository.Memory.Execution"

	if err := checkContext(ctx); err != nil {
		return domain.TradeExecution{}, fmt.Errorf("%s: не удалось проверить контекст: %w", methodCtx, err)
	}
	if err := requireID("исполнение сделки", id); err != nil {
		return domain.TradeExecution{}, fmt.Errorf("%s: некорректный запрос исполнения сделки: %w", methodCtx, err)
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	value, ok := m.executions[id]
	if !ok {
		return domain.TradeExecution{}, fmt.Errorf(
			"%s: исполнение сделки %q не найдено: %w",
			methodCtx,
			id,
			notFound("исполнение сделки", id),
		)
	}
	return value, nil
}

// ListExecutions returns current snapshots newest first.
func (m *Memory) ListExecutions(ctx context.Context, filter domain.ExecutionFilter) ([]domain.TradeExecution, error) {
	const methodCtx = "repository.Memory.ListExecutions"

	if err := checkContext(ctx); err != nil {
		return nil, fmt.Errorf("%s: не удалось проверить контекст: %w", methodCtx, err)
	}
	m.mu.RLock()
	values := make([]domain.TradeExecution, 0, len(m.executions))
	for _, value := range m.executions {
		if filter.OpportunityID != "" && value.OpportunityID != filter.OpportunityID {
			continue
		}
		if filter.Status != "" && value.Status != filter.Status {
			continue
		}
		if !filter.Since.IsZero() && value.UpdatedAt.Before(filter.Since) {
			continue
		}
		if !filter.Until.IsZero() && value.UpdatedAt.After(filter.Until) {
			continue
		}
		values = append(values, value)
	}
	m.mu.RUnlock()
	sort.Slice(values, func(i, j int) bool {
		if values[i].UpdatedAt.Equal(values[j].UpdatedAt) {
			return values[i].ID > values[j].ID
		}
		return values[i].UpdatedAt.After(values[j].UpdatedAt)
	})
	start, end, err := pageBounds(len(values), filter.Limit, filter.Offset)
	if err != nil {
		return nil, fmt.Errorf("%s: не удалось вычислить границы страницы: %w", methodCtx, err)
	}
	return values[start:end], nil
}

// ExecutionHistory returns historical snapshots newest first.
func (m *Memory) ExecutionHistory(ctx context.Context, id string, limit int) ([]domain.TradeExecution, error) {
	const methodCtx = "repository.Memory.ExecutionHistory"

	if err := checkContext(ctx); err != nil {
		return nil, fmt.Errorf("%s: не удалось проверить контекст: %w", methodCtx, err)
	}
	if err := requireID("исполнение сделки", id); err != nil {
		return nil, fmt.Errorf("%s: некорректный запрос истории исполнения: %w", methodCtx, err)
	}
	m.mu.RLock()
	history, ok := m.executionHistory[id]
	if !ok {
		m.mu.RUnlock()
		return nil, fmt.Errorf(
			"%s: история исполнения сделки %q не найдена: %w",
			methodCtx,
			id,
			notFound("исполнение сделки", id),
		)
	}
	values := append([]domain.TradeExecution(nil), history...)
	m.mu.RUnlock()
	for left, right := 0, len(values)-1; left < right; left, right = left+1, right-1 {
		values[left], values[right] = values[right], values[left]
	}
	_, end, err := pageBounds(len(values), limit, 0)
	if err != nil {
		return nil, fmt.Errorf("%s: не удалось вычислить границы истории: %w", methodCtx, err)
	}
	return values[:end], nil
}

// SaveAction stores an action request idempotently by its identifier.
func (m *Memory) SaveAction(ctx context.Context, value domain.ActionRecord) error {
	const methodCtx = "repository.Memory.SaveAction"

	value = normalizedAction(value)
	if err := checkContext(ctx); err != nil {
		return fmt.Errorf("%s: не удалось проверить контекст: %w", methodCtx, err)
	}
	if err := requireID("действие", value.ID); err != nil {
		return fmt.Errorf("%s: некорректная запись действия: %w", methodCtx, err)
	}
	if err := validateActionFrameBasis(value); err != nil {
		return fmt.Errorf("%s: ROI-основание действия некорректно: %w", methodCtx, err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := checkContext(ctx); err != nil {
		return fmt.Errorf("%s: контекст завершён во время ожидания блокировки записи: %w", methodCtx, err)
	}
	if existing, exists := m.actions[value.ID]; exists {
		if actionsEqual(existing, value) {
			return nil
		}
		return fmt.Errorf(
			"%s: действие %q конфликтует с сохранённой записью: %w",
			methodCtx,
			value.ID,
			conflict("действие", value.ID),
		)
	}
	m.actions[value.ID] = cloneAction(value)
	return nil
}

// Action returns an action request by identifier.
func (m *Memory) Action(ctx context.Context, id string) (domain.ActionRecord, error) {
	const methodCtx = "repository.Memory.Action"

	if err := checkContext(ctx); err != nil {
		return domain.ActionRecord{}, fmt.Errorf("%s: не удалось проверить контекст: %w", methodCtx, err)
	}
	if err := requireID("действие", id); err != nil {
		return domain.ActionRecord{}, fmt.Errorf("%s: некорректный запрос действия: %w", methodCtx, err)
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	value, ok := m.actions[id]
	if !ok {
		return domain.ActionRecord{}, fmt.Errorf(
			"%s: действие %q не найдено: %w",
			methodCtx,
			id,
			notFound("действие", id),
		)
	}
	return cloneAction(value), nil
}

// ListActions returns matching action requests newest first.
func (m *Memory) ListActions(ctx context.Context, filter domain.ActionFilter) ([]domain.ActionRecord, error) {
	const methodCtx = "repository.Memory.ListActions"

	if err := checkContext(ctx); err != nil {
		return nil, fmt.Errorf("%s: не удалось проверить контекст: %w", methodCtx, err)
	}
	m.mu.RLock()
	values := make([]domain.ActionRecord, 0, len(m.actions))
	for _, value := range m.actions {
		if filter.SessionID != "" && value.SessionID != filter.SessionID {
			continue
		}
		if filter.AgentID != "" && value.AgentID != filter.AgentID {
			continue
		}
		if filter.Kind != "" && value.Kind != filter.Kind {
			continue
		}
		if filter.PendingOnly && len(m.actionResults[value.ID]) != 0 {
			continue
		}
		if !filter.Since.IsZero() && value.RequestedAt.Before(filter.Since) {
			continue
		}
		if !filter.Until.IsZero() && value.RequestedAt.After(filter.Until) {
			continue
		}
		values = append(values, cloneAction(value))
	}
	m.mu.RUnlock()
	sort.Slice(values, func(i, j int) bool {
		if values[i].RequestedAt.Equal(values[j].RequestedAt) {
			return values[i].ID > values[j].ID
		}
		return values[i].RequestedAt.After(values[j].RequestedAt)
	})
	start, end, err := pageBounds(len(values), filter.Limit, filter.Offset)
	if err != nil {
		return nil, fmt.Errorf("%s: не удалось вычислить границы страницы: %w", methodCtx, err)
	}
	return values[start:end], nil
}

// SaveActionResult appends an immutable action outcome.
func (m *Memory) SaveActionResult(ctx context.Context, value domain.ActionResultRecord) error {
	const methodCtx = "repository.Memory.SaveActionResult"

	if err := checkContext(ctx); err != nil {
		return fmt.Errorf("%s: не удалось проверить контекст: %w", methodCtx, err)
	}
	if err := requireID("действие", value.ActionID); err != nil {
		return fmt.Errorf("%s: некорректный результат действия: %w", methodCtx, err)
	}
	if err := validateActionResult(value); err != nil {
		return fmt.Errorf("%s: некорректный результат действия: %w", methodCtx, err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := checkContext(ctx); err != nil {
		return fmt.Errorf("%s: контекст завершён во время ожидания блокировки записи: %w", methodCtx, err)
	}
	if _, exists := m.actions[value.ActionID]; !exists {
		return fmt.Errorf(
			"%s: исходное действие %q не найдено: %w",
			methodCtx,
			value.ActionID,
			notFound("действие", value.ActionID),
		)
	}
	m.actionResults[value.ActionID] = append(m.actionResults[value.ActionID], value)
	return nil
}

// ActionResult returns the most recently persisted outcome for an action.
func (m *Memory) ActionResult(ctx context.Context, actionID string) (domain.ActionResultRecord, error) {
	const methodCtx = "repository.Memory.ActionResult"

	if err := checkContext(ctx); err != nil {
		return domain.ActionResultRecord{}, fmt.Errorf("%s: не удалось проверить контекст: %w", methodCtx, err)
	}
	if err := requireID("действие", actionID); err != nil {
		return domain.ActionResultRecord{}, fmt.Errorf("%s: некорректный запрос результата действия: %w", methodCtx, err)
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	values := m.actionResults[actionID]
	if len(values) == 0 {
		return domain.ActionResultRecord{}, fmt.Errorf(
			"%s: результат действия %q не найден: %w",
			methodCtx,
			actionID,
			notFound("результат действия", actionID),
		)
	}
	return values[len(values)-1], nil
}

// ListActionResults returns outcomes in reverse insertion order.
func (m *Memory) ListActionResults(ctx context.Context, actionID string, limit int) ([]domain.ActionResultRecord, error) {
	const methodCtx = "repository.Memory.ListActionResults"

	if err := checkContext(ctx); err != nil {
		return nil, fmt.Errorf("%s: не удалось проверить контекст: %w", methodCtx, err)
	}
	if err := requireID("действие", actionID); err != nil {
		return nil, fmt.Errorf("%s: некорректный запрос результатов действия: %w", methodCtx, err)
	}
	m.mu.RLock()
	stored := m.actionResults[actionID]
	if len(stored) == 0 {
		m.mu.RUnlock()
		return nil, fmt.Errorf(
			"%s: результаты действия %q не найдены: %w",
			methodCtx,
			actionID,
			notFound("результат действия", actionID),
		)
	}
	values := append([]domain.ActionResultRecord(nil), stored...)
	m.mu.RUnlock()
	for left, right := 0, len(values)-1; left < right; left, right = left+1, right-1 {
		values[left], values[right] = values[right], values[left]
	}
	_, end, err := pageBounds(len(values), limit, 0)
	if err != nil {
		return nil, fmt.Errorf("%s: не удалось вычислить границы результатов: %w", methodCtx, err)
	}
	return values[:end], nil
}

// SaveEvent stores an event idempotently by its identifier.
func (m *Memory) SaveEvent(ctx context.Context, value domain.AgentEventRecord) error {
	const methodCtx = "repository.Memory.SaveEvent"

	if err := checkContext(ctx); err != nil {
		return fmt.Errorf("%s: не удалось проверить контекст: %w", methodCtx, err)
	}
	if err := requireID("событие", value.ID); err != nil {
		return fmt.Errorf("%s: некорректная запись события: %w", methodCtx, err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := checkContext(ctx); err != nil {
		return fmt.Errorf("%s: контекст завершён во время ожидания блокировки записи: %w", methodCtx, err)
	}
	if existing, exists := m.events[value.ID]; exists {
		if eventsEqual(existing, value) {
			return nil
		}
		return fmt.Errorf(
			"%s: событие %q конфликтует с сохранённой записью: %w",
			methodCtx,
			value.ID,
			conflict("событие", value.ID),
		)
	}
	m.events[value.ID] = cloneEvent(value)
	return nil
}

// Event returns an event by identifier.
func (m *Memory) Event(ctx context.Context, id string) (domain.AgentEventRecord, error) {
	const methodCtx = "repository.Memory.Event"

	if err := checkContext(ctx); err != nil {
		return domain.AgentEventRecord{}, fmt.Errorf("%s: не удалось проверить контекст: %w", methodCtx, err)
	}
	if err := requireID("событие", id); err != nil {
		return domain.AgentEventRecord{}, fmt.Errorf("%s: некорректный запрос события: %w", methodCtx, err)
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	value, ok := m.events[id]
	if !ok {
		return domain.AgentEventRecord{}, fmt.Errorf(
			"%s: событие %q не найдено: %w",
			methodCtx,
			id,
			notFound("событие", id),
		)
	}
	return cloneEvent(value), nil
}

// ListEvents returns matching operational events newest first.
func (m *Memory) ListEvents(ctx context.Context, filter domain.EventFilter) ([]domain.AgentEventRecord, error) {
	const methodCtx = "repository.Memory.ListEvents"

	if err := checkContext(ctx); err != nil {
		return nil, fmt.Errorf("%s: не удалось проверить контекст: %w", methodCtx, err)
	}
	m.mu.RLock()
	values := make([]domain.AgentEventRecord, 0, len(m.events))
	for _, value := range m.events {
		if filter.SessionID != "" && value.SessionID != filter.SessionID {
			continue
		}
		if filter.AgentID != "" && value.AgentID != filter.AgentID {
			continue
		}
		if filter.Kind != "" && value.Kind != filter.Kind {
			continue
		}
		if filter.Severity != "" && value.Severity != filter.Severity {
			continue
		}
		if !filter.Since.IsZero() && value.CreatedAt.Before(filter.Since) {
			continue
		}
		if !filter.Until.IsZero() && value.CreatedAt.After(filter.Until) {
			continue
		}
		values = append(values, cloneEvent(value))
	}
	m.mu.RUnlock()
	sort.Slice(values, func(i, j int) bool {
		if values[i].CreatedAt.Equal(values[j].CreatedAt) {
			return values[i].ID > values[j].ID
		}
		return values[i].CreatedAt.After(values[j].CreatedAt)
	})
	start, end, err := pageBounds(len(values), filter.Limit, filter.Offset)
	if err != nil {
		return nil, fmt.Errorf("%s: не удалось вычислить границы страницы: %w", methodCtx, err)
	}
	return values[start:end], nil
}

// SaveRuntimeRecord atomically replaces one rich module snapshot.
func (m *Memory) SaveRuntimeRecord(ctx context.Context, value domain.RuntimeRecord) error {
	const methodCtx = "repository.Memory.SaveRuntimeRecord"

	if err := checkContext(ctx); err != nil {
		return fmt.Errorf("%s: не удалось проверить контекст: %w", methodCtx, err)
	}
	if err := requireID("запись состояния среды выполнения", value.Key); err != nil {
		return fmt.Errorf("%s: некорректная запись состояния среды выполнения: %w", methodCtx, err)
	}
	if len(value.Payload) == 0 {
		return fmt.Errorf("%s: содержимое записи состояния среды выполнения обязательно", methodCtx)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := checkContext(ctx); err != nil {
		return fmt.Errorf("%s: контекст завершён во время ожидания блокировки записи: %w", methodCtx, err)
	}
	m.runtimeRecords[value.Key] = cloneRuntimeRecord(value)
	return nil
}

// RuntimeRecord returns one rich module snapshot.
func (m *Memory) RuntimeRecord(ctx context.Context, key string) (domain.RuntimeRecord, error) {
	const methodCtx = "repository.Memory.RuntimeRecord"

	if err := checkContext(ctx); err != nil {
		return domain.RuntimeRecord{}, fmt.Errorf("%s: не удалось проверить контекст: %w", methodCtx, err)
	}
	if err := requireID("запись состояния среды выполнения", key); err != nil {
		return domain.RuntimeRecord{}, fmt.Errorf("%s: некорректный запрос состояния среды выполнения: %w", methodCtx, err)
	}
	m.mu.RLock()
	value, exists := m.runtimeRecords[key]
	m.mu.RUnlock()
	if !exists {
		return domain.RuntimeRecord{}, fmt.Errorf(
			"%s: запись состояния среды выполнения %q не найдена: %w",
			methodCtx,
			key,
			notFound("запись состояния среды выполнения", key),
		)
	}
	return cloneRuntimeRecord(value), nil
}

// ListRuntimeRecords returns matching snapshots newest first.
func (m *Memory) ListRuntimeRecords(
	ctx context.Context,
	filter domain.RuntimeRecordFilter,
) ([]domain.RuntimeRecord, error) {
	const methodCtx = "repository.Memory.ListRuntimeRecords"

	if err := checkContext(ctx); err != nil {
		return nil, fmt.Errorf("%s: не удалось проверить контекст: %w", methodCtx, err)
	}
	m.mu.RLock()
	values := make([]domain.RuntimeRecord, 0, len(m.runtimeRecords))
	for _, value := range m.runtimeRecords {
		if filter.Kind != "" && value.Kind != filter.Kind {
			continue
		}
		if filter.KeyPrefix != "" && !strings.HasPrefix(value.Key, filter.KeyPrefix) {
			continue
		}
		values = append(values, cloneRuntimeRecord(value))
	}
	m.mu.RUnlock()
	sort.Slice(values, func(i, j int) bool {
		if values[i].UpdatedAt.Equal(values[j].UpdatedAt) {
			return values[i].Key < values[j].Key
		}
		return values[i].UpdatedAt.After(values[j].UpdatedAt)
	})
	start, end, err := pageBounds(len(values), filter.Limit, filter.Offset)
	if err != nil {
		return nil, fmt.Errorf("%s: не удалось вычислить границы страницы: %w", methodCtx, err)
	}
	return values[start:end], nil
}

// WithinTransaction executes fn against a private copy and publishes all
// changes together only if fn succeeds.
func (m *Memory) WithinTransaction(ctx context.Context, fn func(Store) error) error {
	const methodCtx = "repository.Memory.WithinTransaction"

	if err := checkContext(ctx); err != nil {
		wrapped := fmt.Errorf("%s: не удалось проверить контекст: %w", methodCtx, err)
		m.txState.markFailed(wrapped)
		return wrapped
	}
	if fn == nil {
		return nil
	}
	if m.txState != nil {
		defer func() {
			const methodCtx = "repository.Memory.WithinTransaction.recover"

			if panicValue := recover(); panicValue != nil {
				m.txState.markFailed(fmt.Errorf(
					"%s: во вложенной транзакции произошла паника",
					methodCtx,
				))
				panic(panicValue)
			}
		}()
		if err := fn(m); err != nil {
			wrapped := fmt.Errorf("%s: вложенная операция транзакции завершилась ошибкой: %w", methodCtx, err)
			m.txState.markFailed(wrapped)
			return wrapped
		}
		if failure := m.txState.failedWith(); failure != nil {
			return fmt.Errorf("%s: транзакция ранее помечена для отката: %w", methodCtx, failure)
		}
		return nil
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if err := checkContext(ctx); err != nil {
		return fmt.Errorf("%s: контекст завершён во время ожидания блокировки транзакции: %w", methodCtx, err)
	}
	clone := m.cloneUnlocked()
	state := &transactionState{}
	clone.txState = state
	if err := fn(clone); err != nil {
		return fmt.Errorf("%s: операция транзакции завершилась ошибкой: %w", methodCtx, err)
	}
	if failure := state.failedWith(); failure != nil {
		return fmt.Errorf(
			"%s: транзакция помечена для отката после ошибки вложенной операции: %w",
			methodCtx,
			failure,
		)
	}
	if err := checkContext(ctx); err != nil {
		return fmt.Errorf("%s: контекст завершён перед публикацией транзакции: %w", methodCtx, err)
	}
	m.replaceUnlocked(clone)
	return nil
}

func (m *Memory) cloneUnlocked() *Memory {
	clone := NewMemory()
	for id, value := range m.observations {
		clone.observations[id] = cloneObservation(value)
	}
	clone.quotes = append(clone.quotes, m.quotes...)
	clone.tradeQuotes = append(clone.tradeQuotes, m.tradeQuotes...)
	for id, value := range m.executions {
		clone.executions[id] = value
	}
	for id, values := range m.executionHistory {
		clone.executionHistory[id] = append([]domain.TradeExecution(nil), values...)
	}
	for id, value := range m.actions {
		clone.actions[id] = cloneAction(value)
	}
	for id, values := range m.actionResults {
		clone.actionResults[id] = append([]domain.ActionResultRecord(nil), values...)
	}
	for id, value := range m.events {
		clone.events[id] = cloneEvent(value)
	}
	for key, value := range m.runtimeRecords {
		clone.runtimeRecords[key] = cloneRuntimeRecord(value)
	}
	return clone
}

func (m *Memory) replaceUnlocked(value *Memory) {
	m.observations = value.observations
	m.quotes = value.quotes
	m.tradeQuotes = value.tradeQuotes
	m.executions = value.executions
	m.executionHistory = value.executionHistory
	m.actions = value.actions
	m.actionResults = value.actionResults
	m.events = value.events
	m.runtimeRecords = value.runtimeRecords
}

func cloneObservation(value domain.Observation) domain.Observation {
	cloned := value
	if value.Elements != nil {
		cloned.Elements = append(make([]domain.UIElement, 0, len(value.Elements)), value.Elements...)
	}
	if value.Values != nil {
		cloned.Values = make(map[string]domain.Value, len(value.Values))
		for key, item := range value.Values {
			cloned.Values[key] = item
		}
	}
	return cloned
}

func cloneAction(value domain.ActionRecord) domain.ActionRecord {
	cloned := value
	if value.BasedOnCapturedAt != nil {
		capturedAt := *value.BasedOnCapturedAt
		cloned.BasedOnCapturedAt = &capturedAt
	}
	if value.Point != nil {
		point := *value.Point
		cloned.Point = &point
	}
	if value.ActionPayload != nil {
		cloned.ActionPayload = append(make(json.RawMessage, 0, len(value.ActionPayload)), value.ActionPayload...)
	}
	if value.FrameBasisPayload != nil {
		cloned.FrameBasisPayload = append(
			make(json.RawMessage, 0, len(value.FrameBasisPayload)),
			value.FrameBasisPayload...,
		)
	}
	return cloned
}

func cloneEvent(value domain.AgentEventRecord) domain.AgentEventRecord {
	cloned := value
	if value.Payload != nil {
		cloned.Payload = append(make(json.RawMessage, 0, len(value.Payload)), value.Payload...)
	}
	return cloned
}

func cloneRuntimeRecord(value domain.RuntimeRecord) domain.RuntimeRecord {
	cloned := value
	if value.Payload != nil {
		cloned.Payload = append(make(json.RawMessage, 0, len(value.Payload)), value.Payload...)
	}
	return cloned
}

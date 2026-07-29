// Package repository определяет атомарное хранилище состояния контроллера.
package repository

import (
	"context"
	"fmt"
	"sync"

	"github.com/arena-trading-agent/arena-trading-agent/internal/domain"
)

// Repository сохраняет наблюдения, котировки и торговые транзакции.
type Repository interface {
	SaveObservation(context.Context, domain.Observation) error
	SaveQuote(context.Context, domain.MarketQuote) error
	SaveExecution(context.Context, domain.TradeExecution) error
	Execution(context.Context, string) (domain.TradeExecution, error)
}

// Memory предоставляет потокобезопасное хранилище для симуляции и тестов.
type Memory struct {
	mu           sync.RWMutex
	observations map[uint64]domain.Observation
	quotes       map[string]domain.MarketQuote
	executions   map[string]domain.TradeExecution
}

// NewMemory создаёт пустое хранилище.
func NewMemory() *Memory {
	return &Memory{observations: make(map[uint64]domain.Observation), quotes: make(map[string]domain.MarketQuote), executions: make(map[string]domain.TradeExecution)}
}

// SaveObservation сохраняет последнюю версию наблюдения кадра.
func (m *Memory) SaveObservation(_ context.Context, value domain.Observation) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.observations[value.FrameID] = value
	return nil
}

// SaveQuote сохраняет последнюю котировку предмета.
func (m *Memory) SaveQuote(_ context.Context, value domain.MarketQuote) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.quotes[value.ItemID] = value
	return nil
}

// SaveExecution атомарно сохраняет состояние торговой saga.
func (m *Memory) SaveExecution(_ context.Context, value domain.TradeExecution) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.executions[value.ID] = value
	return nil
}

// Execution возвращает торговую транзакцию по идентификатору.
func (m *Memory) Execution(_ context.Context, id string) (domain.TradeExecution, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	value, ok := m.executions[id]
	if !ok {
		return domain.TradeExecution{}, fmt.Errorf("торговая транзакция %q не найдена", id)
	}
	return value, nil
}

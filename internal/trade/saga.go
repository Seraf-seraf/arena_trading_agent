// Package trade управляет последовательным исполнением и recovery торговой saga.
package trade

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/arena-trading-agent/arena-trading-agent/internal/domain"
	"github.com/arena-trading-agent/arena-trading-agent/internal/repository"
)

// StepRunner выполняет один идемпотентный шаг торгового плана.
type StepRunner interface {
	RunStep(context.Context, domain.TradeExecution, domain.TradeStep) error
	Recover(context.Context, domain.TradeExecution, domain.TradeStep, error) error
}

// Executor гарантирует, что одновременно исполняется не более одной сделки.
type Executor struct {
	mu         sync.Mutex
	repository repository.Repository
	runner     StepRunner
}

// NewExecutor создаёт последовательный исполнитель торговых saga.
func NewExecutor(repository repository.Repository, runner StepRunner) *Executor {
	return &Executor{repository: repository, runner: runner}
}

// Execute сохраняет прогресс после каждого денежного шага и запускает recovery при отказе.
func (e *Executor) Execute(ctx context.Context, execution domain.TradeExecution, opportunity domain.TradeOpportunity) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	execution.Status = domain.TradeRunning
	execution.UpdatedAt = time.Now().UTC()
	if execution.StartedAt.IsZero() {
		execution.StartedAt = execution.UpdatedAt
	}
	if err := e.repository.SaveExecution(ctx, execution); err != nil {
		return fmt.Errorf("не удалось сохранить начало сделки: %w", err)
	}
	for index := execution.CurrentStep; index < len(opportunity.Steps); index++ {
		step := opportunity.Steps[index]
		if err := e.runner.RunStep(ctx, execution, step); err != nil {
			execution.Status = domain.TradeRecovering
			execution.Failure = err.Error()
			execution.UpdatedAt = time.Now().UTC()
			_ = e.repository.SaveExecution(ctx, execution)
			if recoveryErr := e.runner.Recover(ctx, execution, step, err); recoveryErr != nil {
				execution.Status = domain.TradeFailed
				execution.Failure = recoveryErr.Error()
				_ = e.repository.SaveExecution(ctx, execution)
				return fmt.Errorf("не удалось восстановить частичную сделку: %w", recoveryErr)
			}
			return fmt.Errorf("исполнение сделки приостановлено для восстановления: %w", err)
		}
		execution.CurrentStep = index + 1
		execution.UpdatedAt = time.Now().UTC()
		if err := e.repository.SaveExecution(ctx, execution); err != nil {
			return fmt.Errorf("не удалось сохранить прогресс сделки: %w", err)
		}
	}
	execution.Status = domain.TradeCompleted
	execution.UpdatedAt = time.Now().UTC()
	return e.repository.SaveExecution(ctx, execution)
}

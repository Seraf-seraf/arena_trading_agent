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
	const methodCtx = "trade.Executor.Execute"

	e.mu.Lock()
	defer e.mu.Unlock()
	execution.Status = domain.TradeRunning
	execution.UpdatedAt = time.Now().UTC()
	if execution.StartedAt.IsZero() {
		execution.StartedAt = execution.UpdatedAt
	}
	if err := e.repository.SaveExecution(ctx, execution); err != nil {
		return fmt.Errorf("%s: не удалось сохранить начало сделки: %w", methodCtx, err)
	}
	for index := execution.CurrentStep; index < len(opportunity.Steps); index++ {
		step := opportunity.Steps[index]
		if err := e.runner.RunStep(ctx, execution, step); err != nil {
			execution.Status = domain.TradeRecovering
			execution.Failure = fmt.Sprintf("%s: ошибка шага сделки: %s", methodCtx, err)
			execution.UpdatedAt = time.Now().UTC()
			_ = e.repository.SaveExecution(ctx, execution)
			if recoveryErr := e.runner.Recover(ctx, execution, step, err); recoveryErr != nil {
				execution.Status = domain.TradeFailed
				execution.Failure = fmt.Sprintf("%s: ошибка восстановления сделки: %s", methodCtx, recoveryErr)
				_ = e.repository.SaveExecution(ctx, execution)
				return fmt.Errorf("%s: не удалось восстановить частичную сделку: %w", methodCtx, recoveryErr)
			}
			return fmt.Errorf("%s: исполнение сделки приостановлено для восстановления: %w", methodCtx, err)
		}
		execution.CurrentStep = index + 1
		execution.UpdatedAt = time.Now().UTC()
		if err := e.repository.SaveExecution(ctx, execution); err != nil {
			return fmt.Errorf("%s: не удалось сохранить прогресс сделки: %w", methodCtx, err)
		}
	}
	execution.Status = domain.TradeCompleted
	execution.UpdatedAt = time.Now().UTC()
	if err := e.repository.SaveExecution(ctx, execution); err != nil {
		return fmt.Errorf("%s: не удалось сохранить завершение сделки: %w", methodCtx, err)
	}
	return nil
}

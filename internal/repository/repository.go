// Package repository defines durable controller state and audit journals.
package repository

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/arena-trading-agent/arena-trading-agent/internal/domain"
)

// ErrNotFound is returned when a requested record does not exist.
var ErrNotFound = errors.New("запись репозитория не найдена")

// ErrConflict is returned when an immutable record identifier is reused with
// different content.
var ErrConflict = errors.New("конфликт неизменяемой записи репозитория")

// Repository is the small write contract required by the trading executor.
// It intentionally remains backward compatible with the original in-memory
// repository and is convenient for focused test doubles.
type Repository interface {
	SaveObservation(context.Context, domain.Observation) error
	SaveQuote(context.Context, domain.MarketQuote) error
	SaveExecution(context.Context, domain.TradeExecution) error
	Execution(context.Context, string) (domain.TradeExecution, error)
}

// Store is the complete state and audit-journal contract implemented by
// Memory and SQLite.
type Store interface {
	Repository

	Observation(context.Context, uint64) (domain.Observation, error)
	ListObservations(context.Context, domain.ObservationFilter) ([]domain.Observation, error)

	LatestQuote(context.Context, string) (domain.MarketQuote, error)
	ListQuotes(context.Context, domain.QuoteFilter) ([]domain.MarketQuote, error)
	SaveTradeQuote(context.Context, domain.TradeQuote) error
	LatestTradeQuote(context.Context, string) (domain.TradeQuote, error)
	ListTradeQuotes(context.Context, domain.TradeQuoteFilter) ([]domain.TradeQuote, error)

	ListExecutions(context.Context, domain.ExecutionFilter) ([]domain.TradeExecution, error)
	ExecutionHistory(context.Context, string, int) ([]domain.TradeExecution, error)

	SaveAction(context.Context, domain.ActionRecord) error
	Action(context.Context, string) (domain.ActionRecord, error)
	ListActions(context.Context, domain.ActionFilter) ([]domain.ActionRecord, error)

	SaveActionResult(context.Context, domain.ActionResultRecord) error
	ActionResult(context.Context, string) (domain.ActionResultRecord, error)
	ListActionResults(context.Context, string, int) ([]domain.ActionResultRecord, error)

	SaveEvent(context.Context, domain.AgentEventRecord) error
	Event(context.Context, string) (domain.AgentEventRecord, error)
	ListEvents(context.Context, domain.EventFilter) ([]domain.AgentEventRecord, error)

	SaveRuntimeRecord(context.Context, domain.RuntimeRecord) error
	RuntimeRecord(context.Context, string) (domain.RuntimeRecord, error)
	ListRuntimeRecords(context.Context, domain.RuntimeRecordFilter) ([]domain.RuntimeRecord, error)

	// WithinTransaction executes fn atomically. Returning an error rolls the
	// complete unit of work back. A nested call joins the current transaction.
	WithinTransaction(context.Context, func(Store) error) error
}

func notFound(kind, id string) error {
	const methodCtx = "repository.notFound"

	return fmt.Errorf(
		"%s: сущность «%s» с идентификатором %q не найдена: %w",
		methodCtx,
		kind,
		id,
		ErrNotFound,
	)
}

func conflict(kind, id string) error {
	const methodCtx = "repository.conflict"

	return fmt.Errorf(
		"%s: сущность «%s» с идентификатором %q уже содержит другие данные: %w",
		methodCtx,
		kind,
		id,
		ErrConflict,
	)
}

func validateActionResult(value domain.ActionResultRecord) error {
	const methodCtx = "repository.validateActionResult"

	if value.NotSent && value.Success {
		return fmt.Errorf(
			"%s: неотправленное действие не может иметь успешный результат",
			methodCtx,
		)
	}
	if value.NotSent && strings.TrimSpace(value.Error) == "" {
		return fmt.Errorf(
			"%s: результат неотправленного действия должен содержать причину отказа",
			methodCtx,
		)
	}
	if value.NotSent && !value.RetrySafe {
		return fmt.Errorf(
			"%s: неотправленное действие должно быть помечено безопасным для повтора",
			methodCtx,
		)
	}
	return nil
}

func checkContext(ctx context.Context) error {
	const methodCtx = "repository.checkContext"

	if ctx == nil {
		return fmt.Errorf("%s: контекст не задан", methodCtx)
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%s: контекст завершён: %w", methodCtx, err)
	}
	return nil
}

func pageBounds(length, limit, offset int) (int, int, error) {
	const methodCtx = "repository.pageBounds"

	if limit < 0 {
		return 0, 0, fmt.Errorf("%s: лимит не может быть отрицательным", methodCtx)
	}
	if offset < 0 {
		return 0, 0, fmt.Errorf("%s: смещение не может быть отрицательным", methodCtx)
	}
	if offset >= length {
		return length, length, nil
	}
	end := length
	if limit > 0 && limit < length-offset {
		end = offset + limit
	}
	return offset, end, nil
}

func requireID(kind, id string) error {
	const methodCtx = "repository.requireID"

	if id == "" {
		return fmt.Errorf("%s: обязателен идентификатор сущности «%s»", methodCtx, kind)
	}
	return nil
}

func frameIDString(value uint64) string {
	return fmt.Sprintf("%d", value)
}

type transactionState struct {
	mu      sync.Mutex
	failure error
}

func (s *transactionState) markFailed(err error) {
	if s == nil || err == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failure == nil {
		s.failure = err
	}
}

func (s *transactionState) failedWith() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.failure
}

func observationsEqual(first, second domain.Observation) bool {
	return reflect.DeepEqual(normalizedObservation(first), normalizedObservation(second))
}

func normalizedObservation(value domain.Observation) domain.Observation {
	value.CreatedAt = normalizedTime(value.CreatedAt)
	if len(value.Elements) == 0 {
		value.Elements = nil
	} else {
		value.Elements = append([]domain.UIElement(nil), value.Elements...)
	}
	if len(value.Values) == 0 {
		value.Values = nil
	} else {
		values := make(map[string]domain.Value, len(value.Values))
		for key, item := range value.Values {
			values[key] = item
		}
		value.Values = values
	}
	return value
}

func normalizedAction(value domain.ActionRecord) domain.ActionRecord {
	value.Deadline = normalizedTime(value.Deadline)
	value.RequestedAt = normalizedTime(value.RequestedAt)
	if value.BasedOnCapturedAt != nil {
		capturedAt := normalizedTime(*value.BasedOnCapturedAt)
		value.BasedOnCapturedAt = &capturedAt
	}
	if len(value.ActionPayload) == 0 {
		value.ActionPayload = []byte(`{}`)
	} else {
		value.ActionPayload = append([]byte(nil), value.ActionPayload...)
	}
	if len(value.FrameBasisPayload) == 0 {
		value.FrameBasisPayload = []byte(`[]`)
	} else {
		value.FrameBasisPayload = append([]byte(nil), value.FrameBasisPayload...)
	}
	if value.Point != nil {
		point := *value.Point
		value.Point = &point
	}
	return value
}

func actionsEqual(first, second domain.ActionRecord) bool {
	return reflect.DeepEqual(normalizedAction(first), normalizedAction(second))
}

func validateActionFrameBasis(value domain.ActionRecord) error {
	const methodCtx = "repository.validateActionFrameBasis"
	const (
		maxBasisPayloadBytes = 64 << 10
		maxBasisRegions      = 32
	)

	payload := bytes.TrimSpace(value.FrameBasisPayload)
	if len(payload) == 0 || len(payload) > maxBasisPayloadBytes {
		return fmt.Errorf(
			"%s: размер JSON ROI-основания %d должен быть в диапазоне [1, %d]",
			methodCtx,
			len(payload),
			maxBasisPayloadBytes,
		)
	}
	if payload[0] != '[' || payload[len(payload)-1] != ']' {
		return fmt.Errorf("%s: ROI-основание должно быть JSON-массивом", methodCtx)
	}
	var regions []json.RawMessage
	if err := json.Unmarshal(payload, &regions); err != nil {
		return fmt.Errorf("%s: ROI-основание содержит некорректный JSON: %w", methodCtx, err)
	}
	if len(regions) > maxBasisRegions {
		return fmt.Errorf(
			"%s: число областей %d превышает лимит %d",
			methodCtx,
			len(regions),
			maxBasisRegions,
		)
	}
	return nil
}

func eventsEqual(first, second domain.AgentEventRecord) bool {
	return reflect.DeepEqual(normalizedEvent(first), normalizedEvent(second))
}

func normalizedEvent(value domain.AgentEventRecord) domain.AgentEventRecord {
	value.CreatedAt = normalizedTime(value.CreatedAt)
	if len(value.Payload) == 0 {
		value.Payload = nil
	} else {
		value.Payload = append(json.RawMessage(nil), value.Payload...)
	}
	return value
}

func normalizedTime(value time.Time) time.Time {
	if value.IsZero() {
		return time.Time{}
	}
	return value.Round(0).UTC()
}

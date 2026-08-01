package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/arena-trading-agent/arena-trading-agent/internal/domain"
)

func TestStoreRoundTripAndJournals(t *testing.T) {
	t.Parallel()

	factories := map[string]func(*testing.T) Store{
		"memory": func(*testing.T) Store {
			return NewMemory()
		},
		"sqlite": func(t *testing.T) Store {
			store, err := OpenSQLite(context.Background(), filepath.Join(t.TempDir(), "state.db"))
			if err != nil {
				t.Fatalf("OpenSQLite() error = %v", err)
			}
			t.Cleanup(func() {
				if err := store.Close(); err != nil {
					t.Errorf("Close() error = %v", err)
				}
			})
			return store
		},
	}

	for name, factory := range factories {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			testStoreRoundTripAndJournals(t, factory(t))
		})
	}
}

func testStoreRoundTripAndJournals(t *testing.T, store Store) {
	t.Helper()
	ctx := context.Background()
	base := time.Date(2026, time.July, 29, 10, 11, 12, 123456789, time.UTC)

	observation := domain.Observation{
		FrameID: 42,
		State:   domain.StateMarketResults,
		Elements: []domain.UIElement{{
			Kind:       "button",
			Label:      "buy",
			Region:     domain.Rectangle{X: 0.1, Y: 0.2, Width: 0.3, Height: 0.4},
			Confidence: 0.97,
		}},
		Values: map[string]domain.Value{
			"price": {
				Raw:        "12 345",
				Normalized: "12345",
				Source:     "ocr",
				Confidence: 0.99,
				Region:     domain.Rectangle{X: 0.5, Y: 0.5, Width: 0.1, Height: 0.1},
			},
		},
		Confidence: 0.95,
		CreatedAt:  base,
	}
	if err := store.SaveObservation(ctx, observation); err != nil {
		t.Fatalf("SaveObservation() error = %v", err)
	}
	// The memory implementation must not retain mutable caller-owned data.
	observation.Elements[0].Label = "mutated"
	observation.Values["price"] = domain.Value{Raw: "mutated"}

	gotObservation, err := store.Observation(ctx, 42)
	if err != nil {
		t.Fatalf("Observation() error = %v", err)
	}
	if gotObservation.Elements[0].Label != "buy" || gotObservation.Values["price"].Normalized != "12345" {
		t.Fatalf("Observation() retained caller aliases: %#v", gotObservation)
	}
	observations, err := store.ListObservations(ctx, domain.ObservationFilter{
		State: domain.StateMarketResults,
		Since: base.Add(-time.Second),
		Until: base.Add(time.Second),
		Limit: 1,
	})
	if err != nil {
		t.Fatalf("ListObservations() error = %v", err)
	}
	if len(observations) != 1 || observations[0].FrameID != 42 {
		t.Fatalf("ListObservations() = %#v", observations)
	}

	oldQuote := domain.MarketQuote{
		ItemID: "item-1", BuyPrice: 100, SalePrice: 130,
		ObservedAt: base, Confidence: 0.8,
	}
	newQuote := domain.MarketQuote{
		ItemID: "item-1", BuyPrice: 110, SalePrice: 150,
		ObservedAt: base.Add(time.Minute), Confidence: 0.9,
	}
	otherQuote := domain.MarketQuote{
		ItemID: "item-2", BuyPrice: 10, SalePrice: 20,
		ObservedAt: base.Add(2 * time.Minute), Confidence: 0.7,
	}
	for _, value := range []domain.MarketQuote{oldQuote, newQuote, otherQuote} {
		if err := store.SaveQuote(ctx, value); err != nil {
			t.Fatalf("SaveQuote(%q) error = %v", value.ItemID, err)
		}
	}
	latest, err := store.LatestQuote(ctx, "item-1")
	if err != nil {
		t.Fatalf("LatestQuote() error = %v", err)
	}
	if !reflect.DeepEqual(latest, newQuote) {
		t.Fatalf("LatestQuote() = %#v, want %#v", latest, newQuote)
	}
	quotes, err := store.ListQuotes(ctx, domain.QuoteFilter{ItemID: "item-1", Limit: 10})
	if err != nil {
		t.Fatalf("ListQuotes() error = %v", err)
	}
	if !reflect.DeepEqual(quotes, []domain.MarketQuote{newQuote, oldQuote}) {
		t.Fatalf("ListQuotes() = %#v", quotes)
	}
	oldTradeQuote := domain.TradeQuote{
		ItemID: "item-1", PurchasePrice: 100, SalePrice: 140,
		SaleCommission: 4, ListingFee: 2, ObservedAt: base,
		Confidence: .8, LiquidityScore: .7, PriceVolatility: .1, ResaleKnown: true,
	}
	newTradeQuote := oldTradeQuote
	newTradeQuote.PurchasePrice = 105
	newTradeQuote.ObservedAt = base.Add(time.Minute)
	for _, value := range []domain.TradeQuote{oldTradeQuote, newTradeQuote} {
		if err := store.SaveTradeQuote(ctx, value); err != nil {
			t.Fatalf("SaveTradeQuote() error = %v", err)
		}
	}
	latestTradeQuote, err := store.LatestTradeQuote(ctx, "item-1")
	if err != nil || !reflect.DeepEqual(latestTradeQuote, newTradeQuote) {
		t.Fatalf("LatestTradeQuote()=%#v err=%v", latestTradeQuote, err)
	}
	tradeQuotes, err := store.ListTradeQuotes(ctx, domain.TradeQuoteFilter{ItemID: "item-1"})
	if err != nil || !reflect.DeepEqual(tradeQuotes, []domain.TradeQuote{newTradeQuote, oldTradeQuote}) {
		t.Fatalf("ListTradeQuotes()=%#v err=%v", tradeQuotes, err)
	}

	execution := domain.TradeExecution{
		ID:            "execution-1",
		OpportunityID: "opportunity-1",
		Status:        domain.TradeRunning,
		CurrentStep:   1,
		Reserved:      500,
		StartedAt:     base,
		UpdatedAt:     base,
	}
	if err := store.SaveExecution(ctx, execution); err != nil {
		t.Fatalf("SaveExecution(first) error = %v", err)
	}
	execution.Status = domain.TradeCompleted
	execution.CurrentStep = 2
	execution.UpdatedAt = base.Add(time.Minute)
	if err := store.SaveExecution(ctx, execution); err != nil {
		t.Fatalf("SaveExecution(second) error = %v", err)
	}
	currentExecution, err := store.Execution(ctx, execution.ID)
	if err != nil {
		t.Fatalf("Execution() error = %v", err)
	}
	if !reflect.DeepEqual(currentExecution, execution) {
		t.Fatalf("Execution() = %#v, want %#v", currentExecution, execution)
	}
	history, err := store.ExecutionHistory(ctx, execution.ID, 0)
	if err != nil {
		t.Fatalf("ExecutionHistory() error = %v", err)
	}
	if len(history) != 2 || history[0].Status != domain.TradeCompleted || history[1].Status != domain.TradeRunning {
		t.Fatalf("ExecutionHistory() = %#v", history)
	}
	executions, err := store.ListExecutions(ctx, domain.ExecutionFilter{Status: domain.TradeCompleted})
	if err != nil {
		t.Fatalf("ListExecutions() error = %v", err)
	}
	if len(executions) != 1 || executions[0].ID != execution.ID {
		t.Fatalf("ListExecutions() = %#v", executions)
	}

	point := &domain.Point{X: 0.25, Y: 0.75}
	basedOnCapturedAt := base.Add(-time.Second)
	frameBasisPayload := json.RawMessage(
		`[{"region":{"x":0.1,"y":0.2,"width":0.3,"height":0.2},"digest":"digest"}]`,
	)
	action := domain.ActionRecord{
		ID:                 "action-1",
		SessionID:          "session-1",
		AgentID:            "windows-1",
		BasedOnFrame:       42,
		BasedOnCapturedAt:  &basedOnCapturedAt,
		BasedOnFrameDigest: "digest",
		FrameBasisPayload:  frameBasisPayload,
		BasedOnState:       domain.StateMarketHome,
		ExpectedState:      domain.StateMarketResults,
		MinConfidence:      0.91,
		ExpectedWidth:      1920,
		ExpectedHeight:     1080,
		ExpectedDPIPercent: 125,
		Deadline:           base.Add(time.Minute),
		Kind:               "SCROLL",
		Point:              point,
		Value:              "audit-value",
		Delta:              -120,
		RequestedAt:        base,
	}
	if err := store.SaveAction(ctx, action); err != nil {
		t.Fatalf("SaveAction() error = %v", err)
	}
	point.X = 1
	basedOnCapturedAt = base.Add(time.Hour)
	frameBasisPayload[0] = '{'
	storedAction, err := store.Action(ctx, action.ID)
	if err != nil {
		t.Fatalf("Action() error = %v", err)
	}
	if storedAction.Point == nil || storedAction.Point.X != 0.25 ||
		storedAction.BasedOnCapturedAt == nil ||
		!storedAction.BasedOnCapturedAt.Equal(base.Add(-time.Second)) ||
		storedAction.BasedOnFrameDigest != "digest" ||
		string(storedAction.FrameBasisPayload) !=
			`[{"region":{"x":0.1,"y":0.2,"width":0.3,"height":0.2},"digest":"digest"}]` ||
		storedAction.BasedOnState != domain.StateMarketHome ||
		storedAction.MinConfidence != 0.91 {
		t.Fatalf("Action() retained caller alias: %#v", storedAction)
	}
	invalidBasisAction := action
	invalidBasisAction.ID = "action-invalid-frame-basis"
	invalidBasisAction.FrameBasisPayload = json.RawMessage(`null`)
	if err := store.SaveAction(ctx, invalidBasisAction); err == nil {
		t.Fatal("SaveAction() принял ROI-основание, которое не является JSON-массивом")
	}
	pending, err := store.ListActions(ctx, domain.ActionFilter{
		SessionID:   action.SessionID,
		AgentID:     action.AgentID,
		PendingOnly: true,
	})
	if err != nil {
		t.Fatalf("ListActions(pending) error = %v", err)
	}
	if len(pending) != 1 || pending[0].ID != action.ID {
		t.Fatalf("ListActions(pending) = %#v", pending)
	}
	firstResult := domain.ActionResultRecord{
		ActionID:      action.ID,
		MessageID:     "result-message-1",
		CorrelationID: action.ID,
		AgentID:       "windows-1",
		Success:       false,
		NotSent:       true,
		RetrySafe:     true,
		ResultFrame:   43,
		ResultState:   domain.StateMarketResults,
		Error:         "verification failed",
		CompletedAt:   base.Add(time.Second),
		ReceivedAt:    base.Add(1500 * time.Millisecond),
	}
	secondResult := domain.ActionResultRecord{
		ActionID:      action.ID,
		MessageID:     "result-message-2",
		CorrelationID: action.ID,
		AgentID:       "windows-1",
		Success:       true,
		ResultFrame:   44,
		ResultState:   domain.StateItemCard,
		CompletedAt:   base.Add(2 * time.Second),
		ReceivedAt:    base.Add(2500 * time.Millisecond),
	}
	for _, value := range []domain.ActionResultRecord{firstResult, secondResult} {
		if err := store.SaveActionResult(ctx, value); err != nil {
			t.Fatalf("SaveActionResult() error = %v", err)
		}
	}
	latestResult, err := store.ActionResult(ctx, action.ID)
	if err != nil {
		t.Fatalf("ActionResult() error = %v", err)
	}
	if !reflect.DeepEqual(latestResult, secondResult) {
		t.Fatalf("ActionResult() = %#v, want %#v", latestResult, secondResult)
	}
	results, err := store.ListActionResults(ctx, action.ID, 1)
	if err != nil {
		t.Fatalf("ListActionResults() error = %v", err)
	}
	if !reflect.DeepEqual(results, []domain.ActionResultRecord{secondResult}) {
		t.Fatalf("ListActionResults() = %#v", results)
	}
	pending, err = store.ListActions(ctx, domain.ActionFilter{PendingOnly: true})
	if err != nil {
		t.Fatalf("ListActions(after result) error = %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("ListActions(after result) = %#v, want empty", pending)
	}

	event := domain.AgentEventRecord{
		ID:        "event-1",
		SessionID: "session-1",
		AgentID:   "windows-1",
		Kind:      "USER_INPUT",
		Severity:  "WARN",
		Message:   "automation paused",
		Payload:   json.RawMessage(`{"x":10,"y":20}`),
		CreatedAt: base,
	}
	if err := store.SaveEvent(ctx, event); err != nil {
		t.Fatalf("SaveEvent() error = %v", err)
	}
	event.Payload[2] = 'z'
	storedEvent, err := store.Event(ctx, event.ID)
	if err != nil {
		t.Fatalf("Event() error = %v", err)
	}
	if string(storedEvent.Payload) != `{"x":10,"y":20}` {
		t.Fatalf("Event().Payload = %q", storedEvent.Payload)
	}
	events, err := store.ListEvents(ctx, domain.EventFilter{
		SessionID: "session-1",
		Severity:  "WARN",
	})
	if err != nil {
		t.Fatalf("ListEvents() error = %v", err)
	}
	if len(events) != 1 || events[0].ID != storedEvent.ID {
		t.Fatalf("ListEvents() = %#v", events)
	}
	record := domain.RuntimeRecord{
		Key: "inventory/latest", Kind: "inventory",
		Payload: json.RawMessage(`{"revision":1}`), UpdatedAt: base,
	}
	if err := store.SaveRuntimeRecord(ctx, record); err != nil {
		t.Fatalf("SaveRuntimeRecord() error = %v", err)
	}
	record.Payload[2] = 'x'
	storedRecord, err := store.RuntimeRecord(ctx, "inventory/latest")
	if err != nil || string(storedRecord.Payload) != `{"revision":1}` {
		t.Fatalf("RuntimeRecord()=%#v err=%v", storedRecord, err)
	}
	records, err := store.ListRuntimeRecords(ctx, domain.RuntimeRecordFilter{
		Kind: "inventory", KeyPrefix: "inventory/",
	})
	if err != nil || len(records) != 1 || records[0].Key != "inventory/latest" {
		t.Fatalf("ListRuntimeRecords()=%#v err=%v", records, err)
	}

	if _, err := store.Execution(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Execution(missing) error = %v, want ErrNotFound", err)
	}
	if err := store.SaveActionResult(ctx, domain.ActionResultRecord{ActionID: "missing"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("SaveActionResult(missing) error = %v, want ErrNotFound", err)
	}
}

func TestStoreTransactions(t *testing.T) {
	t.Parallel()

	factories := map[string]func(*testing.T) Store{
		"memory": func(*testing.T) Store {
			return NewMemory()
		},
		"sqlite": func(t *testing.T) Store {
			store, err := OpenSQLite(context.Background(), filepath.Join(t.TempDir(), "transactions.db"))
			if err != nil {
				t.Fatalf("OpenSQLite() error = %v", err)
			}
			t.Cleanup(func() { _ = store.Close() })
			return store
		},
	}

	for name, factory := range factories {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			store := factory(t)
			ctx := context.Background()
			rollback := errors.New("rollback")
			err := store.WithinTransaction(ctx, func(tx Store) error {
				if err := tx.SaveQuote(ctx, domain.MarketQuote{
					ItemID: "rolled-back", ObservedAt: time.Now().UTC(),
				}); err != nil {
					return err
				}
				return rollback
			})
			if !errors.Is(err, rollback) {
				t.Fatalf("WithinTransaction(rollback) error = %v", err)
			}
			if _, err := store.LatestQuote(ctx, "rolled-back"); !errors.Is(err, ErrNotFound) {
				t.Fatalf("LatestQuote(rolled-back) error = %v, want ErrNotFound", err)
			}

			err = store.WithinTransaction(ctx, func(tx Store) error {
				if err := tx.SaveAction(ctx, domain.ActionRecord{ID: "committed"}); err != nil {
					return err
				}
				return tx.WithinTransaction(ctx, func(nested Store) error {
					return nested.SaveEvent(ctx, domain.AgentEventRecord{ID: "event-committed"})
				})
			})
			if err != nil {
				t.Fatalf("WithinTransaction(commit) error = %v", err)
			}
			if _, err := store.Action(ctx, "committed"); err != nil {
				t.Fatalf("Action(committed) error = %v", err)
			}
			if _, err := store.Event(ctx, "event-committed"); err != nil {
				t.Fatalf("Event(event-committed) error = %v", err)
			}
		})
	}
}

func TestSQLitePersistsAcrossReopenAndMigratesOnce(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	databasePath := filepath.Join(t.TempDir(), "durable", "state.db")
	store, err := OpenSQLite(ctx, databasePath)
	if err != nil {
		t.Fatalf("OpenSQLite(first) error = %v", err)
	}
	quote := domain.MarketQuote{
		ItemID:     "durable-item",
		BuyPrice:   123,
		SalePrice:  456,
		ObservedAt: time.Date(2026, time.July, 29, 12, 0, 0, 0, time.UTC),
		Confidence: 0.99,
	}
	if err := store.SaveQuote(ctx, quote); err != nil {
		t.Fatalf("SaveQuote() error = %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close(first) error = %v", err)
	}

	reopened, err := OpenSQLite(ctx, databasePath)
	if err != nil {
		t.Fatalf("OpenSQLite(second) error = %v", err)
	}
	defer reopened.Close()
	got, err := reopened.LatestQuote(ctx, quote.ItemID)
	if err != nil {
		t.Fatalf("LatestQuote() error = %v", err)
	}
	if !reflect.DeepEqual(got, quote) {
		t.Fatalf("LatestQuote() = %#v, want %#v", got, quote)
	}
	var migrationCount int
	if err := reopened.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM schema_migrations").Scan(&migrationCount); err != nil {
		t.Fatalf("count migrations: %v", err)
	}
	if migrationCount != 10 {
		t.Fatalf("schema_migrations count = %d, want 10", migrationCount)
	}
}

func TestStoreRejectsInvalidPaginationAndCancelledContext(t *testing.T) {
	t.Parallel()
	store := NewMemory()
	if _, err := store.ListQuotes(context.Background(), domain.QuoteFilter{Limit: -1}); err == nil {
		t.Fatal("ListQuotes(negative limit) error = nil")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := store.SaveQuote(ctx, domain.MarketQuote{ItemID: "item"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("SaveQuote(cancelled) error = %v, want context.Canceled", err)
	}
}

func TestStoreImmutableRecordsAreIdempotentAndRejectConflicts(t *testing.T) {
	for name, factory := range repositoryTestFactories() {
		t.Run(name, func(t *testing.T) {
			store := factory(t)
			ctx := context.Background()
			base := time.Date(2026, time.July, 30, 1, 2, 3, 4, time.UTC)

			observation := domain.Observation{
				FrameID:    701,
				State:      domain.StateMarketResults,
				Elements:   []domain.UIElement{},
				Values:     map[string]domain.Value{},
				Confidence: 0.91,
				CreatedAt:  base,
			}
			if err := store.SaveObservation(ctx, observation); err != nil {
				t.Fatalf("первое сохранение наблюдения: %v", err)
			}
			if err := store.SaveObservation(ctx, observation); err != nil {
				t.Fatalf("идемпотентное сохранение наблюдения: %v", err)
			}
			conflictingObservation := observation
			conflictingObservation.State = domain.StateInventory
			if err := store.SaveObservation(ctx, conflictingObservation); !errors.Is(err, ErrConflict) {
				t.Fatalf("конфликт наблюдения: ошибка = %v, ожидалась ErrConflict", err)
			}
			storedObservation, err := store.Observation(ctx, observation.FrameID)
			if err != nil {
				t.Fatalf("чтение наблюдения после конфликта: %v", err)
			}
			if !observationsEqual(storedObservation, observation) {
				t.Fatalf("наблюдение было перезаписано: %#v", storedObservation)
			}

			action := domain.ActionRecord{
				ID:            "immutable-action",
				SessionID:     "session-1",
				AgentID:       "agent-1",
				BasedOnFrame:  observation.FrameID,
				ExpectedState: domain.StateMarketResults,
				Deadline:      base.Add(time.Minute),
				Kind:          "CLICK",
				ActionPayload: json.RawMessage(`{"kind":"CLICK"}`),
				RequestedAt:   base,
			}
			if err := store.SaveAction(ctx, action); err != nil {
				t.Fatalf("первое сохранение действия: %v", err)
			}
			if err := store.SaveAction(ctx, action); err != nil {
				t.Fatalf("идемпотентное сохранение действия: %v", err)
			}
			conflictingAction := action
			conflictingAction.Kind = "SCROLL"
			if err := store.SaveAction(ctx, conflictingAction); !errors.Is(err, ErrConflict) {
				t.Fatalf("конфликт действия: ошибка = %v, ожидалась ErrConflict", err)
			}
			storedAction, err := store.Action(ctx, action.ID)
			if err != nil {
				t.Fatalf("чтение действия после конфликта: %v", err)
			}
			if !actionsEqual(storedAction, action) {
				t.Fatalf("действие было перезаписано: %#v", storedAction)
			}

			event := domain.AgentEventRecord{
				ID:        "immutable-event",
				SessionID: "session-1",
				AgentID:   "agent-1",
				Kind:      "AUDIT",
				Severity:  "INFO",
				Message:   "исходное событие",
				Payload:   json.RawMessage{},
				CreatedAt: base,
			}
			if err := store.SaveEvent(ctx, event); err != nil {
				t.Fatalf("первое сохранение события: %v", err)
			}
			if err := store.SaveEvent(ctx, event); err != nil {
				t.Fatalf("идемпотентное сохранение события: %v", err)
			}
			conflictingEvent := event
			conflictingEvent.Message = "другое событие"
			if err := store.SaveEvent(ctx, conflictingEvent); !errors.Is(err, ErrConflict) {
				t.Fatalf("конфликт события: ошибка = %v, ожидалась ErrConflict", err)
			}
			storedEvent, err := store.Event(ctx, event.ID)
			if err != nil {
				t.Fatalf("чтение события после конфликта: %v", err)
			}
			if !eventsEqual(storedEvent, event) {
				t.Fatalf("событие было перезаписано: %#v", storedEvent)
			}
		})
	}
}

func TestStoreSwallowedNestedErrorRollsBackOuterTransaction(t *testing.T) {
	for name, factory := range repositoryTestFactories() {
		t.Run(name, func(t *testing.T) {
			store := factory(t)
			ctx := context.Background()
			nestedFailure := errors.New("ошибка вложенной транзакции")

			err := store.WithinTransaction(ctx, func(tx Store) error {
				if err := tx.SaveQuote(ctx, domain.MarketQuote{
					ItemID: "outer-quote", ObservedAt: time.Now().UTC(),
				}); err != nil {
					return err
				}
				nestedErr := tx.WithinTransaction(ctx, func(nested Store) error {
					if err := nested.SaveQuote(ctx, domain.MarketQuote{
						ItemID: "nested-quote", ObservedAt: time.Now().UTC(),
					}); err != nil {
						return err
					}
					return nestedFailure
				})
				if !errors.Is(nestedErr, nestedFailure) {
					return nestedErr
				}
				return nil
			})
			if !errors.Is(err, nestedFailure) {
				t.Fatalf("ошибка внешней транзакции = %v, ожидалась вложенная ошибка", err)
			}
			for _, itemID := range []string{"outer-quote", "nested-quote"} {
				if _, err := store.LatestQuote(ctx, itemID); !errors.Is(err, ErrNotFound) {
					t.Fatalf("котировка %q сохранилась после rollback: %v", itemID, err)
				}
			}
		})
	}
}

func TestSQLitePanicRollsBackAndReleasesConnection(t *testing.T) {
	ctx := context.Background()
	store, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "panic.db"))
	if err != nil {
		t.Fatalf("открытие SQLite: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	var panicValue any
	func() {
		defer func() {
			panicValue = recover()
		}()
		_ = store.WithinTransaction(ctx, func(tx Store) error {
			if err := tx.SaveQuote(ctx, domain.MarketQuote{
				ItemID: "panic-quote", ObservedAt: time.Now().UTC(),
			}); err != nil {
				return err
			}
			panic("преднамеренная паника транзакции")
		})
	}()
	if panicValue == nil {
		t.Fatal("паника callback-а транзакции не была проброшена")
	}

	operationCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if _, err := store.LatestQuote(operationCtx, "panic-quote"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("данные panic-транзакции не были отменены: %v", err)
	}
	if err := store.SaveQuote(operationCtx, domain.MarketQuote{
		ItemID: "after-panic", ObservedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("SQLite не работает после panic rollback: %v", err)
	}
}

func TestMemoryCancelledTransactionDoesNotPublish(t *testing.T) {
	store := NewMemory()
	ctx, cancel := context.WithCancel(context.Background())
	err := store.WithinTransaction(ctx, func(tx Store) error {
		if err := tx.SaveQuote(ctx, domain.MarketQuote{
			ItemID: "cancelled-quote", ObservedAt: time.Now().UTC(),
		}); err != nil {
			return err
		}
		cancel()
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("ошибка отменённой транзакции = %v, ожидалась context.Canceled", err)
	}
	if _, err := store.LatestQuote(context.Background(), "cancelled-quote"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("отменённая транзакция была опубликована: %v", err)
	}
}

func TestMemoryPaginationDoesNotOverflow(t *testing.T) {
	store := NewMemory()
	ctx := context.Background()
	for _, itemID := range []string{"first", "second"} {
		if err := store.SaveQuote(ctx, domain.MarketQuote{
			ItemID: itemID, ObservedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatalf("сохранение котировки %q: %v", itemID, err)
		}
	}
	values, err := store.ListQuotes(ctx, domain.QuoteFilter{Limit: math.MaxInt, Offset: 1})
	if err != nil {
		t.Fatalf("пагинация с MaxInt: %v", err)
	}
	if len(values) != 1 {
		t.Fatalf("пагинация с MaxInt вернула %d записей, ожидалась 1", len(values))
	}
}

func TestMigrationNormalizesLegacyEmptyActionPayload(t *testing.T) {
	ctx := context.Background()
	databasePath := filepath.Join(t.TempDir(), "legacy-action.db")
	legacyDB, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatalf("открытие legacy SQLite: %v", err)
	}
	t.Cleanup(func() { _ = legacyDB.Close() })
	if _, err := legacyDB.ExecContext(ctx, `
		CREATE TABLE schema_migrations (
			version INTEGER PRIMARY KEY,
			name TEXT NOT NULL,
			applied_at TEXT NOT NULL
		)
	`); err != nil {
		t.Fatalf("создание schema_migrations: %v", err)
	}
	legacyMigrations := []string{
		"migrations/001_initial.sql",
		"migrations/002_protocol_audit.sql",
		"migrations/003_action_payload.sql",
		"migrations/004_action_class.sql",
		"migrations/005_runtime_state.sql",
	}
	for index, name := range legacyMigrations {
		body, err := migrationFiles.ReadFile(name)
		if err != nil {
			t.Fatalf("чтение миграции %q: %v", name, err)
		}
		if _, err := legacyDB.ExecContext(ctx, string(body)); err != nil {
			t.Fatalf("применение миграции %q: %v", name, err)
		}
		if _, err := legacyDB.ExecContext(ctx, `
			INSERT INTO schema_migrations(version, name, applied_at)
			VALUES (?, ?, ?)
		`, index+1, filepath.Base(name), encodeTime(time.Now().UTC())); err != nil {
			t.Fatalf("запись миграции %q: %v", name, err)
		}
	}
	if _, err := legacyDB.ExecContext(ctx, `
		INSERT INTO action_requests(
			id, session_id, agent_id, based_on_frame, expected_state,
			expected_width, expected_height, expected_dpi_percent, deadline,
			action_class, action_kind, point_x, point_y, value, delta,
			action_json, requested_at
		) VALUES (
			'legacy-action', '', '', 1, '', 0, 0, 0, '',
			'NAVIGATION', '', NULL, NULL, '', 0, X'', ''
		)
	`); err != nil {
		t.Fatalf("создание legacy-действия: %v", err)
	}
	if err := legacyDB.Close(); err != nil {
		t.Fatalf("закрытие legacy SQLite: %v", err)
	}

	store, err := OpenSQLite(ctx, databasePath)
	if err != nil {
		t.Fatalf("открытие SQLite с миграцией 006: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	action, err := store.Action(ctx, "legacy-action")
	if err != nil {
		t.Fatalf("чтение нормализованного действия: %v", err)
	}
	if string(action.ActionPayload) != `{}` {
		t.Fatalf("action_json после миграции = %q, ожидалось {}", action.ActionPayload)
	}
	if _, err := store.db.ExecContext(ctx, `
		INSERT INTO action_requests(
			id, session_id, agent_id, based_on_frame, expected_state,
			expected_width, expected_height, expected_dpi_percent, deadline,
			action_class, action_kind, point_x, point_y, value, delta, requested_at
		) VALUES (
			'legacy-default-action', '', '', 2, '', 0, 0, 0, '',
			'NAVIGATION', '', NULL, NULL, '', 0, ''
		)
	`); err != nil {
		t.Fatalf("создание действия со старым значением по умолчанию: %v", err)
	}
	defaultAction, err := store.Action(ctx, "legacy-default-action")
	if err != nil {
		t.Fatalf("чтение действия со старым значением по умолчанию: %v", err)
	}
	if string(defaultAction.ActionPayload) != `{}` {
		t.Fatalf("action_json со старым default = %q, ожидалось {}", defaultAction.ActionPayload)
	}
}

func TestSQLiteUsesFullSynchronousMode(t *testing.T) {
	store, err := OpenSQLite(context.Background(), filepath.Join(t.TempDir(), "full-sync.db"))
	if err != nil {
		t.Fatalf("открытие SQLite: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	var mode int
	if err := store.db.QueryRow("PRAGMA synchronous").Scan(&mode); err != nil {
		t.Fatalf("чтение PRAGMA synchronous: %v", err)
	}
	if mode != 2 {
		t.Fatalf("PRAGMA synchronous = %d, ожидался FULL (2)", mode)
	}
}

func repositoryTestFactories() map[string]func(*testing.T) Store {
	return map[string]func(*testing.T) Store{
		"memory": func(*testing.T) Store {
			return NewMemory()
		},
		"sqlite": func(t *testing.T) Store {
			store, err := OpenSQLite(context.Background(), filepath.Join(t.TempDir(), "repository.db"))
			if err != nil {
				t.Fatalf("открытие SQLite: %v", err)
			}
			t.Cleanup(func() { _ = store.Close() })
			return store
		},
	}
}

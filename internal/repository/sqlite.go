package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/arena-trading-agent/arena-trading-agent/internal/domain"
	_ "modernc.org/sqlite"
)

const storageTimeLayout = "2006-01-02T15:04:05.000000000Z07:00"

type sqlRunner interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// SQLite is the production Store backed by a pure-Go SQLite driver.
type SQLite struct {
	db      *sql.DB
	tx      *sql.Tx
	txState *transactionState
}

var _ Store = (*SQLite)(nil)

// OpenSQLite opens or creates a database, configures safe local-runtime
// pragmas and applies all embedded migrations.
func OpenSQLite(ctx context.Context, databasePath string) (*SQLite, error) {
	const methodCtx = "repository.OpenSQLite"

	if err := checkContext(ctx); err != nil {
		return nil, fmt.Errorf("%s: не удалось проверить контекст: %w", methodCtx, err)
	}
	if strings.TrimSpace(databasePath) == "" {
		return nil, fmt.Errorf("%s: путь к базе данных SQLite обязателен", methodCtx)
	}
	if databasePath != ":memory:" && !strings.HasPrefix(databasePath, "file:") {
		parent := filepath.Dir(databasePath)
		if parent != "." {
			if err := os.MkdirAll(parent, 0o750); err != nil {
				return nil, fmt.Errorf("%s: не удалось создать каталог SQLite: %w", methodCtx, err)
			}
		}
	}

	db, err := sql.Open("sqlite", databasePath)
	if err != nil {
		return nil, fmt.Errorf("%s: не удалось открыть SQLite: %w", methodCtx, err)
	}
	// One long-lived connection gives deterministic pragma and :memory:
	// behavior. Controller writes are intentionally serialized.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	closeOnError := func(cause error) (*SQLite, error) {
		_ = db.Close()
		return nil, cause
	}
	if err := db.PingContext(ctx); err != nil {
		return closeOnError(fmt.Errorf("%s: не удалось проверить соединение с SQLite: %w", methodCtx, err))
	}
	for _, pragma := range []string{
		"PRAGMA foreign_keys = ON",
		"PRAGMA busy_timeout = 5000",
		"PRAGMA journal_mode = WAL",
		"PRAGMA synchronous = FULL",
	} {
		if _, err := db.ExecContext(ctx, pragma); err != nil {
			return closeOnError(fmt.Errorf("%s: не удалось применить настройку SQLite %q: %w", methodCtx, pragma, err))
		}
	}
	if err := applyMigrations(ctx, db); err != nil {
		return closeOnError(fmt.Errorf("%s: не удалось применить миграции: %w", methodCtx, err))
	}
	return &SQLite{db: db}, nil
}

// Close closes the underlying database. Transaction-scoped handles cannot
// close the owner connection.
func (s *SQLite) Close() error {
	const methodCtx = "repository.SQLite.Close"

	if s == nil || s.db == nil || s.tx != nil {
		return nil
	}
	if err := s.db.Close(); err != nil {
		return fmt.Errorf("%s: не удалось закрыть SQLite: %w", methodCtx, err)
	}
	return nil
}

func (s *SQLite) runner() sqlRunner {
	if s.tx != nil {
		return s.tx
	}
	return s.db
}

// WithinTransaction executes fn atomically. Nested calls join the current
// SQL transaction, so an error still aborts the outer unit of work.
func (s *SQLite) WithinTransaction(ctx context.Context, fn func(Store) error) error {
	const methodCtx = "repository.SQLite.WithinTransaction"

	if err := checkContext(ctx); err != nil {
		wrapped := fmt.Errorf("%s: не удалось проверить контекст: %w", methodCtx, err)
		s.txState.markFailed(wrapped)
		return wrapped
	}
	if fn == nil {
		return nil
	}
	if s.tx != nil {
		defer func() {
			const methodCtx = "repository.SQLite.WithinTransaction.recover"

			if panicValue := recover(); panicValue != nil {
				s.txState.markFailed(fmt.Errorf(
					"%s: во вложенной транзакции произошла паника",
					methodCtx,
				))
				panic(panicValue)
			}
		}()
		if err := fn(s); err != nil {
			wrapped := fmt.Errorf("%s: вложенная операция транзакции завершилась ошибкой: %w", methodCtx, err)
			s.txState.markFailed(wrapped)
			return wrapped
		}
		if failure := s.txState.failedWith(); failure != nil {
			return fmt.Errorf("%s: транзакция ранее помечена для отката: %w", methodCtx, failure)
		}
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("%s: не удалось начать транзакцию SQLite: %w", methodCtx, err)
	}
	defer func() {
		_ = tx.Rollback()
	}()
	state := &transactionState{}
	transactionStore := &SQLite{db: s.db, tx: tx, txState: state}
	if err := fn(transactionStore); err != nil {
		if rollbackErr := tx.Rollback(); rollbackErr != nil {
			return errors.Join(
				fmt.Errorf("%s: операция транзакции завершилась ошибкой: %w", methodCtx, err),
				fmt.Errorf("%s: не удалось откатить транзакцию SQLite: %w", methodCtx, rollbackErr),
			)
		}
		return fmt.Errorf("%s: операция транзакции завершилась ошибкой: %w", methodCtx, err)
	}
	if failure := state.failedWith(); failure != nil {
		rollbackCause := fmt.Errorf(
			"%s: транзакция помечена для отката после ошибки вложенной операции: %w",
			methodCtx,
			failure,
		)
		if rollbackErr := tx.Rollback(); rollbackErr != nil {
			return errors.Join(
				rollbackCause,
				fmt.Errorf("%s: не удалось откатить помеченную транзакцию SQLite: %w", methodCtx, rollbackErr),
			)
		}
		return rollbackCause
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("%s: не удалось зафиксировать транзакцию SQLite: %w", methodCtx, err)
	}
	return nil
}

// SaveObservation persists the normalized observation for a frame.
func (s *SQLite) SaveObservation(ctx context.Context, value domain.Observation) error {
	const methodCtx = "repository.SQLite.SaveObservation"

	if err := checkContext(ctx); err != nil {
		return fmt.Errorf("%s: не удалось проверить контекст: %w", methodCtx, err)
	}
	frameID, err := databaseUint64("идентификатор кадра", value.FrameID)
	if err != nil {
		return fmt.Errorf("%s: некорректный идентификатор кадра: %w", methodCtx, err)
	}
	elements, err := json.Marshal(value.Elements)
	if err != nil {
		return fmt.Errorf("%s: не удалось сериализовать элементы наблюдения: %w", methodCtx, err)
	}
	values, err := json.Marshal(value.Values)
	if err != nil {
		return fmt.Errorf("%s: не удалось сериализовать значения наблюдения: %w", methodCtx, err)
	}
	result, err := s.runner().ExecContext(ctx, `
		INSERT INTO observations(
			frame_id, state, elements_json, values_json, confidence, created_at
		) VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(frame_id) DO NOTHING
	`, frameID, value.State, elements, values, value.Confidence, encodeTime(value.CreatedAt))
	if err != nil {
		return fmt.Errorf("%s: не удалось сохранить наблюдение кадра %d: %w", methodCtx, value.FrameID, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("%s: не удалось определить результат сохранения наблюдения кадра %d: %w", methodCtx, value.FrameID, err)
	}
	if affected == 0 {
		existing, err := s.Observation(ctx, value.FrameID)
		if err != nil {
			return fmt.Errorf("%s: не удалось проверить существующее наблюдение кадра %d: %w", methodCtx, value.FrameID, err)
		}
		if !observationsEqual(existing, value) {
			return fmt.Errorf(
				"%s: наблюдение кадра %d конфликтует с сохранённой записью: %w",
				methodCtx,
				value.FrameID,
				conflict("наблюдение", frameIDString(value.FrameID)),
			)
		}
	}
	return nil
}

// Observation returns an observation by frame identifier.
func (s *SQLite) Observation(ctx context.Context, frameID uint64) (domain.Observation, error) {
	const methodCtx = "repository.SQLite.Observation"

	if err := checkContext(ctx); err != nil {
		return domain.Observation{}, fmt.Errorf("%s: не удалось проверить контекст: %w", methodCtx, err)
	}
	databaseID, err := databaseUint64("идентификатор кадра", frameID)
	if err != nil {
		return domain.Observation{}, fmt.Errorf("%s: некорректный идентификатор кадра: %w", methodCtx, err)
	}
	value, err := scanObservation(s.runner().QueryRowContext(ctx, `
		SELECT frame_id, state, elements_json, values_json, confidence, created_at
		FROM observations
		WHERE frame_id = ?
	`, databaseID))
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Observation{}, fmt.Errorf(
			"%s: наблюдение для кадра %d не найдено: %w",
			methodCtx,
			frameID,
			notFound("наблюдение", frameIDString(frameID)),
		)
	}
	if err != nil {
		return domain.Observation{}, fmt.Errorf("%s: не удалось прочитать наблюдение кадра %d: %w", methodCtx, frameID, err)
	}
	return value, nil
}

// ListObservations returns observations newest first.
func (s *SQLite) ListObservations(ctx context.Context, filter domain.ObservationFilter) ([]domain.Observation, error) {
	const methodCtx = "repository.SQLite.ListObservations"

	if err := checkContext(ctx); err != nil {
		return nil, fmt.Errorf("%s: не удалось проверить контекст: %w", methodCtx, err)
	}
	query := `
		SELECT frame_id, state, elements_json, values_json, confidence, created_at
		FROM observations`
	conditions := make([]string, 0, 3)
	args := make([]any, 0, 5)
	if filter.State != "" {
		conditions = append(conditions, "state = ?")
		args = append(args, filter.State)
	}
	if !filter.Since.IsZero() {
		conditions = append(conditions, "created_at >= ?")
		args = append(args, encodeTime(filter.Since))
	}
	if !filter.Until.IsZero() {
		conditions = append(conditions, "created_at <= ?")
		args = append(args, encodeTime(filter.Until))
	}
	query = appendConditions(query, conditions)
	query += " ORDER BY created_at DESC, frame_id DESC"
	query, args, err := appendPagination(query, args, filter.Limit, filter.Offset)
	if err != nil {
		return nil, fmt.Errorf("%s: некорректные параметры пагинации: %w", methodCtx, err)
	}
	rows, err := s.runner().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("%s: не удалось получить список наблюдений: %w", methodCtx, err)
	}
	defer rows.Close()
	result := make([]domain.Observation, 0)
	for rows.Next() {
		value, err := scanObservation(rows)
		if err != nil {
			return nil, fmt.Errorf("%s: не удалось прочитать строку наблюдения: %w", methodCtx, err)
		}
		result = append(result, value)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%s: ошибка перебора наблюдений: %w", methodCtx, err)
	}
	return result, nil
}

// SaveQuote appends a market quote.
func (s *SQLite) SaveQuote(ctx context.Context, value domain.MarketQuote) error {
	const methodCtx = "repository.SQLite.SaveQuote"

	if err := checkContext(ctx); err != nil {
		return fmt.Errorf("%s: не удалось проверить контекст: %w", methodCtx, err)
	}
	if err := requireID("предмет", value.ItemID); err != nil {
		return fmt.Errorf("%s: некорректная котировка: %w", methodCtx, err)
	}
	_, err := s.runner().ExecContext(ctx, `
		INSERT INTO market_quotes(
			item_id, buy_price, sale_price, observed_at, confidence
		) VALUES (?, ?, ?, ?, ?)
	`, value.ItemID, value.BuyPrice, value.SalePrice, encodeTime(value.ObservedAt), value.Confidence)
	if err != nil {
		return fmt.Errorf("%s: не удалось сохранить котировку предмета %q: %w", methodCtx, value.ItemID, err)
	}
	return nil
}

// LatestQuote returns the newest quote for an item.
func (s *SQLite) LatestQuote(ctx context.Context, itemID string) (domain.MarketQuote, error) {
	const methodCtx = "repository.SQLite.LatestQuote"

	if err := checkContext(ctx); err != nil {
		return domain.MarketQuote{}, fmt.Errorf("%s: не удалось проверить контекст: %w", methodCtx, err)
	}
	if err := requireID("предмет", itemID); err != nil {
		return domain.MarketQuote{}, fmt.Errorf("%s: некорректный запрос котировки: %w", methodCtx, err)
	}
	value, err := scanQuote(s.runner().QueryRowContext(ctx, `
		SELECT item_id, buy_price, sale_price, observed_at, confidence
		FROM market_quotes
		WHERE item_id = ?
		ORDER BY observed_at DESC, sequence DESC
		LIMIT 1
	`, itemID))
	if errors.Is(err, sql.ErrNoRows) {
		return domain.MarketQuote{}, fmt.Errorf(
			"%s: котировка предмета %q не найдена: %w",
			methodCtx,
			itemID,
			notFound("котировка предмета", itemID),
		)
	}
	if err != nil {
		return domain.MarketQuote{}, fmt.Errorf("%s: не удалось прочитать последнюю котировку предмета %q: %w", methodCtx, itemID, err)
	}
	return value, nil
}

// ListQuotes returns market quote history newest first.
func (s *SQLite) ListQuotes(ctx context.Context, filter domain.QuoteFilter) ([]domain.MarketQuote, error) {
	const methodCtx = "repository.SQLite.ListQuotes"

	if err := checkContext(ctx); err != nil {
		return nil, fmt.Errorf("%s: не удалось проверить контекст: %w", methodCtx, err)
	}
	query := `
		SELECT item_id, buy_price, sale_price, observed_at, confidence
		FROM market_quotes`
	conditions := make([]string, 0, 3)
	args := make([]any, 0, 5)
	if filter.ItemID != "" {
		conditions = append(conditions, "item_id = ?")
		args = append(args, filter.ItemID)
	}
	if !filter.Since.IsZero() {
		conditions = append(conditions, "observed_at >= ?")
		args = append(args, encodeTime(filter.Since))
	}
	if !filter.Until.IsZero() {
		conditions = append(conditions, "observed_at <= ?")
		args = append(args, encodeTime(filter.Until))
	}
	query = appendConditions(query, conditions)
	query += " ORDER BY observed_at DESC, sequence DESC"
	query, args, err := appendPagination(query, args, filter.Limit, filter.Offset)
	if err != nil {
		return nil, fmt.Errorf("%s: некорректные параметры пагинации: %w", methodCtx, err)
	}
	rows, err := s.runner().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("%s: не удалось получить список рыночных котировок: %w", methodCtx, err)
	}
	defer rows.Close()
	result := make([]domain.MarketQuote, 0)
	for rows.Next() {
		value, err := scanQuote(rows)
		if err != nil {
			return nil, fmt.Errorf("%s: не удалось прочитать строку рыночной котировки: %w", methodCtx, err)
		}
		result = append(result, value)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%s: ошибка перебора рыночных котировок: %w", methodCtx, err)
	}
	return result, nil
}

// SaveTradeQuote appends a complete economic quote.
func (s *SQLite) SaveTradeQuote(ctx context.Context, value domain.TradeQuote) error {
	const methodCtx = "repository.SQLite.SaveTradeQuote"

	if err := checkContext(ctx); err != nil {
		return fmt.Errorf("%s: не удалось проверить контекст: %w", methodCtx, err)
	}
	if err := requireID("предмет", value.ItemID); err != nil {
		return fmt.Errorf("%s: некорректная торговая котировка: %w", methodCtx, err)
	}
	_, err := s.runner().ExecContext(ctx, `
		INSERT INTO trade_quotes(
			item_id, purchase_price, sale_price, sale_commission, listing_fee,
			observed_at, confidence, liquidity_score, price_volatility, resale_known
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, value.ItemID, value.PurchasePrice, value.SalePrice, value.SaleCommission,
		value.ListingFee, encodeTime(value.ObservedAt), value.Confidence,
		value.LiquidityScore, value.PriceVolatility, value.ResaleKnown)
	if err != nil {
		return fmt.Errorf("%s: не удалось сохранить торговую котировку предмета %q: %w", methodCtx, value.ItemID, err)
	}
	return nil
}

// LatestTradeQuote returns the newest complete quote for an item.
func (s *SQLite) LatestTradeQuote(ctx context.Context, itemID string) (domain.TradeQuote, error) {
	const methodCtx = "repository.SQLite.LatestTradeQuote"

	if err := checkContext(ctx); err != nil {
		return domain.TradeQuote{}, fmt.Errorf("%s: не удалось проверить контекст: %w", methodCtx, err)
	}
	if err := requireID("предмет", itemID); err != nil {
		return domain.TradeQuote{}, fmt.Errorf("%s: некорректный запрос торговой котировки: %w", methodCtx, err)
	}
	value, err := scanTradeQuote(s.runner().QueryRowContext(ctx, `
		SELECT item_id, purchase_price, sale_price, sale_commission, listing_fee,
		       observed_at, confidence, liquidity_score, price_volatility, resale_known
		FROM trade_quotes
		WHERE item_id = ?
		ORDER BY observed_at DESC, sequence DESC
		LIMIT 1
	`, itemID))
	if errors.Is(err, sql.ErrNoRows) {
		return domain.TradeQuote{}, fmt.Errorf(
			"%s: торговая котировка предмета %q не найдена: %w",
			methodCtx,
			itemID,
			notFound("торговая котировка предмета", itemID),
		)
	}
	if err != nil {
		return domain.TradeQuote{}, fmt.Errorf("%s: не удалось прочитать торговую котировку предмета %q: %w", methodCtx, itemID, err)
	}
	return value, nil
}

// ListTradeQuotes returns complete quotes newest first.
func (s *SQLite) ListTradeQuotes(
	ctx context.Context,
	filter domain.TradeQuoteFilter,
) ([]domain.TradeQuote, error) {
	const methodCtx = "repository.SQLite.ListTradeQuotes"

	if err := checkContext(ctx); err != nil {
		return nil, fmt.Errorf("%s: не удалось проверить контекст: %w", methodCtx, err)
	}
	query := `
		SELECT item_id, purchase_price, sale_price, sale_commission, listing_fee,
		       observed_at, confidence, liquidity_score, price_volatility, resale_known
		FROM trade_quotes`
	conditions := make([]string, 0, 3)
	args := make([]any, 0, 5)
	if filter.ItemID != "" {
		conditions = append(conditions, "item_id = ?")
		args = append(args, filter.ItemID)
	}
	if !filter.Since.IsZero() {
		conditions = append(conditions, "observed_at >= ?")
		args = append(args, encodeTime(filter.Since))
	}
	if !filter.Until.IsZero() {
		conditions = append(conditions, "observed_at <= ?")
		args = append(args, encodeTime(filter.Until))
	}
	query = appendConditions(query, conditions)
	query += " ORDER BY observed_at DESC, sequence DESC"
	query, args, err := appendPagination(query, args, filter.Limit, filter.Offset)
	if err != nil {
		return nil, fmt.Errorf("%s: некорректные параметры пагинации: %w", methodCtx, err)
	}
	rows, err := s.runner().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("%s: не удалось получить список торговых котировок: %w", methodCtx, err)
	}
	defer rows.Close()
	result := make([]domain.TradeQuote, 0)
	for rows.Next() {
		value, err := scanTradeQuote(rows)
		if err != nil {
			return nil, fmt.Errorf("%s: не удалось прочитать строку торговой котировки: %w", methodCtx, err)
		}
		result = append(result, value)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%s: ошибка перебора торговых котировок: %w", methodCtx, err)
	}
	return result, nil
}

// SaveExecution atomically updates the current snapshot and appends history.
func (s *SQLite) SaveExecution(ctx context.Context, value domain.TradeExecution) error {
	const methodCtx = "repository.SQLite.SaveExecution"

	if err := checkContext(ctx); err != nil {
		return fmt.Errorf("%s: не удалось проверить контекст: %w", methodCtx, err)
	}
	if err := requireID("исполнение сделки", value.ID); err != nil {
		return fmt.Errorf("%s: некорректное исполнение сделки: %w", methodCtx, err)
	}
	if s.tx == nil {
		if err := s.WithinTransaction(ctx, func(store Store) error {
			return store.SaveExecution(ctx, value)
		}); err != nil {
			return fmt.Errorf("%s: не удалось сохранить исполнение в транзакции: %w", methodCtx, err)
		}
		return nil
	}
	_, err := s.runner().ExecContext(ctx, `
		INSERT INTO trade_executions(
			id, opportunity_id, status, current_step, reserved,
			started_at, updated_at, failure
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			opportunity_id = excluded.opportunity_id,
			status = excluded.status,
			current_step = excluded.current_step,
			reserved = excluded.reserved,
			started_at = excluded.started_at,
			updated_at = excluded.updated_at,
			failure = excluded.failure
	`, executionArgs(value)...)
	if err != nil {
		return fmt.Errorf("%s: не удалось сохранить исполнение сделки %q: %w", methodCtx, value.ID, err)
	}
	_, err = s.runner().ExecContext(ctx, `
		INSERT INTO trade_execution_history(
			execution_id, opportunity_id, status, current_step, reserved,
			started_at, updated_at, failure
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`, executionArgs(value)...)
	if err != nil {
		return fmt.Errorf("%s: не удалось добавить исполнение сделки %q в историю: %w", methodCtx, value.ID, err)
	}
	return nil
}

// Execution returns the current execution snapshot.
func (s *SQLite) Execution(ctx context.Context, id string) (domain.TradeExecution, error) {
	const methodCtx = "repository.SQLite.Execution"

	if err := checkContext(ctx); err != nil {
		return domain.TradeExecution{}, fmt.Errorf("%s: не удалось проверить контекст: %w", methodCtx, err)
	}
	if err := requireID("исполнение сделки", id); err != nil {
		return domain.TradeExecution{}, fmt.Errorf("%s: некорректный запрос исполнения сделки: %w", methodCtx, err)
	}
	value, err := scanExecution(s.runner().QueryRowContext(ctx, `
		SELECT id, opportunity_id, status, current_step, reserved,
		       started_at, updated_at, failure
		FROM trade_executions
		WHERE id = ?
	`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return domain.TradeExecution{}, fmt.Errorf(
			"%s: исполнение сделки %q не найдено: %w",
			methodCtx,
			id,
			notFound("исполнение сделки", id),
		)
	}
	if err != nil {
		return domain.TradeExecution{}, fmt.Errorf("%s: не удалось прочитать исполнение сделки %q: %w", methodCtx, id, err)
	}
	return value, nil
}

// ListExecutions returns current execution snapshots newest first.
func (s *SQLite) ListExecutions(ctx context.Context, filter domain.ExecutionFilter) ([]domain.TradeExecution, error) {
	const methodCtx = "repository.SQLite.ListExecutions"

	if err := checkContext(ctx); err != nil {
		return nil, fmt.Errorf("%s: не удалось проверить контекст: %w", methodCtx, err)
	}
	query := `
		SELECT id, opportunity_id, status, current_step, reserved,
		       started_at, updated_at, failure
		FROM trade_executions`
	conditions := make([]string, 0, 4)
	args := make([]any, 0, 6)
	if filter.OpportunityID != "" {
		conditions = append(conditions, "opportunity_id = ?")
		args = append(args, filter.OpportunityID)
	}
	if filter.Status != "" {
		conditions = append(conditions, "status = ?")
		args = append(args, filter.Status)
	}
	if !filter.Since.IsZero() {
		conditions = append(conditions, "updated_at >= ?")
		args = append(args, encodeTime(filter.Since))
	}
	if !filter.Until.IsZero() {
		conditions = append(conditions, "updated_at <= ?")
		args = append(args, encodeTime(filter.Until))
	}
	query = appendConditions(query, conditions)
	query += " ORDER BY updated_at DESC, id DESC"
	query, args, err := appendPagination(query, args, filter.Limit, filter.Offset)
	if err != nil {
		return nil, fmt.Errorf("%s: некорректные параметры пагинации: %w", methodCtx, err)
	}
	rows, err := s.runner().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("%s: не удалось получить список исполнений сделок: %w", methodCtx, err)
	}
	defer rows.Close()
	result := make([]domain.TradeExecution, 0)
	for rows.Next() {
		value, err := scanExecution(rows)
		if err != nil {
			return nil, fmt.Errorf("%s: не удалось прочитать строку исполнения сделки: %w", methodCtx, err)
		}
		result = append(result, value)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%s: ошибка перебора исполнений сделок: %w", methodCtx, err)
	}
	return result, nil
}

// ExecutionHistory returns execution snapshots in reverse save order.
func (s *SQLite) ExecutionHistory(ctx context.Context, id string, limit int) ([]domain.TradeExecution, error) {
	const methodCtx = "repository.SQLite.ExecutionHistory"

	if err := checkContext(ctx); err != nil {
		return nil, fmt.Errorf("%s: не удалось проверить контекст: %w", methodCtx, err)
	}
	if err := requireID("исполнение сделки", id); err != nil {
		return nil, fmt.Errorf("%s: некорректный запрос истории исполнения: %w", methodCtx, err)
	}
	query := `
		SELECT execution_id, opportunity_id, status, current_step, reserved,
		       started_at, updated_at, failure
		FROM trade_execution_history
		WHERE execution_id = ?
		ORDER BY sequence DESC`
	args := []any{id}
	query, args, err := appendPagination(query, args, limit, 0)
	if err != nil {
		return nil, fmt.Errorf("%s: некорректные параметры пагинации: %w", methodCtx, err)
	}
	rows, err := s.runner().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("%s: не удалось прочитать историю исполнения сделки %q: %w", methodCtx, id, err)
	}
	defer rows.Close()
	result := make([]domain.TradeExecution, 0)
	for rows.Next() {
		value, err := scanExecution(rows)
		if err != nil {
			return nil, fmt.Errorf("%s: не удалось прочитать строку истории исполнения: %w", methodCtx, err)
		}
		result = append(result, value)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%s: ошибка перебора истории исполнения: %w", methodCtx, err)
	}
	if len(result) == 0 {
		return nil, fmt.Errorf(
			"%s: история исполнения сделки %q не найдена: %w",
			methodCtx,
			id,
			notFound("исполнение сделки", id),
		)
	}
	return result, nil
}

// SaveAction stores an action request idempotently by identifier.
func (s *SQLite) SaveAction(ctx context.Context, value domain.ActionRecord) error {
	const methodCtx = "repository.SQLite.SaveAction"

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
	basedOnFrame, err := databaseUint64("идентификатор исходного кадра", value.BasedOnFrame)
	if err != nil {
		return fmt.Errorf("%s: некорректный идентификатор исходного кадра: %w", methodCtx, err)
	}
	var pointX, pointY any
	if value.Point != nil {
		pointX, pointY = value.Point.X, value.Point.Y
	}
	basedOnCapturedAt := ""
	if value.BasedOnCapturedAt != nil {
		basedOnCapturedAt = encodeTime(*value.BasedOnCapturedAt)
	}
	result, err := s.runner().ExecContext(ctx, `
		INSERT INTO action_requests(
			id, session_id, agent_id, based_on_frame, based_on_captured_at,
			based_on_frame_digest, frame_basis_json, based_on_state, expected_state,
			min_verification_confidence, expected_width, expected_height,
			expected_dpi_percent, deadline, action_class, action_kind,
			point_x, point_y, value, delta, action_json, requested_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO NOTHING
	`, value.ID, value.SessionID, value.AgentID, basedOnFrame, basedOnCapturedAt,
		value.BasedOnFrameDigest, []byte(value.FrameBasisPayload),
		value.BasedOnState, value.ExpectedState,
		value.MinConfidence, value.ExpectedWidth, value.ExpectedHeight,
		value.ExpectedDPIPercent, encodeTime(value.Deadline), value.Class,
		value.Kind, pointX, pointY, value.Value, value.Delta,
		[]byte(value.ActionPayload), encodeTime(value.RequestedAt))
	if err != nil {
		return fmt.Errorf("%s: не удалось сохранить действие %q: %w", methodCtx, value.ID, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("%s: не удалось определить результат сохранения действия %q: %w", methodCtx, value.ID, err)
	}
	if affected == 0 {
		existing, err := s.Action(ctx, value.ID)
		if err != nil {
			return fmt.Errorf("%s: не удалось проверить существующее действие %q: %w", methodCtx, value.ID, err)
		}
		if !actionsEqual(existing, value) {
			return fmt.Errorf(
				"%s: действие %q конфликтует с сохранённой записью: %w",
				methodCtx,
				value.ID,
				conflict("действие", value.ID),
			)
		}
	}
	return nil
}

// Action returns an action request by identifier.
func (s *SQLite) Action(ctx context.Context, id string) (domain.ActionRecord, error) {
	const methodCtx = "repository.SQLite.Action"

	if err := checkContext(ctx); err != nil {
		return domain.ActionRecord{}, fmt.Errorf("%s: не удалось проверить контекст: %w", methodCtx, err)
	}
	if err := requireID("действие", id); err != nil {
		return domain.ActionRecord{}, fmt.Errorf("%s: некорректный запрос действия: %w", methodCtx, err)
	}
	value, err := scanAction(s.runner().QueryRowContext(ctx, `
		SELECT id, session_id, agent_id, based_on_frame, based_on_captured_at,
		       based_on_frame_digest, frame_basis_json, based_on_state, expected_state,
		       min_verification_confidence, expected_width, expected_height,
		       expected_dpi_percent, deadline, action_class, action_kind,
		       point_x, point_y, value, delta, action_json, requested_at
		FROM action_requests
		WHERE id = ?
	`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return domain.ActionRecord{}, fmt.Errorf(
			"%s: действие %q не найдено: %w",
			methodCtx,
			id,
			notFound("действие", id),
		)
	}
	if err != nil {
		return domain.ActionRecord{}, fmt.Errorf("%s: не удалось прочитать действие %q: %w", methodCtx, id, err)
	}
	return value, nil
}

// ListActions returns action requests newest first.
func (s *SQLite) ListActions(ctx context.Context, filter domain.ActionFilter) ([]domain.ActionRecord, error) {
	const methodCtx = "repository.SQLite.ListActions"

	if err := checkContext(ctx); err != nil {
		return nil, fmt.Errorf("%s: не удалось проверить контекст: %w", methodCtx, err)
	}
	query := `
		SELECT a.id, a.session_id, a.agent_id, a.based_on_frame,
		       a.based_on_captured_at, a.based_on_frame_digest,
		       a.frame_basis_json, a.based_on_state,
		       a.expected_state, a.min_verification_confidence, a.expected_width,
		       a.expected_height, a.expected_dpi_percent, a.deadline,
		       a.action_class, a.action_kind, a.point_x, a.point_y, a.value,
		       a.delta, a.action_json, a.requested_at
		FROM action_requests AS a`
	conditions := make([]string, 0, 6)
	args := make([]any, 0, 8)
	if filter.SessionID != "" {
		conditions = append(conditions, "a.session_id = ?")
		args = append(args, filter.SessionID)
	}
	if filter.AgentID != "" {
		conditions = append(conditions, "a.agent_id = ?")
		args = append(args, filter.AgentID)
	}
	if filter.Kind != "" {
		conditions = append(conditions, "a.action_kind = ?")
		args = append(args, filter.Kind)
	}
	if filter.PendingOnly {
		conditions = append(conditions, "NOT EXISTS (SELECT 1 FROM action_results r WHERE r.action_id = a.id)")
	}
	if !filter.Since.IsZero() {
		conditions = append(conditions, "a.requested_at >= ?")
		args = append(args, encodeTime(filter.Since))
	}
	if !filter.Until.IsZero() {
		conditions = append(conditions, "a.requested_at <= ?")
		args = append(args, encodeTime(filter.Until))
	}
	query = appendConditions(query, conditions)
	query += " ORDER BY a.requested_at DESC, a.id DESC"
	query, args, err := appendPagination(query, args, filter.Limit, filter.Offset)
	if err != nil {
		return nil, fmt.Errorf("%s: некорректные параметры пагинации: %w", methodCtx, err)
	}
	rows, err := s.runner().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("%s: не удалось получить список действий: %w", methodCtx, err)
	}
	defer rows.Close()
	result := make([]domain.ActionRecord, 0)
	for rows.Next() {
		value, err := scanAction(rows)
		if err != nil {
			return nil, fmt.Errorf("%s: не удалось прочитать строку действия: %w", methodCtx, err)
		}
		result = append(result, value)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%s: ошибка перебора действий: %w", methodCtx, err)
	}
	return result, nil
}

// SaveActionResult appends an immutable action result.
func (s *SQLite) SaveActionResult(ctx context.Context, value domain.ActionResultRecord) error {
	const methodCtx = "repository.SQLite.SaveActionResult"

	if err := checkContext(ctx); err != nil {
		return fmt.Errorf("%s: не удалось проверить контекст: %w", methodCtx, err)
	}
	if err := requireID("действие", value.ActionID); err != nil {
		return fmt.Errorf("%s: некорректный результат действия: %w", methodCtx, err)
	}
	if err := validateActionResult(value); err != nil {
		return fmt.Errorf("%s: некорректный результат действия: %w", methodCtx, err)
	}
	resultFrame, err := databaseUint64("идентификатор результирующего кадра", value.ResultFrame)
	if err != nil {
		return fmt.Errorf("%s: некорректный идентификатор результирующего кадра: %w", methodCtx, err)
	}
	_, err = s.runner().ExecContext(ctx, `
		INSERT INTO action_results(
			action_id, message_id, correlation_id, agent_id, success, not_sent,
			retry_safe, result_frame, result_state, verification_confidence, error,
			completed_at, received_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, value.ActionID, value.MessageID, value.CorrelationID, value.AgentID,
		value.Success, value.NotSent, value.RetrySafe, resultFrame, value.ResultState,
		value.VerificationConfidence, value.Error,
		encodeTime(value.CompletedAt), encodeTime(value.ReceivedAt))
	if err != nil {
		if isForeignKeyConstraint(err) {
			return fmt.Errorf(
				"%s: исходное действие %q не найдено: %w",
				methodCtx,
				value.ActionID,
				notFound("действие", value.ActionID),
			)
		}
		return fmt.Errorf("%s: не удалось сохранить результат действия %q: %w", methodCtx, value.ActionID, err)
	}
	return nil
}

// ActionResult returns the most recently persisted result for an action.
func (s *SQLite) ActionResult(ctx context.Context, actionID string) (domain.ActionResultRecord, error) {
	const methodCtx = "repository.SQLite.ActionResult"

	if err := checkContext(ctx); err != nil {
		return domain.ActionResultRecord{}, fmt.Errorf("%s: не удалось проверить контекст: %w", methodCtx, err)
	}
	if err := requireID("действие", actionID); err != nil {
		return domain.ActionResultRecord{}, fmt.Errorf("%s: некорректный запрос результата действия: %w", methodCtx, err)
	}
	value, err := scanActionResult(s.runner().QueryRowContext(ctx, `
		SELECT action_id, message_id, correlation_id, agent_id, success, not_sent,
		       retry_safe, result_frame, result_state, verification_confidence, error,
		       completed_at, received_at
		FROM action_results
		WHERE action_id = ?
		ORDER BY sequence DESC
		LIMIT 1
	`, actionID))
	if errors.Is(err, sql.ErrNoRows) {
		return domain.ActionResultRecord{}, fmt.Errorf(
			"%s: результат действия %q не найден: %w",
			methodCtx,
			actionID,
			notFound("результат действия", actionID),
		)
	}
	if err != nil {
		return domain.ActionResultRecord{}, fmt.Errorf("%s: не удалось прочитать результат действия %q: %w", methodCtx, actionID, err)
	}
	return value, nil
}

// ListActionResults returns results in reverse insertion order.
func (s *SQLite) ListActionResults(ctx context.Context, actionID string, limit int) ([]domain.ActionResultRecord, error) {
	const methodCtx = "repository.SQLite.ListActionResults"

	if err := checkContext(ctx); err != nil {
		return nil, fmt.Errorf("%s: не удалось проверить контекст: %w", methodCtx, err)
	}
	if err := requireID("действие", actionID); err != nil {
		return nil, fmt.Errorf("%s: некорректный запрос результатов действия: %w", methodCtx, err)
	}
	query := `
		SELECT action_id, message_id, correlation_id, agent_id, success, not_sent,
		       retry_safe, result_frame, result_state, verification_confidence, error,
		       completed_at, received_at
		FROM action_results
		WHERE action_id = ?
		ORDER BY sequence DESC`
	args := []any{actionID}
	query, args, err := appendPagination(query, args, limit, 0)
	if err != nil {
		return nil, fmt.Errorf("%s: некорректные параметры пагинации: %w", methodCtx, err)
	}
	rows, err := s.runner().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("%s: не удалось получить результаты действия %q: %w", methodCtx, actionID, err)
	}
	defer rows.Close()
	result := make([]domain.ActionResultRecord, 0)
	for rows.Next() {
		value, err := scanActionResult(rows)
		if err != nil {
			return nil, fmt.Errorf("%s: не удалось прочитать строку результата действия: %w", methodCtx, err)
		}
		result = append(result, value)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%s: ошибка перебора результатов действий: %w", methodCtx, err)
	}
	if len(result) == 0 {
		return nil, fmt.Errorf(
			"%s: результаты действия %q не найдены: %w",
			methodCtx,
			actionID,
			notFound("результат действия", actionID),
		)
	}
	return result, nil
}

// SaveEvent stores an operational event idempotently by identifier.
func (s *SQLite) SaveEvent(ctx context.Context, value domain.AgentEventRecord) error {
	const methodCtx = "repository.SQLite.SaveEvent"

	if err := checkContext(ctx); err != nil {
		return fmt.Errorf("%s: не удалось проверить контекст: %w", methodCtx, err)
	}
	if err := requireID("событие", value.ID); err != nil {
		return fmt.Errorf("%s: некорректная запись события: %w", methodCtx, err)
	}
	result, err := s.runner().ExecContext(ctx, `
		INSERT INTO agent_events(
			id, session_id, agent_id, kind, severity, message, payload, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO NOTHING
	`, value.ID, value.SessionID, value.AgentID, value.Kind, value.Severity,
		value.Message, []byte(value.Payload), encodeTime(value.CreatedAt))
	if err != nil {
		return fmt.Errorf("%s: не удалось сохранить событие %q: %w", methodCtx, value.ID, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("%s: не удалось определить результат сохранения события %q: %w", methodCtx, value.ID, err)
	}
	if affected == 0 {
		existing, err := s.Event(ctx, value.ID)
		if err != nil {
			return fmt.Errorf("%s: не удалось проверить существующее событие %q: %w", methodCtx, value.ID, err)
		}
		if !eventsEqual(existing, value) {
			return fmt.Errorf(
				"%s: событие %q конфликтует с сохранённой записью: %w",
				methodCtx,
				value.ID,
				conflict("событие", value.ID),
			)
		}
	}
	return nil
}

// Event returns an operational event by identifier.
func (s *SQLite) Event(ctx context.Context, id string) (domain.AgentEventRecord, error) {
	const methodCtx = "repository.SQLite.Event"

	if err := checkContext(ctx); err != nil {
		return domain.AgentEventRecord{}, fmt.Errorf("%s: не удалось проверить контекст: %w", methodCtx, err)
	}
	if err := requireID("событие", id); err != nil {
		return domain.AgentEventRecord{}, fmt.Errorf("%s: некорректный запрос события: %w", methodCtx, err)
	}
	value, err := scanEvent(s.runner().QueryRowContext(ctx, `
		SELECT id, session_id, agent_id, kind, severity, message, payload, created_at
		FROM agent_events
		WHERE id = ?
	`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return domain.AgentEventRecord{}, fmt.Errorf(
			"%s: событие %q не найдено: %w",
			methodCtx,
			id,
			notFound("событие", id),
		)
	}
	if err != nil {
		return domain.AgentEventRecord{}, fmt.Errorf("%s: не удалось прочитать событие %q: %w", methodCtx, id, err)
	}
	return value, nil
}

// ListEvents returns operational events newest first.
func (s *SQLite) ListEvents(ctx context.Context, filter domain.EventFilter) ([]domain.AgentEventRecord, error) {
	const methodCtx = "repository.SQLite.ListEvents"

	if err := checkContext(ctx); err != nil {
		return nil, fmt.Errorf("%s: не удалось проверить контекст: %w", methodCtx, err)
	}
	query := `
		SELECT id, session_id, agent_id, kind, severity, message, payload, created_at
		FROM agent_events`
	conditions := make([]string, 0, 6)
	args := make([]any, 0, 8)
	if filter.SessionID != "" {
		conditions = append(conditions, "session_id = ?")
		args = append(args, filter.SessionID)
	}
	if filter.AgentID != "" {
		conditions = append(conditions, "agent_id = ?")
		args = append(args, filter.AgentID)
	}
	if filter.Kind != "" {
		conditions = append(conditions, "kind = ?")
		args = append(args, filter.Kind)
	}
	if filter.Severity != "" {
		conditions = append(conditions, "severity = ?")
		args = append(args, filter.Severity)
	}
	if !filter.Since.IsZero() {
		conditions = append(conditions, "created_at >= ?")
		args = append(args, encodeTime(filter.Since))
	}
	if !filter.Until.IsZero() {
		conditions = append(conditions, "created_at <= ?")
		args = append(args, encodeTime(filter.Until))
	}
	query = appendConditions(query, conditions)
	query += " ORDER BY created_at DESC, id DESC"
	query, args, err := appendPagination(query, args, filter.Limit, filter.Offset)
	if err != nil {
		return nil, fmt.Errorf("%s: некорректные параметры пагинации: %w", methodCtx, err)
	}
	rows, err := s.runner().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("%s: не удалось получить список событий: %w", methodCtx, err)
	}
	defer rows.Close()
	result := make([]domain.AgentEventRecord, 0)
	for rows.Next() {
		value, err := scanEvent(rows)
		if err != nil {
			return nil, fmt.Errorf("%s: не удалось прочитать строку события: %w", methodCtx, err)
		}
		result = append(result, value)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%s: ошибка перебора событий: %w", methodCtx, err)
	}
	return result, nil
}

// SaveRuntimeRecord atomically replaces one rich module snapshot.
func (s *SQLite) SaveRuntimeRecord(ctx context.Context, value domain.RuntimeRecord) error {
	const methodCtx = "repository.SQLite.SaveRuntimeRecord"

	if err := checkContext(ctx); err != nil {
		return fmt.Errorf("%s: не удалось проверить контекст: %w", methodCtx, err)
	}
	if err := requireID("запись состояния среды выполнения", value.Key); err != nil {
		return fmt.Errorf("%s: некорректная запись состояния среды выполнения: %w", methodCtx, err)
	}
	if len(value.Payload) == 0 {
		return fmt.Errorf("%s: содержимое записи состояния среды выполнения обязательно", methodCtx)
	}
	_, err := s.runner().ExecContext(ctx, `
		INSERT INTO runtime_records(key, kind, payload, updated_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET
			kind = excluded.kind,
			payload = excluded.payload,
			updated_at = excluded.updated_at
	`, value.Key, value.Kind, []byte(value.Payload), encodeTime(value.UpdatedAt))
	if err != nil {
		return fmt.Errorf("%s: не удалось сохранить запись состояния среды выполнения %q: %w", methodCtx, value.Key, err)
	}
	return nil
}

// RuntimeRecord returns one rich module snapshot.
func (s *SQLite) RuntimeRecord(ctx context.Context, key string) (domain.RuntimeRecord, error) {
	const methodCtx = "repository.SQLite.RuntimeRecord"

	if err := checkContext(ctx); err != nil {
		return domain.RuntimeRecord{}, fmt.Errorf("%s: не удалось проверить контекст: %w", methodCtx, err)
	}
	if err := requireID("запись состояния среды выполнения", key); err != nil {
		return domain.RuntimeRecord{}, fmt.Errorf("%s: некорректный запрос состояния среды выполнения: %w", methodCtx, err)
	}
	value, err := scanRuntimeRecord(s.runner().QueryRowContext(ctx, `
		SELECT key, kind, payload, updated_at
		FROM runtime_records
		WHERE key = ?
	`, key))
	if errors.Is(err, sql.ErrNoRows) {
		return domain.RuntimeRecord{}, fmt.Errorf(
			"%s: запись состояния среды выполнения %q не найдена: %w",
			methodCtx,
			key,
			notFound("запись состояния среды выполнения", key),
		)
	}
	if err != nil {
		return domain.RuntimeRecord{}, fmt.Errorf("%s: не удалось прочитать запись состояния среды выполнения %q: %w", methodCtx, key, err)
	}
	return value, nil
}

// ListRuntimeRecords returns matching snapshots newest first.
func (s *SQLite) ListRuntimeRecords(
	ctx context.Context,
	filter domain.RuntimeRecordFilter,
) ([]domain.RuntimeRecord, error) {
	const methodCtx = "repository.SQLite.ListRuntimeRecords"

	if err := checkContext(ctx); err != nil {
		return nil, fmt.Errorf("%s: не удалось проверить контекст: %w", methodCtx, err)
	}
	query := "SELECT key, kind, payload, updated_at FROM runtime_records"
	conditions := make([]string, 0, 2)
	args := make([]any, 0, 4)
	if filter.Kind != "" {
		conditions = append(conditions, "kind = ?")
		args = append(args, filter.Kind)
	}
	if filter.KeyPrefix != "" {
		conditions = append(conditions, "key LIKE ? ESCAPE '\\'")
		args = append(args, escapeLike(filter.KeyPrefix)+"%")
	}
	query = appendConditions(query, conditions)
	query += " ORDER BY updated_at DESC, key ASC"
	query, args, err := appendPagination(query, args, filter.Limit, filter.Offset)
	if err != nil {
		return nil, fmt.Errorf("%s: некорректные параметры пагинации: %w", methodCtx, err)
	}
	rows, err := s.runner().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("%s: не удалось получить список записей состояния среды выполнения: %w", methodCtx, err)
	}
	defer rows.Close()
	result := make([]domain.RuntimeRecord, 0)
	for rows.Next() {
		value, err := scanRuntimeRecord(rows)
		if err != nil {
			return nil, fmt.Errorf("%s: не удалось прочитать строку состояния среды выполнения: %w", methodCtx, err)
		}
		result = append(result, value)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%s: ошибка перебора записей состояния среды выполнения: %w", methodCtx, err)
	}
	return result, nil
}

type rowScanner interface {
	Scan(...any) error
}

func scanObservation(scanner rowScanner) (domain.Observation, error) {
	const methodCtx = "repository.scanObservation"

	var frameID int64
	var state string
	var elements, values []byte
	var confidence float64
	var createdAt string
	if err := scanner.Scan(&frameID, &state, &elements, &values, &confidence, &createdAt); err != nil {
		return domain.Observation{}, fmt.Errorf("%s: не удалось прочитать поля наблюдения: %w", methodCtx, err)
	}
	if frameID < 0 {
		return domain.Observation{}, fmt.Errorf("%s: идентификатор кадра наблюдения %d не может быть отрицательным", methodCtx, frameID)
	}
	created, err := decodeTime(createdAt)
	if err != nil {
		return domain.Observation{}, fmt.Errorf("%s: не удалось декодировать время наблюдения: %w", methodCtx, err)
	}
	value := domain.Observation{
		FrameID:    uint64(frameID),
		State:      domain.ScreenState(state),
		Confidence: confidence,
		CreatedAt:  created,
	}
	if err := json.Unmarshal(elements, &value.Elements); err != nil {
		return domain.Observation{}, fmt.Errorf("%s: не удалось декодировать элементы наблюдения: %w", methodCtx, err)
	}
	if err := json.Unmarshal(values, &value.Values); err != nil {
		return domain.Observation{}, fmt.Errorf("%s: не удалось декодировать значения наблюдения: %w", methodCtx, err)
	}
	return value, nil
}

func scanQuote(scanner rowScanner) (domain.MarketQuote, error) {
	const methodCtx = "repository.scanQuote"

	var value domain.MarketQuote
	var observedAt string
	if err := scanner.Scan(
		&value.ItemID, &value.BuyPrice, &value.SalePrice,
		&observedAt, &value.Confidence,
	); err != nil {
		return domain.MarketQuote{}, fmt.Errorf("%s: не удалось прочитать поля рыночной котировки: %w", methodCtx, err)
	}
	var err error
	value.ObservedAt, err = decodeTime(observedAt)
	if err != nil {
		return domain.MarketQuote{}, fmt.Errorf("%s: не удалось декодировать время рыночной котировки: %w", methodCtx, err)
	}
	return value, nil
}

func scanTradeQuote(scanner rowScanner) (domain.TradeQuote, error) {
	const methodCtx = "repository.scanTradeQuote"

	var value domain.TradeQuote
	var observedAt string
	if err := scanner.Scan(
		&value.ItemID,
		&value.PurchasePrice,
		&value.SalePrice,
		&value.SaleCommission,
		&value.ListingFee,
		&observedAt,
		&value.Confidence,
		&value.LiquidityScore,
		&value.PriceVolatility,
		&value.ResaleKnown,
	); err != nil {
		return domain.TradeQuote{}, fmt.Errorf("%s: не удалось прочитать поля торговой котировки: %w", methodCtx, err)
	}
	var err error
	value.ObservedAt, err = decodeTime(observedAt)
	if err != nil {
		return domain.TradeQuote{}, fmt.Errorf("%s: не удалось декодировать время торговой котировки: %w", methodCtx, err)
	}
	return value, nil
}

func scanExecution(scanner rowScanner) (domain.TradeExecution, error) {
	const methodCtx = "repository.scanExecution"

	var value domain.TradeExecution
	var status, startedAt, updatedAt string
	if err := scanner.Scan(
		&value.ID, &value.OpportunityID, &status, &value.CurrentStep,
		&value.Reserved, &startedAt, &updatedAt, &value.Failure,
	); err != nil {
		return domain.TradeExecution{}, fmt.Errorf("%s: не удалось прочитать поля исполнения сделки: %w", methodCtx, err)
	}
	value.Status = domain.TradeExecutionStatus(status)
	var err error
	value.StartedAt, err = decodeTime(startedAt)
	if err != nil {
		return domain.TradeExecution{}, fmt.Errorf("%s: не удалось декодировать время начала исполнения: %w", methodCtx, err)
	}
	value.UpdatedAt, err = decodeTime(updatedAt)
	if err != nil {
		return domain.TradeExecution{}, fmt.Errorf("%s: не удалось декодировать время обновления исполнения: %w", methodCtx, err)
	}
	return value, nil
}

func scanAction(scanner rowScanner) (domain.ActionRecord, error) {
	const methodCtx = "repository.scanAction"

	var value domain.ActionRecord
	var basedOnFrame int64
	var basedOnCapturedAt, basedOnFrameDigest, basedOnState string
	var expectedState, deadline, requestedAt string
	var pointX, pointY sql.NullFloat64
	var actionPayload, frameBasisPayload []byte
	if err := scanner.Scan(
		&value.ID, &value.SessionID, &value.AgentID, &basedOnFrame,
		&basedOnCapturedAt, &basedOnFrameDigest, &frameBasisPayload,
		&basedOnState, &expectedState,
		&value.MinConfidence, &value.ExpectedWidth, &value.ExpectedHeight,
		&value.ExpectedDPIPercent, &deadline, &value.Class, &value.Kind,
		&pointX, &pointY, &value.Value, &value.Delta, &actionPayload, &requestedAt,
	); err != nil {
		return domain.ActionRecord{}, fmt.Errorf("%s: не удалось прочитать поля действия: %w", methodCtx, err)
	}
	if basedOnFrame < 0 {
		return domain.ActionRecord{}, fmt.Errorf("%s: идентификатор исходного кадра %d не может быть отрицательным", methodCtx, basedOnFrame)
	}
	if pointX.Valid != pointY.Valid {
		return domain.ActionRecord{}, fmt.Errorf("%s: для точки действия задана только одна координата", methodCtx)
	}
	if pointX.Valid {
		value.Point = &domain.Point{X: pointX.Float64, Y: pointY.Float64}
	}
	value.BasedOnFrame = uint64(basedOnFrame)
	if basedOnCapturedAt != "" {
		capturedAt, err := decodeTime(basedOnCapturedAt)
		if err != nil {
			return domain.ActionRecord{}, fmt.Errorf(
				"%s: не удалось декодировать время исходного кадра: %w",
				methodCtx,
				err,
			)
		}
		value.BasedOnCapturedAt = &capturedAt
	}
	value.BasedOnFrameDigest = basedOnFrameDigest
	value.FrameBasisPayload = append(json.RawMessage(nil), frameBasisPayload...)
	value.BasedOnState = domain.ScreenState(basedOnState)
	value.ExpectedState = domain.ScreenState(expectedState)
	value.ActionPayload = append(json.RawMessage(nil), actionPayload...)
	var err error
	value.Deadline, err = decodeTime(deadline)
	if err != nil {
		return domain.ActionRecord{}, fmt.Errorf("%s: не удалось декодировать срок действия команды: %w", methodCtx, err)
	}
	value.RequestedAt, err = decodeTime(requestedAt)
	if err != nil {
		return domain.ActionRecord{}, fmt.Errorf("%s: не удалось декодировать время запроса действия: %w", methodCtx, err)
	}
	return value, nil
}

func scanActionResult(scanner rowScanner) (domain.ActionResultRecord, error) {
	const methodCtx = "repository.scanActionResult"

	var value domain.ActionResultRecord
	var success, notSent, retrySafe bool
	var resultFrame int64
	var resultState, completedAt, receivedAt string
	if err := scanner.Scan(
		&value.ActionID, &value.MessageID, &value.CorrelationID,
		&value.AgentID, &success, &notSent, &retrySafe, &resultFrame, &resultState,
		&value.VerificationConfidence, &value.Error, &completedAt, &receivedAt,
	); err != nil {
		return domain.ActionResultRecord{}, fmt.Errorf("%s: не удалось прочитать поля результата действия: %w", methodCtx, err)
	}
	if resultFrame < 0 {
		return domain.ActionResultRecord{}, fmt.Errorf("%s: идентификатор результирующего кадра %d не может быть отрицательным", methodCtx, resultFrame)
	}
	value.Success = success
	value.NotSent = notSent
	value.RetrySafe = retrySafe
	value.ResultFrame = uint64(resultFrame)
	value.ResultState = domain.ScreenState(resultState)
	var err error
	value.CompletedAt, err = decodeTime(completedAt)
	if err != nil {
		return domain.ActionResultRecord{}, fmt.Errorf("%s: не удалось декодировать время завершения действия: %w", methodCtx, err)
	}
	value.ReceivedAt, err = decodeTime(receivedAt)
	if err != nil {
		return domain.ActionResultRecord{}, fmt.Errorf("%s: не удалось декодировать время получения результата: %w", methodCtx, err)
	}
	return value, nil
}

func scanEvent(scanner rowScanner) (domain.AgentEventRecord, error) {
	const methodCtx = "repository.scanEvent"

	var value domain.AgentEventRecord
	var payload []byte
	var createdAt string
	if err := scanner.Scan(
		&value.ID, &value.SessionID, &value.AgentID, &value.Kind,
		&value.Severity, &value.Message, &payload, &createdAt,
	); err != nil {
		return domain.AgentEventRecord{}, fmt.Errorf("%s: не удалось прочитать поля события: %w", methodCtx, err)
	}
	value.Payload = append(json.RawMessage(nil), payload...)
	var err error
	value.CreatedAt, err = decodeTime(createdAt)
	if err != nil {
		return domain.AgentEventRecord{}, fmt.Errorf("%s: не удалось декодировать время события: %w", methodCtx, err)
	}
	return value, nil
}

func scanRuntimeRecord(scanner rowScanner) (domain.RuntimeRecord, error) {
	const methodCtx = "repository.scanRuntimeRecord"

	var value domain.RuntimeRecord
	var payload []byte
	var updatedAt string
	if err := scanner.Scan(&value.Key, &value.Kind, &payload, &updatedAt); err != nil {
		return domain.RuntimeRecord{}, fmt.Errorf("%s: не удалось прочитать поля состояния среды выполнения: %w", methodCtx, err)
	}
	value.Payload = append(json.RawMessage(nil), payload...)
	var err error
	value.UpdatedAt, err = decodeTime(updatedAt)
	if err != nil {
		return domain.RuntimeRecord{}, fmt.Errorf("%s: не удалось декодировать время состояния среды выполнения: %w", methodCtx, err)
	}
	return value, nil
}

func executionArgs(value domain.TradeExecution) []any {
	return []any{
		value.ID,
		value.OpportunityID,
		value.Status,
		value.CurrentStep,
		value.Reserved,
		encodeTime(value.StartedAt),
		encodeTime(value.UpdatedAt),
		value.Failure,
	}
}

func encodeTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(storageTimeLayout)
}

func decodeTime(value string) (time.Time, error) {
	const methodCtx = "repository.decodeTime"

	if value == "" {
		return time.Time{}, nil
	}
	parsed, err := time.Parse(storageTimeLayout, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("%s: некорректное значение времени %q: %w", methodCtx, value, err)
	}
	return parsed, nil
}

func databaseUint64(name string, value uint64) (int64, error) {
	const methodCtx = "repository.databaseUint64"

	if value > math.MaxInt64 {
		return 0, fmt.Errorf("%s: значение «%s» %d превышает диапазон INTEGER SQLite", methodCtx, name, value)
	}
	return int64(value), nil
}

func appendConditions(query string, conditions []string) string {
	if len(conditions) == 0 {
		return query
	}
	return query + " WHERE " + strings.Join(conditions, " AND ")
}

func escapeLike(value string) string {
	replacer := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return replacer.Replace(value)
}

func appendPagination(query string, args []any, limit, offset int) (string, []any, error) {
	const methodCtx = "repository.appendPagination"

	if limit < 0 {
		return "", nil, fmt.Errorf("%s: лимит не может быть отрицательным", methodCtx)
	}
	if offset < 0 {
		return "", nil, fmt.Errorf("%s: смещение не может быть отрицательным", methodCtx)
	}
	if limit > 0 {
		query += " LIMIT ?"
		args = append(args, limit)
	} else if offset > 0 {
		query += " LIMIT -1"
	}
	if offset > 0 {
		query += " OFFSET ?"
		args = append(args, offset)
	}
	return query, args, nil
}

func isForeignKeyConstraint(err error) bool {
	return strings.Contains(strings.ToLower(err.Error()), "foreign key constraint")
}

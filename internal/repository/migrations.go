package repository

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

func applyMigrations(ctx context.Context, db *sql.DB) error {
	const methodCtx = "repository.applyMigrations"

	if _, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version INTEGER PRIMARY KEY,
			name TEXT NOT NULL,
			applied_at TEXT NOT NULL
		)
	`); err != nil {
		return fmt.Errorf("%s: не удалось создать таблицу schema_migrations: %w", methodCtx, err)
	}

	names, err := fs.Glob(migrationFiles, "migrations/*.sql")
	if err != nil {
		return fmt.Errorf("%s: не удалось получить список встроенных миграций: %w", methodCtx, err)
	}
	sort.Strings(names)
	for _, name := range names {
		version, err := migrationVersion(name)
		if err != nil {
			return fmt.Errorf("%s: не удалось определить версию миграции %q: %w", methodCtx, name, err)
		}
		var exists int
		err = db.QueryRowContext(ctx,
			"SELECT 1 FROM schema_migrations WHERE version = ?",
			version,
		).Scan(&exists)
		if err == nil {
			continue
		}
		if err != sql.ErrNoRows {
			return fmt.Errorf("%s: не удалось проверить миграцию %q: %w", methodCtx, name, err)
		}

		body, err := migrationFiles.ReadFile(name)
		if err != nil {
			return fmt.Errorf("%s: не удалось прочитать миграцию %q: %w", methodCtx, name, err)
		}
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("%s: не удалось начать транзакцию миграции %q: %w", methodCtx, name, err)
		}
		if _, err = tx.ExecContext(ctx, string(body)); err != nil {
			applyErr := fmt.Errorf("%s: не удалось применить миграцию %q: %w", methodCtx, name, err)
			if rollbackErr := tx.Rollback(); rollbackErr != nil {
				return errors.Join(
					applyErr,
					fmt.Errorf("%s: не удалось откатить миграцию %q: %w", methodCtx, name, rollbackErr),
				)
			}
			return applyErr
		}
		if _, err = tx.ExecContext(ctx,
			"INSERT INTO schema_migrations(version, name, applied_at) VALUES (?, ?, ?)",
			version, path.Base(name), encodeTime(time.Now().UTC()),
		); err != nil {
			recordErr := fmt.Errorf("%s: не удалось записать миграцию %q: %w", methodCtx, name, err)
			if rollbackErr := tx.Rollback(); rollbackErr != nil {
				return errors.Join(
					recordErr,
					fmt.Errorf("%s: не удалось откатить миграцию %q: %w", methodCtx, name, rollbackErr),
				)
			}
			return recordErr
		}
		if err = tx.Commit(); err != nil {
			return fmt.Errorf("%s: не удалось зафиксировать миграцию %q: %w", methodCtx, name, err)
		}
	}
	return nil
}

func migrationVersion(name string) (int, error) {
	const methodCtx = "repository.migrationVersion"

	base := path.Base(name)
	prefix, _, ok := strings.Cut(base, "_")
	if !ok {
		return 0, fmt.Errorf("%s: некорректное имя файла миграции %q", methodCtx, name)
	}
	version, err := strconv.Atoi(prefix)
	if err != nil || version <= 0 {
		if err != nil {
			return 0, fmt.Errorf("%s: некорректная версия миграции в имени %q: %w", methodCtx, name, err)
		}
		return 0, fmt.Errorf("%s: версия миграции в имени %q должна быть положительной", methodCtx, name)
	}
	return version, nil
}

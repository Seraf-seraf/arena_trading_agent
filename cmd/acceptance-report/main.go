package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/arena-trading-agent/arena-trading-agent/internal/acceptance"
	"github.com/arena-trading-agent/arena-trading-agent/internal/repository"
)

type commandOptions struct {
	databasePath  string
	runs          int
	opportunityID string
	since         time.Time
}

func main() {
	ctx, stop := signal.NotifyContext(
		context.Background(),
		os.Interrupt,
		syscall.SIGTERM,
	)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

func run(
	ctx context.Context,
	args []string,
	stdout io.Writer,
	stderr io.Writer,
) int {
	const methodCtx = "cmd.acceptance-report.run"

	options, err := parseOptions(args)
	if err != nil {
		report := failureReport(commandOptions{runs: 50}, fmt.Sprintf(
			"%s: не удалось разобрать параметры: %v",
			methodCtx,
			err,
		))
		if outputErr := encodeReport(stdout, report); outputErr != nil {
			_, _ = fmt.Fprintf(
				stderr,
				"%s: не удалось вывести JSON с ошибкой параметров: %v\n",
				methodCtx,
				outputErr,
			)
		}
		return 2
	}
	store, err := repository.OpenSQLite(ctx, options.databasePath)
	if err != nil {
		report := failureReport(options, fmt.Sprintf(
			"%s: не удалось открыть SQLite: %v",
			methodCtx,
			err,
		))
		if outputErr := encodeReport(stdout, report); outputErr != nil {
			_, _ = fmt.Fprintf(
				stderr,
				"%s: не удалось вывести JSON ошибки SQLite: %v\n",
				methodCtx,
				outputErr,
			)
		}
		return 2
	}
	report, generateErr := acceptance.Generate(ctx, store, acceptance.Options{
		Runs:          options.runs,
		OpportunityID: options.opportunityID,
		Since:         options.since,
	})
	closeErr := store.Close()
	if generateErr != nil || closeErr != nil {
		reasons := make([]string, 0, 2)
		if generateErr != nil {
			reasons = append(reasons, fmt.Sprintf(
				"%s: не удалось сформировать отчёт: %v",
				methodCtx,
				generateErr,
			))
		}
		if closeErr != nil {
			reasons = append(reasons, fmt.Sprintf(
				"%s: не удалось закрыть SQLite: %v",
				methodCtx,
				closeErr,
			))
		}
		report = failureReport(options, strings.Join(reasons, "; "))
		if outputErr := encodeReport(stdout, report); outputErr != nil {
			_, _ = fmt.Fprintf(
				stderr,
				"%s: не удалось вывести JSON ошибки отчёта: %v\n",
				methodCtx,
				outputErr,
			)
		}
		return 2
	}
	if err := encodeReport(stdout, report); err != nil {
		_, _ = fmt.Fprintf(
			stderr,
			"%s: не удалось вывести JSON-отчёт: %v\n",
			methodCtx,
			err,
		)
		return 2
	}
	if !report.Accepted {
		return 1
	}
	return 0
}

func parseOptions(args []string) (commandOptions, error) {
	const methodCtx = "cmd.acceptance-report.parseOptions"

	flags := flag.NewFlagSet("acceptance-report", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	databasePath := flags.String("db", "data/arena.db", "путь к SQLite")
	runs := flags.Int("runs", 50, "число последних последовательных прогонов")
	opportunityID := flags.String(
		"opportunity",
		"",
		"ожидаемый идентификатор фиксированной возможности; чужие прогоны не фильтруются, а отклоняются",
	)
	sinceRaw := flags.String(
		"since",
		"",
		"необязательная нижняя граница времени в RFC3339",
	)
	if err := flags.Parse(args); err != nil {
		return commandOptions{}, fmt.Errorf("%s: некорректные флаги: %w", methodCtx, err)
	}
	if flags.NArg() != 0 {
		return commandOptions{}, fmt.Errorf(
			"%s: позиционные аргументы не поддерживаются",
			methodCtx,
		)
	}
	options := commandOptions{
		databasePath:  strings.TrimSpace(*databasePath),
		runs:          *runs,
		opportunityID: strings.TrimSpace(*opportunityID),
	}
	if options.databasePath == "" {
		return commandOptions{}, fmt.Errorf("%s: флаг -db не может быть пустым", methodCtx)
	}
	if strings.TrimSpace(*sinceRaw) != "" {
		since, err := time.Parse(time.RFC3339, strings.TrimSpace(*sinceRaw))
		if err != nil {
			return commandOptions{}, fmt.Errorf(
				"%s: значение -since должно соответствовать RFC3339: %w",
				methodCtx,
				err,
			)
		}
		options.since = since.UTC()
	}
	return options, nil
}

func failureReport(options commandOptions, reason string) acceptance.Report {
	report := acceptance.Report{
		SchemaVersion: acceptance.CurrentSchemaVersion,
		GeneratedAt:   time.Now().UTC(),
		Accepted:      false,
		RequiredRuns:  options.runs,
		OpportunityID: options.opportunityID,
		Reasons:       []string{reason},
		Runs:          make([]acceptance.Run, 0),
	}
	if !options.since.IsZero() {
		since := options.since.UTC()
		report.Since = &since
	}
	return report
}

func encodeReport(writer io.Writer, report acceptance.Report) error {
	const methodCtx = "cmd.acceptance-report.encodeReport"

	if writer == nil {
		return fmt.Errorf("%s: поток JSON-вывода не задан", methodCtx)
	}
	encoder := json.NewEncoder(writer)
	encoder.SetIndent("", "  ")
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(report); err != nil {
		return fmt.Errorf("%s: не удалось сериализовать JSON-отчёт: %w", methodCtx, err)
	}
	return nil
}

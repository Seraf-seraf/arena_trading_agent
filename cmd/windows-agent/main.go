package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/arena-trading-agent/arena-trading-agent/internal/agent"
)

const version = "0.1.0"

func main() {
	controllerURL := flag.String("controller", "ws://localhost:8787/ws/agent", "WebSocket URL контроллера")
	agentID := flag.String("agent-id", "windows-local", "уникальный идентификатор агента")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil)).With("процесс", "windows-agent")
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := agent.NewClient(*controllerURL, *agentID, version, logger).Run(ctx); err != nil {
		logger.Error("агент аварийно остановлен", "метод", "main", "ошибка", err)
		os.Exit(1)
	}
}

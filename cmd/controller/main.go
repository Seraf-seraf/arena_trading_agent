package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/arena-trading-agent/arena-trading-agent/internal/controller"
)

func main() {
	listen := flag.String("listen", ":8787", "адрес HTTP-сервера")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil)).With("процесс", "controller")
	server := &http.Server{
		Addr:              *listen,
		Handler:           controller.NewServer(logger).Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			logger.Error("не удалось штатно остановить контроллер", "метод", "main", "ошибка", err)
		}
	}()

	logger.Info("контроллер запущен", "метод", "main", "адрес", *listen)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("контроллер аварийно остановлен", "метод", "main", "ошибка", err)
		os.Exit(1)
	}
	logger.Info("контроллер остановлен", "метод", "main")
}

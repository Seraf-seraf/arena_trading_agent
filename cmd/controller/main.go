package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/arena-trading-agent/arena-trading-agent/internal/automation"
	appconfig "github.com/arena-trading-agent/arena-trading-agent/internal/config"
	"github.com/arena-trading-agent/arena-trading-agent/internal/controller"
	"github.com/arena-trading-agent/arena-trading-agent/internal/dashboard"
	"github.com/arena-trading-agent/arena-trading-agent/internal/detection"
	"github.com/arena-trading-agent/arena-trading-agent/internal/inventory"
	"github.com/arena-trading-agent/arena-trading-agent/internal/navigation"
	"github.com/arena-trading-agent/arena-trading-agent/internal/observation"
	"github.com/arena-trading-agent/arena-trading-agent/internal/opportunity"
	"github.com/arena-trading-agent/arena-trading-agent/internal/protocol"
	"github.com/arena-trading-agent/arena-trading-agent/internal/recording"
	"github.com/arena-trading-agent/arena-trading-agent/internal/repository"
	"github.com/arena-trading-agent/arena-trading-agent/internal/session"
	"github.com/arena-trading-agent/arena-trading-agent/internal/trading"
	"github.com/arena-trading-agent/arena-trading-agent/internal/vision"
)

func main() {
	const methodCtx = "cmd.controller.main"

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil)).With(
		"процесс", "controller",
		"метод", methodCtx,
	)
	flags := flag.NewFlagSet("controller", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	listen := flags.String("listen", "127.0.0.1:8787", "адрес HTTP-сервера")
	runtimeConfigPath := flags.String(
		"config",
		"configs/runtime.example.json",
		"путь к строгой runtime-конфигурации",
	)
	databasePath := flags.String("db", "data/arena.db", "путь к SQLite")
	recordingPath := flags.String("recordings", "data/frames", "каталог кадров и sidecar")
	lmStudioURL := flags.String("lm-studio", "http://127.0.0.1:1234", "URL LM Studio")
	lmModel := flags.String("lm-model", "qwen3.5-0.8b", "ключ модели компьютерного зрения в LM Studio")
	lmAPIKey := flags.String("lm-api-key", "", "необязательный токен API LM Studio")
	lmAutoLoad := flags.Bool("lm-auto-load", true, "автоматически загрузить компактную модель компьютерного зрения")
	lmContext := flags.Int("lm-context", 2048, "длина контекста модели компьютерного зрения")
	ocrURL := flags.String("ocr", "http://127.0.0.1:8788", "URL сервиса OCR")
	screenConfig := flags.String("screen-config", "", "путь к JSON откалиброванного детектора экранов")
	observeInterval := flags.Duration("observe-interval", 5*time.Second, "интервал режима OBSERVE")
	expectedWidth := flags.Int("expected-width", 0, "ожидаемая ширина клиентской области для TRADE")
	expectedHeight := flags.Int("expected-height", 0, "ожидаемая высота клиентской области для TRADE")
	expectedDPI := flags.Int("expected-dpi", 0, "ожидаемый масштаб DPI в процентах для TRADE")
	minConfidence := flags.Float64("min-confidence", .8, "минимальная уверенность предварительной проверки")
	if err := flags.Parse(os.Args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			_, _ = fmt.Fprintf(os.Stdout, "%s: параметры запуска:\n", methodCtx)
			flags.SetOutput(os.Stdout)
			flags.PrintDefaults()
			return
		}
		logger.Error("не удалось разобрать параметры запуска", "ошибка", err)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	runtimeConfig, err := appconfig.Load(*runtimeConfigPath)
	if err != nil {
		logger.Error("не удалось загрузить конфигурацию среды выполнения", "ошибка", err)
		os.Exit(1)
	}
	store, err := repository.OpenSQLite(ctx, *databasePath)
	if err != nil {
		logger.Error("не удалось открыть SQLite", "ошибка", err)
		os.Exit(1)
	}
	defer store.Close()
	recorder, err := recording.New(*recordingPath)
	if err != nil {
		logger.Error("не удалось открыть хранилище кадров", "ошибка", err)
		os.Exit(1)
	}
	vlm, err := vision.NewLMStudio(vision.LMStudioConfig{
		BaseURL: *lmStudioURL, Model: *lmModel, APIKey: *lmAPIKey,
		AutoLoad: *lmAutoLoad, ContextLength: *lmContext,
	})
	if err != nil {
		logger.Error("некорректная конфигурация LM Studio", "ошибка", err)
		os.Exit(1)
	}
	ocr, err := vision.NewOCRClient(*ocrURL, 20*time.Second)
	if err != nil {
		logger.Error("некорректная конфигурация OCR", "ошибка", err)
		os.Exit(1)
	}

	detectorPath := runtimeConfig.DetectorConfigPath
	if *screenConfig != "" {
		detectorPath = *screenConfig
	}
	var matcher *detection.Matcher
	if detectorPath != "" {
		matcher, err = detection.Load(detectorPath)
		if err != nil {
			logger.Error("не удалось загрузить детектор экранов", "ошибка", err)
			os.Exit(1)
		}
	}
	observer := observation.New(detection.LocalDetector{Matcher: matcher}, ocr, vlm)
	transport := controller.NewServer(
		logger,
		controller.WithAuditStore(store),
		controller.WithActionFrameSink(func(
			frameCtx context.Context,
			agentID string,
			actionID string,
			frame protocol.Frame,
		) error {
			const methodCtx = "cmd.controller.actionFrameSink"

			if err := frameCtx.Err(); err != nil {
				return fmt.Errorf("%s: контекст сохранения контрольного кадра завершён: %w", methodCtx, err)
			}
			if _, err := recorder.SaveActionFrame(agentID, actionID, frame); err != nil {
				return fmt.Errorf("%s: не удалось сохранить контрольный кадр действия: %w", methodCtx, err)
			}
			return nil
		}),
	)
	effectiveConfidence := *minConfidence
	if runtimeConfig.Risk.MinConfidence > effectiveConfidence {
		effectiveConfidence = runtimeConfig.Risk.MinConfidence
	}
	coordinator := session.New(
		transport, observer, store, recorder, vlm, ocr, logger,
		session.Config{
			ObserveInterval: *observeInterval, ExpectedWidth: *expectedWidth,
			ExpectedHeight: *expectedHeight, ExpectedDPIPercent: *expectedDPI,
			MinConfidence: effectiveConfidence,
		},
	)
	transitions, err := runtimeConfig.NavigationTransitions()
	if err != nil {
		logger.Error("не удалось скомпилировать граф навигации", "ошибка", err)
		os.Exit(1)
	}
	navigator, err := navigation.New(transitions)
	if err != nil {
		logger.Error("не удалось создать навигатор", "ошибка", err)
		os.Exit(1)
	}
	navigationExecutor, err := navigation.NewExecutor(
		transport,
		coordinator,
		navigation.Config{
			MinConfidence: effectiveConfidence,
		},
	)
	if err != nil {
		logger.Error("не удалось создать исполнитель навигации", "ошибка", err)
		os.Exit(1)
	}
	router, err := automation.NewRouter(transport, coordinator, navigator, navigationExecutor)
	if err != nil {
		logger.Error("не удалось создать маршрутизатор интерфейса", "ошибка", err)
		os.Exit(1)
	}
	marketScanner, err := automation.NewMarketScannerWithRecipes(
		router,
		store,
		runtimeConfig.Watchlist,
		runtimeConfig.Recipes,
		effectiveConfidence,
	)
	if err != nil {
		logger.Error("не удалось создать сканер рынка", "ошибка", err)
		os.Exit(1)
	}
	contactScanner, err := automation.NewContactScanner(
		router,
		store,
		runtimeConfig.Recipes,
		effectiveConfidence,
	)
	if err != nil {
		logger.Error("не удалось создать сканер контактов", "ошибка", err)
		os.Exit(1)
	}
	accountScanner, err := automation.NewAccountScanner(
		router,
		store,
		effectiveConfidence,
		runtimeConfig.TrackedItemIDs(),
	)
	if err != nil {
		logger.Error("не удалось создать сканер состояния аккаунта", "ошибка", err)
		os.Exit(1)
	}
	finder, err := opportunity.NewFinder(opportunity.Config{
		MaxMultistepDepth:   runtimeConfig.Strategy.MaxMultistepDepth,
		MaxRecipeExpansions: runtimeConfig.Strategy.MaxRecipeExpansions,
		QuoteTTL:            runtimeConfig.Risk.MaxQuoteAge.Value(),
	})
	if err != nil {
		logger.Error("не удалось создать поиск возможностей", "ошибка", err)
		os.Exit(1)
	}
	inventoryTracker, err := inventory.NewTracker(runtimeConfig.Risk.AvailableSlots)
	if err != nil {
		logger.Error("не удалось создать учёт инвентаря", "ошибка", err)
		os.Exit(1)
	}
	tradingService, err := trading.NewService(
		finder,
		inventoryTracker,
		trading.Config{
			ScoreWeights:    runtimeConfig.Strategy.ScoreWeights(),
			ProfitScale:     runtimeConfig.Strategy.ProfitScale,
			MaxStepAttempts: runtimeConfig.Strategy.MaxStepAttempts,
		},
	)
	if err != nil {
		logger.Error("не удалось создать торговую стратегию", "ошибка", err)
		os.Exit(1)
	}
	tradeRunner, err := automation.NewTradeRunner(
		router,
		runtimeConfig.Watchlist,
		runtimeConfig.Recipes,
		effectiveConfidence,
	)
	if err != nil {
		logger.Error("не удалось создать исполнитель сделок", "ошибка", err)
		os.Exit(1)
	}
	automationEngine, err := automation.NewEngine(
		transport,
		accountScanner,
		marketScanner,
		contactScanner,
		tradingService,
		tradeRunner,
		store,
		*runtimeConfig,
		logger,
		automation.EngineConfig{},
	)
	if err != nil {
		logger.Error("не удалось создать движок автоматизации", "ошибка", err)
		os.Exit(1)
	}
	if err := automationEngine.Recover(ctx); err != nil {
		logger.Error("не удалось восстановить движок автоматизации", "ошибка", err)
		os.Exit(1)
	}
	coordinator.SetModeReadiness(automationEngine.AuthorizeMode)
	transport.SetModeAuthorizer(coordinator.AuthorizeMode)
	go coordinator.Run(ctx)
	go automationEngine.Run(ctx)

	mux := http.NewServeMux()
	mux.Handle("/api/v1/runtime", coordinator.Handler())
	mux.Handle("/api/v1/runtime/", coordinator.Handler())
	mux.Handle("/api/v1/automation", automationEngine.Handler())
	mux.Handle("/api/v1/automation/", automationEngine.Handler())
	mux.Handle("/dashboard", dashboard.Handler())
	mux.Handle("/", transport.Handler())
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/" {
			dashboard.Handler().ServeHTTP(w, r)
			return
		}
		mux.ServeHTTP(w, r)
	})

	server := &http.Server{
		Addr:              *listen,
		Handler:           securityHeaders(handler),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	go func() {
		const methodCtx = "cmd.controller.shutdown"
		logger := logger.With("метод", methodCtx)

		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			logger.Error("не удалось штатно остановить контроллер", "ошибка", err)
		}
	}()

	logger.Info("контроллер запущен", "адрес", *listen, "панель_управления", "http://"+*listen+"/")
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("контроллер аварийно остановлен", "ошибка", err)
		os.Exit(1)
	}
	logger.Info("контроллер остановлен")
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'unsafe-inline'; script-src 'unsafe-inline'")
		next.ServeHTTP(w, r)
	})
}

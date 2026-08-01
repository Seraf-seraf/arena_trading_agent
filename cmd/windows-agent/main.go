package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"

	"github.com/arena-trading-agent/arena-trading-agent/internal/agent"
	"github.com/arena-trading-agent/arena-trading-agent/internal/detection"
	winplatform "github.com/arena-trading-agent/arena-trading-agent/internal/platform/windows"
)

const version = "0.2.0"

func main() {
	const methodCtx = "cmd.windows-agent.main"

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil)).With(
		"процесс", "windows-agent",
		"метод", methodCtx,
	)
	flags := flag.NewFlagSet("windows-agent", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	controllerURL := flags.String("controller", "ws://localhost:8787/ws/agent", "URL WebSocket контроллера")
	agentID := flags.String("agent-id", "windows-local", "уникальный идентификатор агента")
	processName := flags.String("process", "UAGame.exe", "имя процесса игры")
	windowTitle := flags.String("window-title", "Arena Breakout Infinite", "часть заголовка окна игры")
	screenConfig := flags.String("screen-config", "", "путь к JSON откалиброванного детектора экранов")
	allowInput := flags.Bool("allow-input", false, "разрешить SendInput после предварительной проверки контроллера")
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

	if err := validateSafetyFlags(*allowInput, *screenConfig); err != nil {
		logger.Error("небезопасная конфигурация Windows-агента", "ошибка", err)
		os.Exit(1)
	}
	if runtime.GOOS != "windows" {
		logger.Warn("агент запущен вне Windows: доступно только транспортное соединение")
		run(ctx, logger, agent.NewClient(*controllerURL, *agentID, version, logger))
		return
	}
	if err := winplatform.EnableDPIAwareness(); err != nil {
		logger.Error("не удалось включить режим масштабирования DPI", "ошибка", err)
		os.Exit(1)
	}

	var matcher *detection.Matcher
	if *screenConfig != "" {
		var err error
		matcher, err = detection.Load(*screenConfig)
		if err != nil {
			logger.Error("не удалось загрузить детектор экранов", "ошибка", err)
			os.Exit(1)
		}
	}

	window := winplatform.NewWindowManager(winplatform.WindowCriteria{
		ProcessName: *processName, TitleContains: *windowTitle,
	})
	capture := agent.NewTrackingCapture(winplatform.NewAdaptiveCapture(window))
	supervisor := agent.NewSafetySupervisor(3)

	var executor *agent.ActionExecutor
	var input *winplatform.SendInputDriver
	if *allowInput {
		input = winplatform.NewSendInputDriver(window)
		executor = agent.NewActionExecutor(
			input,
			capture,
			window,
			detection.AgentStateDetector{Matcher: matcher},
			capture.LatestFrame,
			supervisor.Paused,
		)
	} else {
		logger.Warn("SendInput выключен; агент работает в безопасном режиме наблюдения")
	}

	client := agent.NewRuntimeClient(agent.ClientOptions{
		ControllerURL:      *controllerURL,
		AgentID:            *agentID,
		Version:            version,
		Logger:             logger,
		Capture:            capture,
		Window:             window,
		Executor:           executor,
		IsStopped:          supervisor.Paused,
		IsEmergencyStopped: supervisor.Stopped,
		SafetyReason:       supervisor.Reason,
		EmergencyStop:      supervisor.Emergency,
		OnActionResult:     supervisor.RecordActionResult,
	})
	supervisor.AttachTransport(client)

	monitor := winplatform.NewSafetyMonitor(winplatform.SafetyOptions{})
	go func() {
		const methodCtx = "cmd.windows-agent.safetyMonitor"
		logger := logger.With("метод", methodCtx)

		err := monitor.Run(ctx, func(event winplatform.SafetyEvent) {
			switch event.Kind {
			case winplatform.SafetyEmergencyHotkey:
				supervisor.Emergency("нажат Ctrl+Alt+F12", true)
			}
		})
		if err != nil && !errors.Is(err, context.Canceled) {
			logger.Error("монитор безопасности остановлен", "ошибка", err)
			supervisor.Emergency("монитор безопасности недоступен", false)
		}
	}()

	run(ctx, logger, client)
}

func validateSafetyFlags(allowInput bool, screenConfig string) error {
	const methodCtx = "cmd.windows-agent.validateSafetyFlags"

	if !allowInput {
		return nil
	}
	if strings.TrimSpace(screenConfig) == "" {
		return fmt.Errorf(
			"%s: флаг -allow-input требует откалиброванный файл -screen-config",
			methodCtx,
		)
	}
	return nil
}

func run(ctx context.Context, logger *slog.Logger, client *agent.Client) {
	const methodCtx = "cmd.windows-agent.run"
	logger = logger.With("метод", methodCtx)

	if err := client.Run(ctx); err != nil {
		logger.Error("агент аварийно остановлен", "ошибка", err)
		os.Exit(1)
	}
}

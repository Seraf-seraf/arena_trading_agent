package controller_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/arena-trading-agent/arena-trading-agent/internal/controller"
	"github.com/arena-trading-agent/arena-trading-agent/internal/domain"
	"github.com/arena-trading-agent/arena-trading-agent/internal/protocol"
	"github.com/arena-trading-agent/arena-trading-agent/internal/repository"
)

func TestServerStartsPausedAndBlocksInput(t *testing.T) {
	t.Parallel()

	server := controller.NewServer(discardLogger())
	if got := server.Mode(); got != domain.ModePaused {
		t.Fatalf("начальный mode = %s, ожидался PAUSED", got)
	}

	_, err := server.RequestAction(context.Background(), "missing", protocol.ActionRequest{
		Action: protocol.Action{Kind: "CLICK", Value: "left"},
	})
	if !errors.Is(err, controller.ErrModeDisallowsInput) {
		t.Fatalf("PAUSED должен блокировать ввод, получено: %v", err)
	}

	if err := server.SetMode(domain.ModeObserve); err != nil {
		t.Fatal(err)
	}
	_, err = server.RequestAction(context.Background(), "missing", protocol.ActionRequest{
		Action: protocol.Action{Kind: "CLICK", Value: "left"},
	})
	if !errors.Is(err, controller.ErrModeDisallowsInput) {
		t.Fatalf("OBSERVE должен блокировать ввод, получено: %v", err)
	}
}

func TestControllerHTTPFallbackErrorsAreLocalized(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(controller.NewServer(discardLogger()).Handler())
	defer server.Close()

	response, err := http.Get(server.URL + "/нет-такого-маршрута")
	if err != nil {
		t.Fatal(err)
	}
	var notFound map[string]string
	decodeResponse(t, response, http.StatusNotFound, &notFound)
	if !strings.Contains(notFound["error"], "controller.Server.notFound") ||
		!strings.Contains(notFound["error"], "не найден") {
		t.Fatalf("ошибка 404 не локализована: %v", notFound)
	}

	request, err := http.NewRequest(http.MethodDelete, server.URL+"/healthz", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	var methodError map[string]string
	decodeResponse(t, response, http.StatusMethodNotAllowed, &methodError)
	if !strings.Contains(methodError["error"], "controller.Server.methodNotAllowed") ||
		!strings.Contains(methodError["error"], "не поддерживается") {
		t.Fatalf("ошибка 405 не локализована: %v", methodError)
	}
}

func TestInputModeRequiresSuccessfulFreshAuthorization(t *testing.T) {
	t.Parallel()

	server := controller.NewServer(discardLogger())
	called := 0
	server.SetModeAuthorizer(func(_ context.Context, mode domain.AgentMode) error {
		called++
		if mode != domain.ModeScan {
			t.Fatalf("неожиданный режим авторизации: %s", mode)
		}
		return errors.New("preflight failed")
	})
	if err := server.SetMode(domain.ModeObserve); err != nil {
		t.Fatal(err)
	}
	if called != 0 {
		t.Fatal("безопасный режим не должен запускать input preflight")
	}
	if err := server.SetMode(domain.ModeScan); !errors.Is(err, controller.ErrModeAuthorization) {
		t.Fatalf("ошибка авторизации = %v", err)
	}
	if called != 1 || server.Mode() != domain.ModePaused {
		t.Fatalf("input gate открылся после ошибки: called=%d mode=%s", called, server.Mode())
	}
}

func TestSafetyTransitionInvalidatesRunningModeAuthorization(t *testing.T) {
	t.Parallel()

	server := controller.NewServer(discardLogger())
	started := make(chan struct{})
	release := make(chan struct{})
	server.SetModeAuthorizer(func(_ context.Context, _ domain.AgentMode) error {
		close(started)
		<-release
		return nil
	})
	result := make(chan error, 1)
	go func() {
		result <- server.SetModeContext(context.Background(), domain.ModeTrade)
	}()
	<-started
	if err := server.SetMode(domain.ModePaused); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-result; !errors.Is(err, controller.ErrModeAuthorization) {
		t.Fatalf("устаревшая авторизация вернула %v", err)
	}
	if server.Mode() != domain.ModePaused {
		t.Fatalf("устаревший preflight открыл режим %s", server.Mode())
	}
}

func TestAgentPauseEventClosesControllerInputGate(t *testing.T) {
	t.Parallel()

	server := controller.NewServer(discardLogger())
	if err := server.SetMode(domain.ModeScan); err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()
	conn := connectAgent(t, httpServer.URL, "pause-agent", "v1")
	waitForAgents(t, httpServer.URL, 1)

	envelope, err := protocol.NewEnvelope(
		protocol.MessageAgentEvent,
		"pause-event",
		protocol.AgentEvent{
			Kind:     "AUTOMATION_PAUSED",
			Severity: "warning",
			Message:  "agent.SafetySupervisor.Pause: пользователь выполнил физический ввод",
			At:       time.Now().UTC(),
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := wsjson.Write(ctx, conn, envelope); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, time.Second, func() bool {
		return server.Mode() == domain.ModePaused
	})
}

func TestAgentPauseEventPreservesObserveAndBlocksInput(t *testing.T) {
	t.Parallel()

	server := controller.NewServer(discardLogger())
	if err := server.SetMode(domain.ModeObserve); err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()
	conn := connectAgent(t, httpServer.URL, "observe-pause-agent", "v1")
	waitForAgents(t, httpServer.URL, 1)

	sendAgentEvent(t, conn, "observe-pause-event", protocol.AgentEvent{
		Kind:     "AUTOMATION_PAUSED",
		Severity: "warning",
		Message:  "пользователь выполнил физический ввод",
		At:       time.Now().UTC(),
	})
	waitUntil(t, time.Second, func() bool {
		state, ok := server.AgentState("observe-pause-agent")
		return ok && state.AutomationPaused
	})
	if got := server.Mode(); got != domain.ModeObserve {
		t.Fatalf("AUTOMATION_PAUSED изменил OBSERVE на %s", got)
	}
	if _, err := server.RequestAction(
		context.Background(),
		"observe-pause-agent",
		protocol.ActionRequest{Action: protocol.Action{Kind: "CLICK", Value: "left"}},
	); !errors.Is(err, controller.ErrModeDisallowsInput) {
		t.Fatalf("OBSERVE открыл input gate после AUTOMATION_PAUSED: %v", err)
	}
}

func TestAgentPauseAndEmergencyEventsHandleOtherModes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		initialMode  domain.AgentMode
		eventKind    string
		expectedMode domain.AgentMode
	}{
		{
			name:         "automation pause in trade",
			initialMode:  domain.ModeTrade,
			eventKind:    "AUTOMATION_PAUSED",
			expectedMode: domain.ModePaused,
		},
		{
			name:         "automation pause in paused",
			initialMode:  domain.ModePaused,
			eventKind:    "AUTOMATION_PAUSED",
			expectedMode: domain.ModePaused,
		},
		{
			name:         "emergency stop in observe",
			initialMode:  domain.ModeObserve,
			eventKind:    "EMERGENCY_STOP_APPLIED",
			expectedMode: domain.ModePaused,
		},
	}
	for index, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			server := controller.NewServer(discardLogger())
			if err := server.SetMode(test.initialMode); err != nil {
				t.Fatal(err)
			}
			httpServer := httptest.NewServer(server.Handler())
			defer httpServer.Close()
			agentID := fmt.Sprintf("safety-mode-agent-%d", index)
			conn := connectAgent(t, httpServer.URL, agentID, "v1")
			waitForAgents(t, httpServer.URL, 1)

			sendAgentEvent(t, conn, fmt.Sprintf("safety-mode-event-%d", index), protocol.AgentEvent{
				Kind:     test.eventKind,
				Severity: "warning",
				Message:  "проверка события безопасности",
				At:       time.Now().UTC(),
			})
			waitUntil(t, time.Second, func() bool {
				state, ok := server.AgentState(agentID)
				return ok && state.AutomationPaused && server.Mode() == test.expectedMode
			})
			if got := server.Mode(); got != test.expectedMode {
				t.Fatalf("%s изменил %s на %s, ожидался %s", test.eventKind, test.initialMode, got, test.expectedMode)
			}
		})
	}
}

func TestDuplexRequestsAreCorrelatedAndUpdateSnapshot(t *testing.T) {
	t.Parallel()

	server := controller.NewServer(discardLogger())
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()
	conn := connectAgent(t, httpServer.URL, "duplex-agent", "v1")
	waitForAgents(t, httpServer.URL, 1)

	frameResult := make(chan struct {
		frame protocol.Frame
		err   error
	}, 1)
	go func() {
		frame, err := server.RequestFrame(context.Background(), "duplex-agent", 7)
		frameResult <- struct {
			frame protocol.Frame
			err   error
		}{frame: frame, err: err}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var request protocol.Envelope
	if err := wsjson.Read(ctx, conn, &request); err != nil {
		t.Fatalf("не удалось прочитать FRAME_REQUEST: %v", err)
	}
	if request.Type != protocol.MessageFrameRequest || request.MessageID == "" {
		t.Fatalf("неожиданный request envelope: %+v", request)
	}
	var frameRequest protocol.FrameRequest
	if err := json.Unmarshal(request.Payload, &frameRequest); err != nil {
		t.Fatal(err)
	}
	if frameRequest.AfterFrame != 7 {
		t.Fatalf("AfterFrame = %d, ожидалось 7", frameRequest.AfterFrame)
	}

	wantFrame := protocol.Frame{
		ID:         8,
		CapturedAt: time.Now().UTC(),
		Encoding:   "png",
		Data:       []byte("test-image"),
		Region:     domain.Rectangle{Width: 1, Height: 1},
	}
	response, err := protocol.NewCorrelatedEnvelope(protocol.MessageFrame, "frame-8", request.MessageID, wantFrame)
	if err != nil {
		t.Fatal(err)
	}
	if err := wsjson.Write(ctx, conn, response); err != nil {
		t.Fatalf("не удалось отправить FRAME: %v", err)
	}

	select {
	case result := <-frameResult:
		if result.err != nil {
			t.Fatalf("RequestFrame вернул ошибку: %v", result.err)
		}
		if result.frame.ID != wantFrame.ID ||
			!bytes.Equal(result.frame.Data, wantFrame.Data) ||
			result.frame.ContentDigest != protocol.ComputeFrameDigest(wantFrame.Data) {
			t.Fatalf("неожиданный frame: %+v", result.frame)
		}
	case <-ctx.Done():
		t.Fatal("RequestFrame не получил коррелированный ответ")
	}

	state, ok := server.AgentState("duplex-agent")
	if !ok || state.LatestFrame == nil ||
		state.LatestFrame.ID != 8 ||
		state.LatestFrame.Size != len(wantFrame.Data) ||
		state.LatestFrame.ContentDigest != protocol.ComputeFrameDigest(wantFrame.Data) {
		t.Fatalf("frame не попал в snapshot: %+v", state)
	}

	statusResult := make(chan error, 1)
	go func() {
		status, err := server.RequestWindowStatus(context.Background(), "duplex-agent")
		if err == nil && (status.Width != 1920 || !status.Active) {
			err = errors.New("получен неверный WINDOW_STATUS")
		}
		statusResult <- err
	}()
	request = protocol.Envelope{}
	if err := wsjson.Read(ctx, conn, &request); err != nil {
		t.Fatalf("не удалось прочитать WINDOW_STATUS_REQUEST: %v", err)
	}
	if request.Type != protocol.MessageWindowStatusRequest {
		t.Fatalf("тип = %s, ожидался WINDOW_STATUS_REQUEST", request.Type)
	}
	status := protocol.WindowStatus{Active: true, Width: 1920, Height: 1080, DPIPercent: 100}
	response, err = protocol.NewCorrelatedEnvelope(protocol.MessageWindowStatus, "window-1", request.MessageID, status)
	if err != nil {
		t.Fatal(err)
	}
	if err := wsjson.Write(ctx, conn, response); err != nil {
		t.Fatal(err)
	}
	if err := <-statusResult; err != nil {
		t.Fatal(err)
	}
}

func TestCorrelatedFrameResponseRequiresExactMessageType(t *testing.T) {
	t.Parallel()

	server := controller.NewServer(discardLogger())
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()
	conn := connectAgent(t, httpServer.URL, "strict-type-agent", "v1")
	waitForAgents(t, httpServer.URL, 1)

	result := make(chan error, 1)
	go func() {
		_, err := server.RequestFrame(context.Background(), "strict-type-agent", 40)
		result <- err
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var request protocol.Envelope
	if err := wsjson.Read(ctx, conn, &request); err != nil {
		t.Fatal(err)
	}
	if request.Type != protocol.MessageFrameRequest {
		t.Fatalf("получен другой запрос: %+v", request)
	}
	wrongType, err := protocol.NewCorrelatedEnvelope(
		protocol.MessageFrameRegion,
		"wrong-frame-type",
		request.MessageID,
		protocol.Frame{
			ID:         41,
			CapturedAt: time.Now().UTC(),
			Encoding:   "png",
			Data:       []byte("region"),
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := wsjson.Write(ctx, conn, wrongType); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		if !errors.Is(err, controller.ErrUnexpectedResponse) {
			t.Fatalf("ошибка типа ответа = %v, ожидалась ErrUnexpectedResponse", err)
		}
	case <-ctx.Done():
		t.Fatal("несовпадение типа ответа не доставлено запросу")
	}
	if _, ok := server.LatestFrame("strict-type-agent"); ok {
		t.Fatal("FRAME_REGION от другого запроса попал в latest frame")
	}
}

func TestLatestFrameNeverRegressesByIDOrCaptureTime(t *testing.T) {
	t.Parallel()

	server := controller.NewServer(discardLogger())
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()
	conn := connectAgent(t, httpServer.URL, "monotonic-frame-agent", "v1")
	waitForAgents(t, httpServer.URL, 1)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	baseTime := time.Now().UTC()
	sendFrame := func(frame protocol.Frame) {
		t.Helper()
		envelope, err := protocol.NewEnvelope(protocol.MessageFrame, "passive-frame", frame)
		if err != nil {
			t.Fatal(err)
		}
		if err := wsjson.Write(ctx, conn, envelope); err != nil {
			t.Fatal(err)
		}
	}

	sendFrame(protocol.Frame{
		ID: 100, CapturedAt: baseTime, Encoding: "png", Data: []byte("current"),
	})
	waitForFrame(t, server, "monotonic-frame-agent", 100)
	sendFrame(protocol.Frame{
		ID: 99, CapturedAt: baseTime.Add(time.Second), Encoding: "png", Data: []byte("old-id"),
	})
	sendFrame(protocol.Frame{
		ID: 101, CapturedAt: baseTime.Add(-time.Second), Encoding: "png", Data: []byte("old-time"),
	})
	time.Sleep(20 * time.Millisecond)
	frame, ok := server.LatestFrame("monotonic-frame-agent")
	if !ok || frame.ID != 100 || string(frame.Data) != "current" {
		t.Fatalf("latest frame регрессировал: %+v", frame)
	}

	sendFrame(protocol.Frame{
		ID: 101, CapturedAt: baseTime.Add(time.Second), Encoding: "png", Data: []byte("new"),
	})
	waitForFrame(t, server, "monotonic-frame-agent", 101)
	frame, _ = server.LatestFrame("monotonic-frame-agent")
	if string(frame.Data) != "new" {
		t.Fatalf("новый монотонный кадр не сохранён: %+v", frame)
	}
}

func TestActionRequestIsBoundToStoredFrameMetadata(t *testing.T) {
	t.Parallel()

	store := repository.NewMemory()
	server := controller.NewServer(
		discardLogger(),
		controller.WithAuditStore(store),
	)
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()
	conn := connectAgent(t, httpServer.URL, "bound-action-agent", "v1")
	waitForAgents(t, httpServer.URL, 1)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	capturedAt := time.Now().UTC()
	data := controllerBasisPNG(t)
	frameEnvelope, err := protocol.NewEnvelope(protocol.MessageFrame, "source-frame", protocol.Frame{
		ID: 77, CapturedAt: capturedAt,
		Region:   domain.Rectangle{Width: 1, Height: 1},
		Encoding: "png", Data: data,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := wsjson.Write(ctx, conn, frameEnvelope); err != nil {
		t.Fatal(err)
	}
	waitForFrame(t, server, "bound-action-agent", 77)
	if err := server.SetMode(domain.ModeScan); err != nil {
		t.Fatal(err)
	}
	frameBasis, err := protocol.BuildFrameRegionBasis(
		protocol.Frame{
			ID: 77, CapturedAt: capturedAt,
			Region:   domain.Rectangle{Width: 1, Height: 1},
			Encoding: "png", Data: data,
		},
		[]domain.Rectangle{{Width: .5, Height: 1}},
	)
	if err != nil {
		t.Fatalf("BuildFrameRegionBasis() error = %v", err)
	}

	result := make(chan error, 1)
	go func() {
		_, requestErr := server.RequestAction(context.Background(), "bound-action-agent", protocol.ActionRequest{
			ID:                        "bound-action",
			SessionID:                 "session",
			BasedOnFrame:              77,
			FrameBasis:                frameBasis,
			BasedOnState:              domain.StateMainMenu,
			ExpectedState:             domain.StateMarketHome,
			MinVerificationConfidence: .93,
			ExpectedWidth:             1280,
			ExpectedHeight:            1024,
			ExpectedDPIPercent:        100,
			Deadline:                  time.Now().Add(time.Second),
			Action:                    protocol.Action{Kind: "CLICK", Value: "LEFT"},
		})
		result <- requestErr
	}()

	var requestEnvelope protocol.Envelope
	if err := wsjson.Read(ctx, conn, &requestEnvelope); err != nil {
		t.Fatal(err)
	}
	var request protocol.ActionRequest
	if err := json.Unmarshal(requestEnvelope.Payload, &request); err != nil {
		t.Fatal(err)
	}
	if request.BasedOnCapturedAt == nil ||
		!request.BasedOnCapturedAt.Equal(capturedAt) ||
		request.BasedOnFrameDigest != protocol.ComputeFrameDigest(data) ||
		len(request.FrameBasis) != 1 ||
		request.BasedOnState != domain.StateMainMenu ||
		request.MinVerificationConfidence != .93 {
		t.Fatalf("ACTION_REQUEST не привязан к исходному кадру: %+v", request)
	}
	storedAction, err := store.Action(ctx, request.ID)
	if err != nil {
		t.Fatalf("аудит привязанного ACTION_REQUEST не найден: %v", err)
	}
	if storedAction.BasedOnCapturedAt == nil ||
		!storedAction.BasedOnCapturedAt.Equal(capturedAt) ||
		storedAction.BasedOnFrameDigest != protocol.ComputeFrameDigest(data) ||
		storedAction.BasedOnState != domain.StateMainMenu ||
		storedAction.MinConfidence != .93 {
		t.Fatalf("аудит потерял основание ACTION_REQUEST: %+v", storedAction)
	}
	var storedBasis []protocol.FrameRegionDigest
	if err := json.Unmarshal(storedAction.FrameBasisPayload, &storedBasis); err != nil {
		t.Fatalf("аудит ROI-основания не декодируется: %v", err)
	}
	if len(storedBasis) != 1 ||
		storedBasis[0].Region != frameBasis[0].Region ||
		storedBasis[0].Digest != frameBasis[0].Digest {
		t.Fatalf("аудит потерял ROI-основание: %+v", storedBasis)
	}
	response, err := protocol.NewCorrelatedEnvelope(
		protocol.MessageActionResult,
		"bound-result",
		requestEnvelope.MessageID,
		protocol.ActionResult{
			ID:          request.ID,
			Success:     true,
			ResultFrame: 78,
			ResultState: domain.StateMarketHome,
			CompletedAt: time.Now().UTC(),
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := wsjson.Write(ctx, conn, response); err != nil {
		t.Fatal(err)
	}
	if err := <-result; err != nil {
		t.Fatalf("RequestAction() error = %v", err)
	}
}

func TestControllerRejectsMismatchedOrMalformedROIBasisBeforeWire(t *testing.T) {
	t.Parallel()

	server := controller.NewServer(discardLogger())
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()
	conn := connectAgent(t, httpServer.URL, "roi-validation-agent", "v1")
	defer conn.Close(websocket.StatusNormalClosure, "тест завершён")
	waitForAgents(t, httpServer.URL, 1)
	capturedAt := time.Now().UTC()
	data := controllerBasisPNG(t)
	frame := protocol.Frame{
		ID: 91, CapturedAt: capturedAt,
		Region:   domain.Rectangle{Width: 1, Height: 1},
		Encoding: "png", Data: data,
	}
	envelope, err := protocol.NewEnvelope(protocol.MessageFrame, "roi-source", frame)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := wsjson.Write(ctx, conn, envelope); err != nil {
		t.Fatal(err)
	}
	waitForFrame(t, server, "roi-validation-agent", frame.ID)
	if err := server.SetMode(domain.ModeScan); err != nil {
		t.Fatal(err)
	}
	validBasis, err := protocol.BuildFrameRegionBasis(
		frame,
		[]domain.Rectangle{{Width: .5, Height: 1}},
	)
	if err != nil {
		t.Fatal(err)
	}
	common := protocol.ActionRequest{
		SessionID:          "session",
		BasedOnFrame:       frame.ID,
		BasedOnState:       domain.StateMainMenu,
		ExpectedState:      domain.StateMarketHome,
		ExpectedWidth:      1280,
		ExpectedHeight:     1024,
		ExpectedDPIPercent: 100,
		Class:              protocol.ActionNavigation,
		Deadline:           time.Now().Add(time.Second),
		Action:             protocol.Action{Kind: "CLICK", Value: "LEFT"},
	}

	mismatched := common
	mismatched.ID = "roi-mismatch"
	mismatched.FrameBasis = append([]protocol.FrameRegionDigest(nil), validBasis...)
	mismatched.FrameBasis[0].Digest = protocol.ComputeFrameDigest([]byte("другие пиксели"))
	if _, err := server.RequestAction(
		context.Background(),
		"roi-validation-agent",
		mismatched,
	); err == nil || !strings.Contains(err.Error(), "не соответствует сохранённому кадру") {
		t.Fatalf("чужое ROI-основание не отклонено: %v", err)
	}

	malformed := common
	malformed.ID = "roi-malformed"
	malformed.FrameBasis = make(
		[]protocol.FrameRegionDigest,
		protocol.MaxFrameBasisRegions+1,
	)
	if _, err := server.RequestAction(
		context.Background(),
		"roi-validation-agent",
		malformed,
	); err == nil || !strings.Contains(err.Error(), "число областей") {
		t.Fatalf("чрезмерное ROI-основание не отклонено: %v", err)
	}
}

func TestControllerRejectsMonetaryActionWithoutROIBasis(t *testing.T) {
	t.Parallel()

	server := controller.NewServer(discardLogger())
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()
	conn := connectAgent(t, httpServer.URL, "money-roi-agent", "v1")
	defer conn.Close(websocket.StatusNormalClosure, "тест завершён")
	waitForAgents(t, httpServer.URL, 1)
	if err := server.SetMode(domain.ModeTrade); err != nil {
		t.Fatal(err)
	}
	_, err := server.RequestAction(context.Background(), "money-roi-agent", protocol.ActionRequest{
		ID:                 "money-without-roi",
		SessionID:          "session",
		BasedOnFrame:       1,
		BasedOnState:       domain.StatePurchaseDialog,
		ExpectedState:      domain.StateConfirmation,
		ExpectedWidth:      1280,
		ExpectedHeight:     1024,
		ExpectedDPIPercent: 100,
		Class:              protocol.ActionPurchase,
		Deadline:           time.Now().Add(time.Second),
		Action: protocol.Action{
			Kind: "CLICK", Point: &domain.Point{X: .5, Y: .5}, Value: "LEFT",
		},
	})
	if err == nil || !strings.Contains(err.Error(), "требует непустое frame_basis") {
		t.Fatalf("денежное действие без ROI не отклонено: %v", err)
	}
}

func TestDuplicateAgentIDAtomicallyReplacesOldConnection(t *testing.T) {
	t.Parallel()

	server := controller.NewServer(discardLogger())
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()

	oldConn := connectAgent(t, httpServer.URL, "same-agent", "old")
	waitForAgentVersion(t, server, "same-agent", "old")
	newConn := connectAgent(t, httpServer.URL, "same-agent", "new")
	waitForAgentVersion(t, server, "same-agent", "new")

	// Завершение handler старого websocket не должно удалить новый map entry.
	_ = oldConn.Close(websocket.StatusNormalClosure, "old done")
	waitForAgentVersion(t, server, "same-agent", "new")
	waitForAgents(t, httpServer.URL, 1)

	result := make(chan error, 1)
	go func() {
		_, err := server.RequestWindowStatus(context.Background(), "same-agent")
		result <- err
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var request protocol.Envelope
	if err := wsjson.Read(ctx, newConn, &request); err != nil {
		t.Fatalf("новое подключение не получило команду: %v", err)
	}
	response, err := protocol.NewCorrelatedEnvelope(
		protocol.MessageWindowStatus,
		"window-new",
		request.MessageID,
		protocol.WindowStatus{Active: true},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := wsjson.Write(ctx, newConn, response); err != nil {
		t.Fatal(err)
	}
	if err := <-result; err != nil {
		t.Fatalf("команда новому подключению завершилась ошибкой: %v", err)
	}
}

func TestActionResultFallsBackToPayloadIDAndEmergencyPauses(t *testing.T) {
	t.Parallel()

	server := controller.NewServer(discardLogger())
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()
	conn := connectAgent(t, httpServer.URL, "action-agent", "v1")
	waitForAgents(t, httpServer.URL, 1)
	if err := server.SetMode(domain.ModeScan); err != nil {
		t.Fatal(err)
	}

	actionResult := make(chan struct {
		value protocol.ActionResult
		err   error
	}, 1)
	go func() {
		value, err := server.RequestAction(context.Background(), "action-agent", protocol.ActionRequest{
			ID:                 "action-1",
			SessionID:          "session-1",
			BasedOnFrame:       12,
			ExpectedState:      domain.StateMainMenu,
			ExpectedWidth:      1280,
			ExpectedHeight:     1024,
			ExpectedDPIPercent: 100,
			Deadline:           time.Now().Add(time.Second),
			Action:             protocol.Action{Kind: "CLICK", Value: "PRIMARY"},
		})
		actionResult <- struct {
			value protocol.ActionResult
			err   error
		}{value: value, err: err}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var request protocol.Envelope
	if err := wsjson.Read(ctx, conn, &request); err != nil {
		t.Fatalf("ACTION_REQUEST не доставлен: %v", err)
	}
	if request.Type != protocol.MessageActionRequest || request.MessageID != "action-1" {
		t.Fatalf("неожиданный ACTION_REQUEST: %+v", request)
	}
	// Старый агент может не знать correlation_id. Для ACTION_RESULT payload.ID
	// остаётся обратносовместимым ключом корреляции.
	response, err := protocol.NewEnvelope(protocol.MessageActionResult, "agent-result-1", protocol.ActionResult{
		ID:          "action-1",
		Success:     true,
		ResultFrame: 13,
		ResultState: domain.StateMainMenu,
		CompletedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := wsjson.Write(ctx, conn, response); err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-actionResult:
		if result.err != nil || !result.value.Success || result.value.ResultFrame != 13 {
			t.Fatalf("неожиданный action result: %+v, err=%v", result.value, result.err)
		}
	case <-ctx.Done():
		t.Fatal("ACTION_RESULT не был коррелирован по payload.id")
	}

	stop, err := protocol.NewEnvelope(protocol.MessageEmergencyStop, "stop-1", protocol.EmergencyStop{
		Reason: "test hotkey",
		At:     time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := wsjson.Write(ctx, conn, stop); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && server.Mode() != domain.ModePaused {
		time.Sleep(5 * time.Millisecond)
	}
	if server.Mode() != domain.ModePaused {
		t.Fatalf("EMERGENCY_STOP не перевёл controller в PAUSED: %s", server.Mode())
	}
	state, ok := server.AgentState("action-agent")
	if !ok || !state.EmergencyStopped || !state.AutomationPaused {
		t.Fatalf("emergency stop не отражён в agent state: %+v", state)
	}
}

func TestScanModeRejectsSemanticMoneyActionBeforeAuditAndWire(t *testing.T) {
	t.Parallel()

	store := repository.NewMemory()
	server := controller.NewServer(discardLogger(), controller.WithAuditStore(store))
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()
	connectAgent(t, httpServer.URL, "scan-agent", "v1")
	waitForAgents(t, httpServer.URL, 1)
	if err := server.SetMode(domain.ModeScan); err != nil {
		t.Fatal(err)
	}
	_, err := server.RequestAction(context.Background(), "scan-agent", protocol.ActionRequest{
		ID:                 "forbidden-purchase",
		SessionID:          "session-1",
		BasedOnFrame:       1,
		ExpectedState:      domain.StateConfirmation,
		ExpectedWidth:      1280,
		ExpectedHeight:     1024,
		ExpectedDPIPercent: 100,
		Class:              protocol.ActionPurchase,
		Deadline:           time.Now().Add(time.Second),
		Action:             protocol.Action{Kind: "CLICK"},
	})
	if !errors.Is(err, controller.ErrModeDisallowsMoney) {
		t.Fatalf("purchase in SCAN error=%v", err)
	}
	actions, listErr := store.ListActions(context.Background(), domain.ActionFilter{})
	if listErr != nil {
		t.Fatal(listErr)
	}
	if len(actions) != 0 {
		t.Fatalf("forbidden action was audited as sent: %+v", actions)
	}
}

func TestProtocolAuditPersistsActionResultAndEvent(t *testing.T) {
	t.Parallel()

	store := repository.NewMemory()
	server := controller.NewServer(discardLogger(), controller.WithAuditStore(store))
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()
	conn := connectAgent(t, httpServer.URL, "audit-agent", "v1")
	waitForAgents(t, httpServer.URL, 1)
	if err := server.SetMode(domain.ModeScan); err != nil {
		t.Fatal(err)
	}

	completedAt := time.Now().UTC()
	resultCh := make(chan error, 1)
	go func() {
		_, err := server.RequestAction(context.Background(), "audit-agent", protocol.ActionRequest{
			ID:                 "audit-action-1",
			SessionID:          "session-1",
			BasedOnFrame:       52,
			ExpectedState:      domain.StateMarketResults,
			ExpectedWidth:      1920,
			ExpectedHeight:     1080,
			ExpectedDPIPercent: 125,
			Deadline:           time.Now().Add(time.Second),
			Action:             protocol.Action{Kind: "SCROLL", Delta: -120},
		})
		resultCh <- err
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var request protocol.Envelope
	if err := wsjson.Read(ctx, conn, &request); err != nil {
		t.Fatalf("ACTION_REQUEST не доставлен: %v", err)
	}
	storedAction, err := store.Action(ctx, "audit-action-1")
	if err != nil {
		t.Fatalf("ACTION_REQUEST не записан до отправки: %v", err)
	}
	if storedAction.AgentID != "audit-agent" ||
		storedAction.ExpectedWidth != 1920 ||
		storedAction.ExpectedHeight != 1080 ||
		storedAction.ExpectedDPIPercent != 125 ||
		storedAction.Delta != -120 {
		t.Fatalf("ACTION_REQUEST записан неполно: %+v", storedAction)
	}

	response, err := protocol.NewCorrelatedEnvelope(
		protocol.MessageActionResult,
		"audit-result-message-1",
		request.MessageID,
		protocol.ActionResult{
			ID:          "audit-action-1",
			Success:     true,
			ResultFrame: 53,
			ResultState: domain.StateMarketResults,
			CompletedAt: completedAt,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := wsjson.Write(ctx, conn, response); err != nil {
		t.Fatal(err)
	}
	if err := <-resultCh; err != nil {
		t.Fatalf("RequestAction() error = %v", err)
	}

	var storedResult domain.ActionResultRecord
	waitUntil(t, time.Second, func() bool {
		var loadErr error
		storedResult, loadErr = store.ActionResult(context.Background(), "audit-action-1")
		return loadErr == nil
	})
	if storedResult.MessageID != "audit-result-message-1" ||
		storedResult.CorrelationID != "audit-action-1" ||
		storedResult.AgentID != "audit-agent" ||
		storedResult.ReceivedAt.IsZero() {
		t.Fatalf("ACTION_RESULT записан неполно: %+v", storedResult)
	}

	eventEnvelope, err := protocol.NewEnvelope(protocol.MessageAgentEvent, "audit-event-1", protocol.AgentEvent{
		SessionID: "session-1",
		Kind:      "USER_INPUT",
		Severity:  "warning",
		Message:   "automation paused",
		FrameID:   53,
		At:        completedAt,
		Details:   map[string]string{"source": "mouse"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := wsjson.Write(ctx, conn, eventEnvelope); err != nil {
		t.Fatal(err)
	}
	var storedEvent domain.AgentEventRecord
	waitUntil(t, time.Second, func() bool {
		var loadErr error
		storedEvent, loadErr = store.Event(context.Background(), "audit-event-1")
		return loadErr == nil
	})
	if storedEvent.AgentID != "audit-agent" ||
		storedEvent.SessionID != "session-1" ||
		storedEvent.Kind != "USER_INPUT" {
		t.Fatalf("AGENT_EVENT записан неполно: %+v", storedEvent)
	}
	var eventPayload protocol.AgentEvent
	if err := json.Unmarshal(storedEvent.Payload, &eventPayload); err != nil {
		t.Fatalf("payload AGENT_EVENT повреждён: %v", err)
	}
	if eventPayload.FrameID != 53 || eventPayload.Details["source"] != "mouse" {
		t.Fatalf("payload AGENT_EVENT неполон: %+v", eventPayload)
	}
}

func TestActionResultAuditFailurePausesAndReturnsContextualError(t *testing.T) {
	t.Parallel()

	store := &failingInboundAuditStore{}
	server := controller.NewServer(
		discardLogger(),
		controller.WithAuditStore(store),
		controller.WithAuditTimeout(25*time.Millisecond),
	)
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()
	conn := connectAgent(t, httpServer.URL, "audit-failure-agent", "v1")
	waitForAgents(t, httpServer.URL, 1)
	if err := server.SetMode(domain.ModeScan); err != nil {
		t.Fatal(err)
	}

	resultCh := make(chan error, 1)
	go func() {
		result, err := server.RequestAction(context.Background(), "audit-failure-agent", protocol.ActionRequest{
			ID:                 "audit-failure-action",
			SessionID:          "session-1",
			BasedOnFrame:       1,
			ExpectedState:      domain.StateMainMenu,
			ExpectedWidth:      1280,
			ExpectedHeight:     1024,
			ExpectedDPIPercent: 100,
			Deadline:           time.Now().Add(time.Second),
			Action:             protocol.Action{Kind: "CLICK"},
		})
		if err == nil && !result.Success {
			err = errors.New("получен неуспешный результат")
		}
		resultCh <- err
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var request protocol.Envelope
	if err := wsjson.Read(ctx, conn, &request); err != nil {
		t.Fatal(err)
	}
	response, err := protocol.NewCorrelatedEnvelope(
		protocol.MessageActionResult,
		"failed-persistence-result",
		request.MessageID,
		protocol.ActionResult{ID: request.MessageID, Success: true},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := wsjson.Write(ctx, conn, response); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-resultCh:
		if !errors.Is(err, controller.ErrAuditPersistence) {
			t.Fatalf("ошибка SaveActionResult = %v, ожидалась ErrAuditPersistence", err)
		}
	case <-ctx.Done():
		t.Fatal("ошибка SaveActionResult не была доставлена ожидающему запросу")
	}
	if server.Mode() != domain.ModePaused {
		t.Fatalf("после потери ACTION_RESULT режим = %s, ожидался PAUSED", server.Mode())
	}
}

func TestActionAuditFailurePreventsUnauditedInput(t *testing.T) {
	t.Parallel()

	store := &failingActionAuditStore{}
	server := controller.NewServer(discardLogger(), controller.WithAuditStore(store))
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()
	connectAgent(t, httpServer.URL, "audit-closed-agent", "v1")
	waitForAgents(t, httpServer.URL, 1)
	if err := server.SetMode(domain.ModeScan); err != nil {
		t.Fatal(err)
	}

	_, err := server.RequestAction(context.Background(), "audit-closed-agent", protocol.ActionRequest{
		ID:                 "must-not-be-sent",
		SessionID:          "session-1",
		BasedOnFrame:       1,
		ExpectedState:      domain.StateMainMenu,
		ExpectedWidth:      1280,
		ExpectedHeight:     1024,
		ExpectedDPIPercent: 100,
		Deadline:           time.Now().Add(time.Second),
		Action:             protocol.Action{Kind: "CLICK"},
	})
	if !errors.Is(err, controller.ErrAuditPersistence) {
		t.Fatalf("SaveAction failure error = %v, want ErrAuditPersistence", err)
	}
}

func TestSlowActionAuditDoesNotBlockSafetyPause(t *testing.T) {
	t.Parallel()

	store := &blockingActionAuditStore{
		started: make(chan struct{}),
		release: make(chan struct{}),
		results: make(chan domain.ActionResultRecord, 1),
	}
	server := controller.NewServer(
		discardLogger(),
		controller.WithAuditStore(store),
		controller.WithRequestTimeout(time.Second),
	)
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()
	conn := connectAgent(t, httpServer.URL, "blocking-audit-agent", "v1")
	waitForAgents(t, httpServer.URL, 1)
	if err := server.SetMode(domain.ModeScan); err != nil {
		t.Fatal(err)
	}

	actionErr := make(chan error, 1)
	go func() {
		_, err := server.RequestAction(
			context.Background(),
			"blocking-audit-agent",
			protocol.ActionRequest{
				ID:                 "blocked-before-wire",
				SessionID:          "session-1",
				BasedOnFrame:       1,
				ExpectedState:      domain.StateMainMenu,
				ExpectedWidth:      1280,
				ExpectedHeight:     1024,
				ExpectedDPIPercent: 100,
				Deadline:           time.Now().Add(time.Second),
				Action:             protocol.Action{Kind: "CLICK"},
			},
		)
		actionErr <- err
	}()

	select {
	case <-store.started:
	case <-time.After(time.Second):
		t.Fatal("запись ACTION_REQUEST не началась")
	}
	pauseDone := make(chan error, 1)
	go func() {
		pauseDone <- server.SetMode(domain.ModePaused)
	}()
	select {
	case err := <-pauseDone:
		if err != nil {
			t.Fatalf("аварийная пауза завершилась ошибкой: %v", err)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("медленный аудит заблокировал аварийную паузу")
	}
	close(store.release)

	select {
	case err := <-actionErr:
		if !errors.Is(err, controller.ErrActionNotSent) ||
			!errors.Is(err, controller.ErrModeDisallowsInput) {
			t.Fatalf("неотправленное действие вернуло неверную ошибку: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("неотправленное действие не завершилось")
	}
	select {
	case result := <-store.results:
		if result.Success || !result.NotSent ||
			result.ActionID != "blocked-before-wire" ||
			!strings.Contains(result.Error, "гарантированно не отправлено") {
			t.Fatalf("аудит локального отказа неполон: %+v", result)
		}
	case <-time.After(time.Second):
		t.Fatal("локальный отказ не закрыл запись ACTION_REQUEST")
	}

	readCtx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	var unexpected protocol.Envelope
	if err := wsjson.Read(readCtx, conn, &unexpected); err == nil {
		t.Fatalf("агент получил действие после аварийной паузы: %+v", unexpected)
	}
}

func TestControllerEmergencyStopRequiresCorrelatedAcknowledgement(t *testing.T) {
	t.Parallel()

	server := controller.NewServer(discardLogger(), controller.WithRequestTimeout(time.Second))
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()
	conn := connectAgent(t, httpServer.URL, "stop-ack-agent", "v1")
	waitForAgents(t, httpServer.URL, 1)
	if err := server.SetMode(domain.ModeScan); err != nil {
		t.Fatal(err)
	}

	result := make(chan error, 1)
	go func() {
		result <- server.SendEmergencyStop(
			context.Background(),
			"stop-ack-agent",
			"проверка подтверждения",
		)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var request protocol.Envelope
	if err := wsjson.Read(ctx, conn, &request); err != nil {
		t.Fatalf("агент не получил EMERGENCY_STOP: %v", err)
	}
	if request.Type != protocol.MessageEmergencyStop || request.MessageID == "" {
		t.Fatalf("получена неверная команда остановки: %+v", request)
	}
	select {
	case err := <-result:
		t.Fatalf("остановка завершилась до подтверждения агента: %v", err)
	default:
	}
	response, err := protocol.NewCorrelatedEnvelope(
		protocol.MessageAgentEvent,
		"stop-applied-event",
		request.MessageID,
		protocol.AgentEvent{
			Kind:     "EMERGENCY_STOP_APPLIED",
			Severity: "critical",
			Message:  "проверка подтверждения",
			At:       time.Now().UTC(),
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := wsjson.Write(ctx, conn, response); err != nil {
		t.Fatalf("не удалось отправить подтверждение остановки: %v", err)
	}
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("подтверждённая остановка завершилась ошибкой: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("controller не принял подтверждение аварийной остановки")
	}
	if server.Mode() != domain.ModePaused {
		t.Fatalf("после остановки режим = %s, ожидался PAUSED", server.Mode())
	}
	state, ok := server.AgentState("stop-ack-agent")
	if !ok || !state.EmergencyStopped || !state.AutomationPaused ||
		state.SafetyReason != "проверка подтверждения" {
		t.Fatalf("подтверждённая остановка не отражена в состоянии агента: %+v", state)
	}
}

func TestHeartbeatTimeoutIsNotExtendedByOtherTraffic(t *testing.T) {
	t.Parallel()

	server := controller.NewServer(discardLogger(), controller.WithHeartbeatTimeout(250*time.Millisecond))
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()
	conn := connectAgent(t, httpServer.URL, "silent-heartbeat", "v1")
	waitForAgents(t, httpServer.URL, 1)

	stopTraffic := make(chan struct{})
	defer close(stopTraffic)
	go func() {
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stopTraffic:
				return
			case at := <-ticker.C:
				envelope, err := protocol.NewEnvelope(protocol.MessageAgentEvent, "event", protocol.AgentEvent{Kind: "BUSY", At: at})
				if err != nil {
					return
				}
				writeCtx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
				err = wsjson.Write(writeCtx, conn, envelope)
				cancel()
				if err != nil {
					return
				}
			}
		}
	}()

	waitForAgentsWithin(t, httpServer.URL, 0, 2*time.Second)
}

type failingInboundAuditStore struct{}

func (*failingInboundAuditStore) SaveAction(context.Context, domain.ActionRecord) error {
	return nil
}

func (*failingInboundAuditStore) SaveActionResult(ctx context.Context, _ domain.ActionResultRecord) error {
	<-ctx.Done()
	return ctx.Err()
}

func (*failingInboundAuditStore) SaveEvent(context.Context, domain.AgentEventRecord) error {
	return errors.New("event storage unavailable")
}

type failingActionAuditStore struct{}

func (*failingActionAuditStore) SaveAction(context.Context, domain.ActionRecord) error {
	return errors.New("action storage unavailable")
}

func (*failingActionAuditStore) SaveActionResult(context.Context, domain.ActionResultRecord) error {
	return nil
}

func (*failingActionAuditStore) SaveEvent(context.Context, domain.AgentEventRecord) error {
	return nil
}

type blockingActionAuditStore struct {
	started chan struct{}
	release chan struct{}
	results chan domain.ActionResultRecord
}

func (s *blockingActionAuditStore) SaveAction(
	ctx context.Context,
	_ domain.ActionRecord,
) error {
	close(s.started)
	select {
	case <-s.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *blockingActionAuditStore) SaveActionResult(
	ctx context.Context,
	record domain.ActionResultRecord,
) error {
	select {
	case s.results <- record:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (*blockingActionAuditStore) SaveEvent(
	context.Context,
	domain.AgentEventRecord,
) error {
	return nil
}

func waitUntil(t *testing.T, timeout time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("условие не выполнено до deadline")
}

func TestAPIExposesStateFrameAndExplicitMode(t *testing.T) {
	t.Parallel()

	server := controller.NewServer(discardLogger())
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()
	conn := connectAgent(t, httpServer.URL, "api-agent", "v1")
	waitForAgents(t, httpServer.URL, 1)

	frame := protocol.Frame{
		ID:         33,
		CapturedAt: time.Now().UTC(),
		Encoding:   "png",
		Data:       []byte{1, 2, 3, 4},
		Region:     domain.Rectangle{Width: 1, Height: 1},
	}
	envelope, err := protocol.NewEnvelope(protocol.MessageFrame, "passive-frame", frame)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := wsjson.Write(ctx, conn, envelope); err != nil {
		t.Fatal(err)
	}
	waitForFrame(t, server, "api-agent", 33)

	response, err := http.Get(httpServer.URL + "/api/v1/state")
	if err != nil {
		t.Fatal(err)
	}
	var state controller.RuntimeState
	decodeResponse(t, response, http.StatusOK, &state)
	if state.Mode != domain.ModePaused || len(state.Agents) != 1 {
		t.Fatalf("неожиданный API state: %+v", state)
	}

	response, err = http.Get(httpServer.URL + "/api/v1/agents/api-agent/frame?raw=true")
	if err != nil {
		t.Fatal(err)
	}
	raw, readErr := io.ReadAll(response.Body)
	response.Body.Close()
	if readErr != nil || response.StatusCode != http.StatusOK || !bytes.Equal(raw, frame.Data) {
		t.Fatalf("raw frame endpoint: status=%d data=%v err=%v", response.StatusCode, raw, readErr)
	}

	modeBody := strings.NewReader(`{"mode":"OBSERVE"}`)
	request, err := http.NewRequest(http.MethodPut, httpServer.URL+"/api/v1/mode", modeBody)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	var mode map[string]any
	decodeResponse(t, response, http.StatusOK, &mode)
	if server.Mode() != domain.ModeObserve {
		t.Fatalf("PUT mode не установил OBSERVE: %s", server.Mode())
	}

	actionBody := strings.NewReader(`{
		"type":"ACTION_REQUEST",
		"action":{
			"id":"blocked-action",
			"based_on_frame":33,
			"expected_state":"MAIN_MENU",
			"action":{"kind":"CLICK","value":"left"}
		}
	}`)
	response, err = http.Post(httpServer.URL+"/api/v1/agents/api-agent/commands", "application/json", actionBody)
	if err != nil {
		t.Fatal(err)
	}
	var apiError map[string]string
	decodeResponse(t, response, http.StatusForbidden, &apiError)
	if !strings.Contains(apiError["error"], "commands API отключён") {
		t.Fatalf("неожиданная ошибка commands API: %v", apiError)
	}
}

func controllerBasisPNG(t *testing.T) []byte {
	t.Helper()

	pixels := image.NewNRGBA(image.Rect(0, 0, 4, 3))
	for y := 0; y < pixels.Bounds().Dy(); y++ {
		for x := 0; x < pixels.Bounds().Dx(); x++ {
			pixels.SetNRGBA(x, y, color.NRGBA{
				R: uint8(10 + x),
				G: uint8(20 + y),
				B: 30,
				A: 255,
			})
		}
	}
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, pixels); err != nil {
		t.Fatalf("png.Encode() error = %v", err)
	}
	return encoded.Bytes()
}

func connectAgent(t *testing.T, serverURL, agentID, version string) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	wsURL := "ws" + strings.TrimPrefix(serverURL, "http") + "/ws/agent"
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("websocket.Dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.CloseNow() })
	hello, err := protocol.NewEnvelope(protocol.MessageHello, "hello-"+agentID+"-"+version, protocol.Hello{
		AgentID: agentID,
		Version: version,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := wsjson.Write(ctx, conn, hello); err != nil {
		t.Fatalf("HELLO: %v", err)
	}
	return conn
}

func sendAgentEvent(
	t *testing.T,
	conn *websocket.Conn,
	messageID string,
	event protocol.AgentEvent,
) {
	t.Helper()
	envelope, err := protocol.NewEnvelope(protocol.MessageAgentEvent, messageID, event)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := wsjson.Write(ctx, conn, envelope); err != nil {
		t.Fatal(err)
	}
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func waitForAgentVersion(t *testing.T, server *controller.Server, agentID, version string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		state, ok := server.AgentState(agentID)
		if ok && state.Version == version {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("агент %q не перешёл на version %q", agentID, version)
}

func waitForAgentsWithin(t *testing.T, serverURL string, want int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		response, err := http.Get(serverURL + "/healthz")
		if err == nil {
			var health struct {
				Agents int `json:"agents"`
			}
			decodeErr := json.NewDecoder(response.Body).Decode(&health)
			response.Body.Close()
			if decodeErr == nil && health.Agents == want {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("число агентов не стало равным %d за %s", want, timeout)
}

func waitForFrame(t *testing.T, server *controller.Server, agentID string, frameID uint64) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		frame, ok := server.LatestFrame(agentID)
		if ok && frame.ID == frameID {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("frame %d не появился у %s", frameID, agentID)
}

func decodeResponse(t *testing.T, response *http.Response, status int, destination any) {
	t.Helper()
	defer response.Body.Close()
	if response.StatusCode != status {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("status=%d, ожидался %d: %s", response.StatusCode, status, body)
	}
	if err := json.NewDecoder(response.Body).Decode(destination); err != nil {
		t.Fatalf("не удалось декодировать HTTP response: %v", err)
	}
}

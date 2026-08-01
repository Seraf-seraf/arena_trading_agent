package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arena-trading-agent/arena-trading-agent/internal/domain"
	"github.com/arena-trading-agent/arena-trading-agent/internal/protocol"
)

func TestPublishEmergencyStopConcurrentNeverBlocks(t *testing.T) {
	t.Parallel()

	client := NewClient("ws://127.0.0.1:8787/ws/agent", "agent", "v1", nil)
	const publishers = 64
	var wait sync.WaitGroup
	wait.Add(publishers)
	for index := 0; index < publishers; index++ {
		go func(value int) {
			defer wait.Done()
			client.PublishEmergencyStop(fmt.Sprintf("причина-%d", value), true)
		}(index)
	}
	done := make(chan struct{})
	go func() {
		wait.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("конкурентная публикация emergency stop заблокировалась")
	}
	select {
	case stop := <-client.emergencies:
		if !strings.Contains(stop.Reason, "agent.Client.PublishEmergencyStop") {
			t.Fatalf("причина не содержит контекст метода: %q", stop.Reason)
		}
	default:
		t.Fatal("очередь не сохранила аварийную остановку")
	}
}

func TestActionResultIsReplayedWithoutReservingSecondExecution(t *testing.T) {
	client := NewRuntimeClient(ClientOptions{})
	request := protocol.ActionRequest{
		ID:            "stable-action-id",
		SessionID:     "session-1",
		BasedOnFrame:  10,
		ExpectedState: domain.StateMarketHome,
		Deadline:      time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC),
		Action:        protocol.Action{Kind: "CLICK", Value: "LEFT"},
	}

	cached, fingerprint, err := client.reserveAction(request)
	if err != nil || cached != nil {
		t.Fatalf("первая резервация: cached=%+v err=%v", cached, err)
	}
	frame := protocol.Frame{ID: 11, Encoding: "png", Data: []byte{1, 2, 3}}
	want := protocol.ActionResult{
		ID:          request.ID,
		Success:     true,
		ResultFrame: frame.ID,
		ResultState: domain.StateMarketHome,
		Frame:       &frame,
		CompletedAt: time.Now().UTC(),
	}
	client.rememberActionResult(request.ID, fingerprint, want)

	cached, _, err = client.reserveAction(request)
	if err != nil || cached == nil {
		t.Fatalf("повтор завершённой команды: cached=%+v err=%v", cached, err)
	}
	if cached.ID != want.ID || cached.ResultFrame != want.ResultFrame ||
		cached.Frame == nil || len(cached.Frame.Data) != 3 {
		t.Fatalf("возвращён другой cached result: %+v", cached)
	}
}

func TestActionIDCannotBeReusedForDifferentCommand(t *testing.T) {
	client := NewRuntimeClient(ClientOptions{})
	request := protocol.ActionRequest{
		ID:       "one-id",
		Deadline: time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC),
		Action:   protocol.Action{Kind: "CLICK", Value: "LEFT"},
	}
	if _, _, err := client.reserveAction(request); err != nil {
		t.Fatal(err)
	}
	request.Action.Value = "RIGHT"
	_, _, err := client.reserveAction(request)
	if err == nil || !strings.Contains(err.Error(), "другой команды") {
		t.Fatalf("повтор id с другим body не отклонён: %v", err)
	}
}

func TestRejectedBusyReservationCanBeRetried(t *testing.T) {
	client := NewRuntimeClient(ClientOptions{})
	request := protocol.ActionRequest{
		ID:       "not-enqueued",
		Deadline: time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC),
		Action:   protocol.Action{Kind: "CLICK"},
	}
	_, fingerprint, err := client.reserveAction(request)
	if err != nil {
		t.Fatal(err)
	}
	client.cancelActionReservation(request.ID, fingerprint)
	cached, _, err := client.reserveAction(request)
	if err != nil || cached != nil {
		t.Fatalf("отменённую резервацию нельзя повторить: cached=%+v err=%v", cached, err)
	}
}

func TestEvictedHeavyResultLeavesNonExecutableTombstone(t *testing.T) {
	client := NewRuntimeClient(ClientOptions{})
	first := protocol.ActionRequest{
		ID:        "evicted-result",
		SessionID: "session-1",
		Deadline:  time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC),
		Action:    protocol.Action{Kind: "CLICK", Value: "LEFT"},
	}
	_, fingerprint, err := client.reserveAction(first)
	if err != nil {
		t.Fatal(err)
	}
	client.rememberActionResult(first.ID, fingerprint, protocol.ActionResult{
		ID: first.ID, Success: true, CompletedAt: time.Now().UTC(),
	})

	for index := 0; index < maxRememberedActions; index++ {
		request := first
		request.ID = fmt.Sprintf("newer-result-%d", index)
		_, requestFingerprint, reserveErr := client.reserveAction(request)
		if reserveErr != nil {
			t.Fatalf("резервация %d: %v", index, reserveErr)
		}
		client.rememberActionResult(request.ID, requestFingerprint, protocol.ActionResult{
			ID: request.ID, Success: true, CompletedAt: time.Now().UTC(),
		})
	}

	cached, _, err := client.reserveAction(first)
	if cached != nil {
		t.Fatalf("удалённый тяжёлый результат неожиданно остался в кэше: %+v", cached)
	}
	if err == nil || !strings.Contains(err.Error(), "повторное исполнение запрещено") {
		t.Fatalf("старое действие после очистки кэша не отклонено: %v", err)
	}
}

func TestEvictedTombstoneIsRememberedByBoundedFilter(t *testing.T) {
	client := NewRuntimeClient(ClientOptions{})
	first := protocol.ActionRequest{
		ID:        "oldest-action",
		SessionID: "session-1",
		Deadline:  time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC),
		Action:    protocol.Action{Kind: "CLICK", Value: "LEFT"},
	}

	for index := 0; index <= maxRememberedActionTombstones; index++ {
		request := first
		if index > 0 {
			request.ID = fmt.Sprintf("bounded-action-%d", index)
		}
		_, fingerprint, err := client.reserveAction(request)
		if err != nil {
			t.Fatalf("резервация %d: %v", index, err)
		}
		client.rememberActionResult(request.ID, fingerprint, protocol.ActionResult{
			ID: request.ID, Success: true, CompletedAt: time.Now().UTC(),
		})
	}
	if len(client.actions) > maxRememberedActionTombstones {
		t.Fatalf("журнал действий вырос сверх лимита: %d", len(client.actions))
	}

	cached, _, err := client.reserveAction(first)
	if cached != nil {
		t.Fatalf("для удалённого tombstone неожиданно найден результат: %+v", cached)
	}
	if err == nil || !strings.Contains(err.Error(), "повторное исполнение запрещено") {
		t.Fatalf("удалённый tombstone разрешил повтор команды: %v", err)
	}
}

func TestSecondCaptureRequestGetsCorrelatedBusyEvent(t *testing.T) {
	client := NewRuntimeClient(ClientOptions{})
	outbound := make(chan protocol.Envelope, 1)
	actionQueue := make(chan actionJob)
	captureQueue := make(chan captureJob, 1)
	var captureBusy atomic.Bool
	ctx := context.Background()

	first := mustClientEnvelope(t, protocol.MessageFrameRequest, "frame-request-1", protocol.FrameRequest{})
	if err := client.handle(
		ctx,
		first,
		outbound,
		actionQueue,
		captureQueue,
		&captureBusy,
	); err != nil {
		t.Fatalf("первый запрос захвата отклонён: %v", err)
	}
	if len(captureQueue) != 1 || cap(captureQueue) != 1 {
		t.Fatalf("очередь захвата имеет неверный размер: len=%d cap=%d", len(captureQueue), cap(captureQueue))
	}

	second := mustClientEnvelope(t, protocol.MessageFrameRequest, "frame-request-2", protocol.FrameRequest{})
	if err := client.handle(
		ctx,
		second,
		outbound,
		actionQueue,
		captureQueue,
		&captureBusy,
	); err != nil {
		t.Fatalf("занятость захвата вернула ошибку транспорта: %v", err)
	}

	response := <-outbound
	if response.Type != protocol.MessageAgentEvent ||
		response.CorrelationID != second.MessageID {
		t.Fatalf("ответ о занятости не скоррелирован: %+v", response)
	}
	var event protocol.AgentEvent
	if err := json.Unmarshal(response.Payload, &event); err != nil {
		t.Fatal(err)
	}
	if event.Kind != "CAPTURE_BUSY" {
		t.Fatalf("получено другое событие: %+v", event)
	}
}

func TestActionLoopPassesRequestDeadlineToExecutor(t *testing.T) {
	input := &deadlineRecordingInput{deadlines: make(chan time.Time, 1)}
	data := []byte("stable-screen")
	baseCapturedAt := time.Now().Add(-500 * time.Millisecond)
	preInput := protocol.Frame{
		ID:            42,
		CapturedAt:    time.Now().Add(-100 * time.Millisecond),
		ContentDigest: protocol.ComputeFrameDigest(data),
		Encoding:      "png",
		Data:          data,
	}
	var latest atomic.Uint64
	latest.Store(41)
	executor := NewActionExecutor(
		input,
		&deadlineCaptureDriver{frame: preInput, latest: &latest},
		staticWindowManager{},
		deadlineStateDetector{},
		latest.Load,
		func() bool { return false },
	)
	client := NewRuntimeClient(ClientOptions{Executor: executor})
	request := protocol.ActionRequest{
		ID:                        "deadline-action",
		SessionID:                 "session-1",
		BasedOnFrame:              41,
		BasedOnCapturedAt:         &baseCapturedAt,
		BasedOnFrameDigest:        protocol.ComputeFrameDigest(data),
		BasedOnState:              domain.StateMarketHome,
		ExpectedState:             domain.StateMarketHome,
		MinVerificationConfidence: 0.8,
		ExpectedWidth:             1920,
		ExpectedHeight:            1080,
		ExpectedDPIPercent:        100,
		Class:                     protocol.ActionNavigation,
		Deadline:                  time.Now().Add(time.Minute),
		Action:                    protocol.Action{Kind: "CLICK", Value: "LEFT"},
	}
	_, fingerprint, err := client.reserveAction(request)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan struct{})
	outbound := make(chan protocol.Envelope, 1)
	queue := make(chan actionJob)
	go func() {
		defer close(done)
		client.actionLoop(ctx, outbound, queue)
	}()
	queue <- actionJob{
		correlationID: "deadline-message",
		request:       request,
		fingerprint:   fingerprint,
	}

	select {
	case actual := <-input.deadlines:
		if !actual.Equal(request.Deadline) {
			t.Fatalf("исполнитель получил другой deadline: got=%s want=%s", actual, request.Deadline)
		}
	case <-time.After(time.Second):
		t.Fatal("исполнитель не получил действие")
	}
	select {
	case <-outbound:
	case <-time.After(time.Second):
		t.Fatal("цикл действий не отправил результат")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("цикл действий не завершился после отмены")
	}
}

func TestActionLoopChecksStoppedBeforeExecutor(t *testing.T) {
	input := &deadlineRecordingInput{deadlines: make(chan time.Time, 1)}
	executor := NewActionExecutor(
		input,
		unusedCaptureDriver{},
		staticWindowManager{},
		unusedStateDetector{},
		func() uint64 { return 41 },
		func() bool { return false },
	)
	client := NewRuntimeClient(ClientOptions{
		Executor:  executor,
		IsStopped: func() bool { return true },
	})
	request := protocol.ActionRequest{
		ID:                 "stopped-action",
		SessionID:          "session-1",
		BasedOnFrame:       41,
		ExpectedState:      domain.StateMarketHome,
		ExpectedWidth:      1920,
		ExpectedHeight:     1080,
		ExpectedDPIPercent: 100,
		Deadline:           time.Now().Add(time.Minute),
		Action:             protocol.Action{Kind: "CLICK", Value: "LEFT"},
	}
	_, fingerprint, err := client.reserveAction(request)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan struct{})
	outbound := make(chan protocol.Envelope, 1)
	queue := make(chan actionJob)
	go func() {
		defer close(done)
		client.actionLoop(ctx, outbound, queue)
	}()
	queue <- actionJob{
		correlationID: "stopped-message",
		request:       request,
		fingerprint:   fingerprint,
	}

	var result protocol.ActionResult
	select {
	case envelope := <-outbound:
		if err := json.Unmarshal(envelope.Payload, &result); err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("цикл действий не отправил отказ остановленного агента")
	}
	if !strings.Contains(result.Error, "агент остановлен") {
		t.Fatalf("получен неверный отказ: %+v", result)
	}
	select {
	case deadline := <-input.deadlines:
		t.Fatalf("остановленный агент вызвал InputDriver с deadline %s", deadline)
	default:
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("цикл действий не завершился после отмены")
	}
}

func mustClientEnvelope(
	t *testing.T,
	messageType protocol.MessageType,
	messageID string,
	payload any,
) protocol.Envelope {
	t.Helper()
	envelope, err := protocol.NewEnvelope(messageType, messageID, payload)
	if err != nil {
		t.Fatal(err)
	}
	return envelope
}

type deadlineRecordingInput struct {
	deadlines chan time.Time
}

func (d *deadlineRecordingInput) Move(context.Context, domain.Point) error {
	return errors.New("тестовый драйвер не поддерживает перемещение")
}

func (d *deadlineRecordingInput) Click(ctx context.Context, _ string) error {
	deadline, ok := ctx.Deadline()
	if !ok {
		return errors.New("контекст действия не содержит deadline")
	}
	d.deadlines <- deadline
	return errors.New("тестовая остановка после проверки deadline")
}

func (d *deadlineRecordingInput) Scroll(context.Context, int) error {
	return errors.New("тестовый драйвер не поддерживает прокрутку")
}

func (d *deadlineRecordingInput) Key(context.Context, string) error {
	return errors.New("тестовый драйвер не поддерживает клавиатуру")
}

func (d *deadlineRecordingInput) Text(context.Context, string) error {
	return errors.New("тестовый драйвер не поддерживает ввод текста")
}

type unusedCaptureDriver struct{}

func (unusedCaptureDriver) Capture(context.Context) (protocol.Frame, error) {
	return protocol.Frame{}, errors.New("тестовый захват не должен вызываться")
}

func (unusedCaptureDriver) CaptureRegion(context.Context, domain.Rectangle) (protocol.Frame, error) {
	return protocol.Frame{}, errors.New("тестовый захват области не должен вызываться")
}

type deadlineCaptureDriver struct {
	frame  protocol.Frame
	latest *atomic.Uint64
}

func (driver *deadlineCaptureDriver) Capture(context.Context) (protocol.Frame, error) {
	driver.latest.Store(driver.frame.ID)
	return driver.frame, nil
}

func (*deadlineCaptureDriver) CaptureRegion(
	context.Context,
	domain.Rectangle,
) (protocol.Frame, error) {
	return protocol.Frame{}, errors.New("тестовый захват области не должен вызываться")
}

type staticWindowManager struct{}

func (staticWindowManager) Status(context.Context) (protocol.WindowStatus, error) {
	return protocol.WindowStatus{
		Active: true, Width: 1920, Height: 1080, DPIPercent: 100,
	}, nil
}

type unusedStateDetector struct{}

func (unusedStateDetector) Detect(context.Context, protocol.Frame) (domain.ScreenState, float64, error) {
	return domain.StateUnknown, 0, errors.New("тестовый детектор не должен вызываться")
}

type deadlineStateDetector struct{}

func (deadlineStateDetector) Detect(
	context.Context,
	protocol.Frame,
) (domain.ScreenState, float64, error) {
	return domain.StateMarketHome, 1, nil
}

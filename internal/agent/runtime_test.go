package agent_test

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arena-trading-agent/arena-trading-agent/internal/agent"
	"github.com/arena-trading-agent/arena-trading-agent/internal/domain"
	"github.com/arena-trading-agent/arena-trading-agent/internal/protocol"
)

func TestActionExecutorRejectsStaleFrameBeforeInput(t *testing.T) {
	executor := agent.NewActionExecutor(nil, nil, nil, nil, func() uint64 { return 42 }, func() bool { return false })
	base := testFrame(41, time.Now().UTC(), []byte{1})
	request := boundRequest(base, domain.StateMainMenu, domain.StateMainMenu)
	result := executor.Execute(context.Background(), request)
	if result.Success {
		t.Fatal("команда на устаревшем кадре не должна исполняться")
	}
	if !strings.Contains(result.Error, "устаревшем кадре") {
		t.Fatalf("неожиданная ошибка: %q", result.Error)
	}
	if !result.RetrySafe {
		t.Fatal("отказ до ввода должен быть явно помечен как безопасный для повтора")
	}
}

func TestActionExecutorRejectsEmergencyStopBeforeInput(t *testing.T) {
	executor := agent.NewActionExecutor(nil, nil, nil, nil, func() uint64 { return 42 }, func() bool { return true })
	result := executor.Execute(context.Background(), protocol.ActionRequest{ID: "action-1", BasedOnFrame: 42})
	if result.Success || !strings.Contains(result.Error, "аварийной кнопкой") {
		t.Fatalf("аварийная остановка не отклонила команду: %+v", result)
	}
}

func TestActionExecutorReturnsExactVerificationFrame(t *testing.T) {
	input := &fakeInput{}
	base := testFrame(42, time.Now().UTC().Add(-3*time.Millisecond), []byte{1, 2, 3})
	preInput := testFrame(43, base.CapturedAt.Add(time.Millisecond), base.Data)
	frame := testFrame(44, base.CapturedAt.Add(2*time.Millisecond), []byte{4, 5, 6})
	var latest atomic.Uint64
	latest.Store(base.ID)
	executor := agent.NewActionExecutor(
		input,
		&fakeCapture{frames: []protocol.Frame{preInput, frame}, latest: &latest},
		fakeWindow{status: protocol.WindowStatus{
			Active: true, Width: 1280, Height: 1024, DPIPercent: 100,
		}},
		&fakeDetector{responses: []detectionResponse{
			{state: domain.StateMainMenu, confidence: .94},
			{state: domain.StateMarketHome, confidence: .93},
		}},
		latest.Load,
		func() bool { return false },
	)
	request := boundRequest(base, domain.StateMainMenu, domain.StateMarketHome)
	request.ID = "action-with-frame"
	request.Action = protocol.Action{Kind: "CLICK", Value: "LEFT"}
	result := executor.Execute(context.Background(), request)
	if !result.Success || input.clicks != 1 {
		t.Fatalf("действие не выполнено: result=%+v clicks=%d", result, input.clicks)
	}
	if result.Frame == nil || result.Frame.ID != 44 || result.ResultFrame != 44 {
		t.Fatalf("контрольный кадр отсутствует: %+v", result)
	}
	if result.VerificationConfidence != .93 {
		t.Fatalf("confidence=%v, ожидалось .93", result.VerificationConfidence)
	}
}

func TestActionExecutorSetsCompletedAtAfterControlCapture(t *testing.T) {
	captureStarted := make(chan struct{})
	releaseCapture := make(chan struct{})
	resultReady := make(chan protocol.ActionResult, 1)
	base := testFrame(42, time.Now().UTC().Add(-3*time.Millisecond), []byte{1})
	preInput := testFrame(43, base.CapturedAt.Add(time.Millisecond), base.Data)
	frame := testFrame(44, base.CapturedAt.Add(2*time.Millisecond), []byte{2})
	var latest atomic.Uint64
	latest.Store(base.ID)
	executor := agent.NewActionExecutor(
		&fakeInput{},
		&fakeCapture{
			frames: []protocol.Frame{preInput, frame},
			latest: &latest,
			beforeReturn: func(call int) {
				if call == 2 {
					close(captureStarted)
					<-releaseCapture
				}
			},
		},
		fakeWindow{status: protocol.WindowStatus{
			Active: true, Width: 1280, Height: 1024, DPIPercent: 100,
		}},
		&fakeDetector{responses: []detectionResponse{
			{state: domain.StateMainMenu, confidence: .93},
			{state: domain.StateMarketHome, confidence: .93},
		}},
		latest.Load,
		func() bool { return false },
	)
	go func() {
		request := boundRequest(base, domain.StateMainMenu, domain.StateMarketHome)
		request.ID = "action-completion-time"
		request.Deadline = time.Now().Add(time.Minute)
		request.Action = protocol.Action{Kind: "CLICK", Value: "LEFT"}
		resultReady <- executor.Execute(context.Background(), request)
	}()

	<-captureStarted
	captureReleasedAt := time.Now().UTC()
	close(releaseCapture)
	result := <-resultReady
	if !result.Success {
		t.Fatalf("действие не выполнено: %+v", result)
	}
	if result.CompletedAt.Before(captureReleasedAt) {
		t.Fatalf(
			"время завершения %s предшествует окончанию контрольного захвата %s",
			result.CompletedAt,
			captureReleasedAt,
		)
	}
}

func TestActionExecutorValidatesAndRunsSequenceAtomically(t *testing.T) {
	input := &fakeInput{}
	base := testFrame(1, time.Now().UTC().Add(-3*time.Millisecond), []byte{1})
	preInput := testFrame(2, base.CapturedAt.Add(time.Millisecond), base.Data)
	frame := testFrame(3, base.CapturedAt.Add(2*time.Millisecond), []byte{2})
	var latest atomic.Uint64
	latest.Store(base.ID)
	executor := agent.NewActionExecutor(
		input,
		&fakeCapture{frames: []protocol.Frame{preInput, frame}, latest: &latest},
		fakeWindow{status: protocol.WindowStatus{
			Active: true, Width: 1280, Height: 1024, DPIPercent: 100,
		}},
		&fakeDetector{responses: []detectionResponse{
			{state: domain.StateMarketSearch, confidence: .95},
			{state: domain.StateMarketResults, confidence: .95},
		}},
		latest.Load,
		func() bool { return false },
	)
	request := boundRequest(base, domain.StateMarketSearch, domain.StateMarketResults)
	request.ID = "sequence-1"
	request.Action = protocol.Action{
		Kind: "SEQUENCE",
		Steps: []protocol.Action{
			{Kind: "CLICK", Point: &domain.Point{X: .5, Y: .25}, Value: "LEFT"},
			{Kind: "KEY", Value: "CTRL+A"},
			{Kind: "TEXT", Value: "item"},
			{Kind: "KEY", Value: "ENTER"},
		},
	}
	result := executor.Execute(context.Background(), request)
	if !result.Success {
		t.Fatalf("sequence отклонена: %+v", result)
	}
	want := []string{"move", "click", "key", "text", "key"}
	if len(input.operations) != len(want) {
		t.Fatalf("operations=%v, want=%v", input.operations, want)
	}
	for index := range want {
		if input.operations[index] != want[index] {
			t.Fatalf("operations=%v, want=%v", input.operations, want)
		}
	}
}

func TestActionExecutorCancelsLongInputAfterLocalPause(t *testing.T) {
	t.Parallel()

	input := &blockingTextInput{started: make(chan struct{})}
	base := testFrame(50, time.Now().UTC().Add(-2*time.Millisecond), []byte("экран"))
	preInput := testFrame(51, base.CapturedAt.Add(time.Millisecond), base.Data)
	var latest atomic.Uint64
	latest.Store(base.ID)
	var paused atomic.Bool
	executor := agent.NewActionExecutor(
		input,
		&fakeCapture{frames: []protocol.Frame{preInput}, latest: &latest},
		fakeWindow{status: protocol.WindowStatus{
			Active: true, Width: 1280, Height: 1024, DPIPercent: 100,
		}},
		&fakeDetector{state: domain.StateMarketSearch, confidence: .99},
		latest.Load,
		paused.Load,
	)
	request := boundRequest(base, domain.StateMarketSearch, domain.StateMarketResults)
	request.ID = "long-text-pause"
	request.Deadline = time.Now().Add(time.Minute)
	request.Action = protocol.Action{Kind: "TEXT", Value: "длинный текст"}
	resultReady := make(chan protocol.ActionResult, 1)
	go func() {
		resultReady <- executor.Execute(context.Background(), request)
	}()

	<-input.started
	paused.Store(true)
	select {
	case result := <-resultReady:
		if result.Success || result.RetrySafe ||
			!strings.Contains(result.Error, "не удалось ввести текст") {
			t.Fatalf("длительный ввод не был безопасно остановлен: %+v", result)
		}
	case <-time.After(time.Second):
		t.Fatal("длительный ввод не остановился после локальной паузы")
	}
}

func TestActionExecutorStopsSequenceWhenSafetyStateChanges(t *testing.T) {
	tests := []struct {
		name             string
		triggerOperation string
		firstStep        protocol.Action
		wantOperations   []string
		activate         func(*agent.SafetySupervisor)
	}{
		{
			name:             "пауза перед следующим шагом",
			triggerOperation: "click",
			firstStep:        protocol.Action{Kind: "CLICK", Value: "LEFT"},
			wantOperations:   []string{"click"},
			activate: func(supervisor *agent.SafetySupervisor) {
				supervisor.Pause("запрошена локальная пауза")
			},
		},
		{
			name:             "аварийная остановка внутри составного шага",
			triggerOperation: "move",
			firstStep: protocol.Action{
				Kind: "CLICK", Point: &domain.Point{X: .5, Y: .25}, Value: "LEFT",
			},
			wantOperations: []string{"move"},
			activate: func(supervisor *agent.SafetySupervisor) {
				supervisor.Emergency("нажата аварийная кнопка", true)
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			supervisor := agent.NewSafetySupervisor(3)
			input := &fakeInput{}
			input.afterOperation = func(operation string) {
				if operation == test.triggerOperation {
					test.activate(supervisor)
				}
			}
			base := testFrame(1, time.Now().UTC().Add(-2*time.Millisecond), []byte{1})
			preInput := testFrame(2, base.CapturedAt.Add(time.Millisecond), base.Data)
			var latest atomic.Uint64
			latest.Store(base.ID)
			executor := agent.NewActionExecutor(
				input,
				&fakeCapture{frames: []protocol.Frame{preInput}, latest: &latest},
				fakeWindow{status: protocol.WindowStatus{
					Active: true, Width: 1280, Height: 1024, DPIPercent: 100,
				}},
				&fakeDetector{state: domain.StateMarketSearch, confidence: .95},
				latest.Load,
				supervisor.Paused,
			)
			request := boundRequest(base, domain.StateMarketSearch, domain.StateMarketResults)
			request.ID = "sequence-safety"
			request.Action = protocol.Action{
				Kind: "SEQUENCE",
				Steps: []protocol.Action{
					test.firstStep,
					{Kind: "KEY", Value: "ENTER"},
					{Kind: "TEXT", Value: "не должно вводиться"},
				},
			}
			result := executor.Execute(context.Background(), request)
			if result.Success {
				t.Fatalf("последовательность продолжилась после изменения состояния безопасности: %+v", result)
			}
			if !strings.Contains(result.Error, "agent.ActionExecutor.ensureInputAllowed") {
				t.Fatalf("ошибка не содержит контекст проверки безопасности: %q", result.Error)
			}
			if !equalOperations(input.operations, test.wantOperations) {
				t.Fatalf("после блокировки выполнены лишние операции: %v", input.operations)
			}
			if result.RetrySafe {
				t.Fatal("частично исполненная последовательность помечена безопасной для повтора")
			}
		})
	}
}

func TestActionExecutorRejectsNestedSequenceBeforeAnyInput(t *testing.T) {
	input := &fakeInput{}
	base := testFrame(1, time.Now().UTC(), []byte{1})
	executor := agent.NewActionExecutor(
		input,
		&fakeCapture{},
		fakeWindow{status: protocol.WindowStatus{
			Active: true, Width: 1, Height: 1, DPIPercent: 100,
		}},
		&fakeDetector{},
		func() uint64 { return 1 },
		func() bool { return false },
	)
	request := boundRequest(base, domain.StateMainMenu, domain.StateMainMenu)
	request.ID = "nested"
	request.ExpectedWidth, request.ExpectedHeight = 1, 1
	request.Action = protocol.Action{Kind: "SEQUENCE", Steps: []protocol.Action{
		{Kind: "CLICK"},
		{Kind: "SEQUENCE", Steps: []protocol.Action{{Kind: "CLICK"}}},
	}}
	result := executor.Execute(context.Background(), request)
	if result.Success || len(input.operations) != 0 {
		t.Fatalf("nested sequence partially executed: result=%+v operations=%v", result, input.operations)
	}
}

func TestActionExecutorRejectsChangedContentBeforeInput(t *testing.T) {
	input := &fakeInput{}
	base := testFrame(10, time.Now().UTC().Add(-2*time.Millisecond), []byte("старый экран"))
	preInput := testFrame(11, base.CapturedAt.Add(time.Millisecond), []byte("изменённый экран"))
	var latest atomic.Uint64
	latest.Store(base.ID)
	executor := agent.NewActionExecutor(
		input,
		&fakeCapture{frames: []protocol.Frame{preInput}, latest: &latest},
		fakeWindow{status: protocol.WindowStatus{
			Active: true, Width: 1280, Height: 1024, DPIPercent: 100,
		}},
		&fakeDetector{state: domain.StateMainMenu, confidence: .99},
		latest.Load,
		func() bool { return false },
	)

	result := executor.Execute(
		context.Background(),
		boundRequest(base, domain.StateMainMenu, domain.StateMarketHome),
	)
	if result.Success || len(input.operations) != 0 {
		t.Fatalf("изменившийся экран привёл к вводу: result=%+v operations=%v", result, input.operations)
	}
	if !strings.Contains(result.Error, "содержимое экрана изменилось") {
		t.Fatalf("получена другая ошибка: %q", result.Error)
	}
}

func TestActionExecutorAcceptsUnrelatedAnimationOutsideROIBasis(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	base := runtimePixelFrame(t, 60, now.Add(-3*time.Millisecond), -1, color.NRGBA{})
	preInput := runtimePixelFrame(
		t,
		61,
		now.Add(-2*time.Millisecond),
		3,
		color.NRGBA{R: 250, A: 255},
	)
	control := runtimePixelFrame(
		t,
		62,
		now.Add(-time.Millisecond),
		3,
		color.NRGBA{G: 250, A: 255},
	)
	region := domain.Rectangle{Width: .5, Height: 1}
	basis, err := protocol.BuildFrameRegionBasis(base, []domain.Rectangle{region})
	if err != nil {
		t.Fatalf("BuildFrameRegionBasis() error = %v", err)
	}
	var latest atomic.Uint64
	latest.Store(base.ID)
	input := &fakeInput{}
	executor := agent.NewActionExecutor(
		input,
		&fakeCapture{frames: []protocol.Frame{preInput, control}, latest: &latest},
		fakeWindow{status: protocol.WindowStatus{
			Active: true, Width: 1280, Height: 1024, DPIPercent: 100,
		}},
		&fakeDetector{responses: []detectionResponse{
			{state: domain.StateMainMenu, confidence: .99},
			{state: domain.StateMarketHome, confidence: .99},
		}},
		latest.Load,
		func() bool { return false },
	)
	request := boundRequest(base, domain.StateMainMenu, domain.StateMarketHome)
	request.FrameBasis = basis

	result := executor.Execute(context.Background(), request)
	if !result.Success || input.clicks != 1 {
		t.Fatalf(
			"анимация вне ROI ошибочно заблокировала ввод: result=%+v clicks=%d",
			result,
			input.clicks,
		)
	}
}

func TestActionExecutorRejectsChangedPixelInsideROIBasis(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	base := runtimePixelFrame(t, 70, now.Add(-2*time.Millisecond), -1, color.NRGBA{})
	preInput := runtimePixelFrame(
		t,
		71,
		now.Add(-time.Millisecond),
		1,
		color.NRGBA{R: 250, A: 255},
	)
	region := domain.Rectangle{Width: .5, Height: 1}
	basis, err := protocol.BuildFrameRegionBasis(base, []domain.Rectangle{region})
	if err != nil {
		t.Fatalf("BuildFrameRegionBasis() error = %v", err)
	}
	var latest atomic.Uint64
	latest.Store(base.ID)
	input := &fakeInput{}
	executor := agent.NewActionExecutor(
		input,
		&fakeCapture{frames: []protocol.Frame{preInput}, latest: &latest},
		fakeWindow{status: protocol.WindowStatus{
			Active: true, Width: 1280, Height: 1024, DPIPercent: 100,
		}},
		&fakeDetector{state: domain.StateMainMenu, confidence: .99},
		latest.Load,
		func() bool { return false },
	)
	request := boundRequest(base, domain.StateMainMenu, domain.StateMarketHome)
	request.FrameBasis = basis

	result := executor.Execute(context.Background(), request)
	if result.Success || len(input.operations) != 0 ||
		!strings.Contains(result.Error, "пиксели области 1 изменились") {
		t.Fatalf("изменение внутри ROI не остановило ввод: %+v", result)
	}
}

func TestActionExecutorRejectsEmptyROIBasisForMonetaryClass(t *testing.T) {
	t.Parallel()

	base := testFrame(80, time.Now().UTC(), []byte("экран"))
	executor := agent.NewActionExecutor(
		&fakeInput{},
		&fakeCapture{},
		fakeWindow{},
		&fakeDetector{},
		func() uint64 { return base.ID },
		func() bool { return false },
	)
	request := boundRequest(base, domain.StatePurchaseDialog, domain.StateConfirmation)
	request.Class = protocol.ActionPurchase

	result := executor.Execute(context.Background(), request)
	if result.Success || !result.RetrySafe ||
		!strings.Contains(result.Error, "требует непустое ROI-основание") {
		t.Fatalf("денежное действие без ROI не было отклонено fail-closed: %+v", result)
	}
}

func TestActionExecutorRejectsMalformedOrExcessiveROIBasis(t *testing.T) {
	t.Parallel()

	base := testFrame(90, time.Now().UTC(), []byte("экран"))
	validDigest := protocol.ComputeFrameDigest([]byte("roi"))
	tests := []struct {
		name  string
		basis []protocol.FrameRegionDigest
	}{
		{
			name: "некорректная область",
			basis: []protocol.FrameRegionDigest{{
				Region: domain.Rectangle{X: .9, Width: .2, Height: .2},
				Digest: validDigest,
			}},
		},
		{
			name:  "слишком много областей",
			basis: make([]protocol.FrameRegionDigest, protocol.MaxFrameBasisRegions+1),
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			input := &fakeInput{}
			executor := agent.NewActionExecutor(
				input,
				&fakeCapture{},
				fakeWindow{},
				&fakeDetector{},
				func() uint64 { return base.ID },
				func() bool { return false },
			)
			request := boundRequest(base, domain.StateMainMenu, domain.StateMarketHome)
			request.FrameBasis = test.basis

			result := executor.Execute(context.Background(), request)
			if result.Success || !result.RetrySafe || len(input.operations) != 0 ||
				!strings.Contains(result.Error, "ROI-основание команды некорректно") {
				t.Fatalf("некорректное ROI-основание принято: %+v", result)
			}
		})
	}
}

func TestActionExecutorRejectsUnexpectedSourceStateAndThreshold(t *testing.T) {
	tests := []struct {
		name       string
		state      domain.ScreenState
		confidence float64
		threshold  float64
		want       string
	}{
		{
			name:       "изменилось состояние",
			state:      domain.StateInventory,
			confidence: .99,
			threshold:  .9,
			want:       "перед вводом ожидался экран",
		},
		{
			name:       "порог маршрута выше уверенности",
			state:      domain.StateMainMenu,
			confidence: .91,
			threshold:  .95,
			want:       "недостаточная уверенность перед вводом",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			input := &fakeInput{}
			base := testFrame(20, time.Now().UTC().Add(-2*time.Millisecond), []byte("экран"))
			preInput := testFrame(21, base.CapturedAt.Add(time.Millisecond), base.Data)
			var latest atomic.Uint64
			latest.Store(base.ID)
			executor := agent.NewActionExecutor(
				input,
				&fakeCapture{frames: []protocol.Frame{preInput}, latest: &latest},
				fakeWindow{status: protocol.WindowStatus{
					Active: true, Width: 1280, Height: 1024, DPIPercent: 100,
				}},
				&fakeDetector{state: test.state, confidence: test.confidence},
				latest.Load,
				func() bool { return false },
			)
			request := boundRequest(base, domain.StateMainMenu, domain.StateMarketHome)
			request.MinVerificationConfidence = test.threshold

			result := executor.Execute(context.Background(), request)
			if result.Success || len(input.operations) != 0 {
				t.Fatalf("небезопасное состояние привело к вводу: result=%+v operations=%v", result, input.operations)
			}
			if !strings.Contains(result.Error, test.want) {
				t.Fatalf("получена другая ошибка: %q", result.Error)
			}
		})
	}
}

func TestActionExecutorRechecksGeometryImmediatelyBeforeInput(t *testing.T) {
	input := &fakeInput{}
	base := testFrame(30, time.Now().UTC().Add(-2*time.Millisecond), []byte("экран"))
	preInput := testFrame(31, base.CapturedAt.Add(time.Millisecond), base.Data)
	var latest atomic.Uint64
	latest.Store(base.ID)
	executor := agent.NewActionExecutor(
		input,
		&fakeCapture{frames: []protocol.Frame{preInput}, latest: &latest},
		&changingWindow{statuses: []protocol.WindowStatus{
			{Active: true, Width: 1280, Height: 1024, DPIPercent: 100},
			{Active: true, Width: 1920, Height: 1080, DPIPercent: 100},
		}},
		&fakeDetector{state: domain.StateMainMenu, confidence: .99},
		latest.Load,
		func() bool { return false },
	)

	result := executor.Execute(
		context.Background(),
		boundRequest(base, domain.StateMainMenu, domain.StateMarketHome),
	)
	if result.Success || len(input.operations) != 0 {
		t.Fatalf("изменившаяся геометрия привела к вводу: result=%+v operations=%v", result, input.operations)
	}
	if !strings.Contains(result.Error, "ширина клиентской области изменилась") {
		t.Fatalf("получена другая ошибка: %q", result.Error)
	}
}

func equalOperations(got []string, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for index := range want {
		if got[index] != want[index] {
			return false
		}
	}
	return true
}

type fakeInput struct {
	clicks         int
	operations     []string
	afterOperation func(string)
}

type blockingTextInput struct {
	started chan struct{}
}

func (*blockingTextInput) Move(context.Context, domain.Point) error { return nil }
func (*blockingTextInput) Click(context.Context, string) error      { return nil }
func (*blockingTextInput) Scroll(context.Context, int) error        { return nil }
func (*blockingTextInput) Key(context.Context, string) error        { return nil }
func (input *blockingTextInput) Text(ctx context.Context, _ string) error {
	close(input.started)
	<-ctx.Done()
	return ctx.Err()
}

func (input *fakeInput) record(operation string) {
	input.operations = append(input.operations, operation)
	if input.afterOperation != nil {
		input.afterOperation(operation)
	}
}

func (input *fakeInput) Move(context.Context, domain.Point) error {
	input.record("move")
	return nil
}
func (input *fakeInput) Click(context.Context, string) error {
	input.clicks++
	input.record("click")
	return nil
}
func (input *fakeInput) Scroll(context.Context, int) error {
	input.record("scroll")
	return nil
}
func (input *fakeInput) Key(context.Context, string) error {
	input.record("key")
	return nil
}
func (input *fakeInput) Text(context.Context, string) error {
	input.record("text")
	return nil
}

type fakeCapture struct {
	frames       []protocol.Frame
	frame        protocol.Frame
	latest       *atomic.Uint64
	calls        atomic.Int32
	beforeReturn func(int)
}

func (capture *fakeCapture) Capture(context.Context) (protocol.Frame, error) {
	call := int(capture.calls.Add(1))
	if capture.beforeReturn != nil {
		capture.beforeReturn(call)
	}
	frame := capture.frame
	if call <= len(capture.frames) {
		frame = capture.frames[call-1]
	}
	if capture.latest != nil {
		capture.latest.Store(frame.ID)
	}
	return frame, nil
}
func (capture *fakeCapture) CaptureRegion(context.Context, domain.Rectangle) (protocol.Frame, error) {
	return capture.frame, nil
}

type fakeWindow struct {
	status protocol.WindowStatus
}

func (window fakeWindow) Status(context.Context) (protocol.WindowStatus, error) {
	return window.status, nil
}

type changingWindow struct {
	statuses []protocol.WindowStatus
	calls    atomic.Int32
}

func (window *changingWindow) Status(context.Context) (protocol.WindowStatus, error) {
	call := int(window.calls.Add(1))
	if call <= len(window.statuses) {
		return window.statuses[call-1], nil
	}
	return window.statuses[len(window.statuses)-1], nil
}

type fakeDetector struct {
	state      domain.ScreenState
	confidence float64
	responses  []detectionResponse
	calls      atomic.Int32
}

type detectionResponse struct {
	state      domain.ScreenState
	confidence float64
}

func (detector *fakeDetector) Detect(context.Context, protocol.Frame) (domain.ScreenState, float64, error) {
	call := int(detector.calls.Add(1))
	if call <= len(detector.responses) {
		response := detector.responses[call-1]
		return response.state, response.confidence, nil
	}
	return detector.state, detector.confidence, nil
}

func testFrame(id uint64, capturedAt time.Time, data []byte) protocol.Frame {
	return protocol.Frame{
		ID:            id,
		CapturedAt:    capturedAt,
		ContentDigest: protocol.ComputeFrameDigest(data),
		Region:        domain.Rectangle{Width: 1, Height: 1},
		Encoding:      "png",
		Data:          data,
	}
}

func runtimePixelFrame(
	t *testing.T,
	id uint64,
	capturedAt time.Time,
	changedX int,
	changed color.NRGBA,
) protocol.Frame {
	t.Helper()

	pixels := image.NewNRGBA(image.Rect(0, 0, 4, 3))
	for y := 0; y < pixels.Bounds().Dy(); y++ {
		for x := 0; x < pixels.Bounds().Dx(); x++ {
			pixels.SetNRGBA(x, y, color.NRGBA{R: 10, G: 20, B: 30, A: 255})
		}
	}
	if changedX >= 0 {
		pixels.SetNRGBA(changedX, 1, changed)
	}
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, pixels); err != nil {
		t.Fatalf("png.Encode() error = %v", err)
	}
	return testFrame(id, capturedAt, encoded.Bytes())
}

func boundRequest(
	base protocol.Frame,
	basedOnState domain.ScreenState,
	expectedState domain.ScreenState,
) protocol.ActionRequest {
	capturedAt := base.CapturedAt
	return protocol.ActionRequest{
		ID:                        "action-1",
		SessionID:                 "session-1",
		BasedOnFrame:              base.ID,
		BasedOnCapturedAt:         &capturedAt,
		BasedOnFrameDigest:        protocol.ComputeFrameDigest(base.Data),
		BasedOnState:              basedOnState,
		ExpectedState:             expectedState,
		MinVerificationConfidence: .8,
		ExpectedWidth:             1280,
		ExpectedHeight:            1024,
		ExpectedDPIPercent:        100,
		Class:                     protocol.ActionNavigation,
		Deadline:                  time.Now().Add(time.Second),
		Action:                    protocol.Action{Kind: "CLICK", Value: "LEFT"},
	}
}

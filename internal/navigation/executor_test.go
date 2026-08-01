package navigation_test

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/png"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arena-trading-agent/arena-trading-agent/internal/domain"
	"github.com/arena-trading-agent/arena-trading-agent/internal/navigation"
	"github.com/arena-trading-agent/arena-trading-agent/internal/protocol"
)

var fixedNow = time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)

type actionReply struct {
	result protocol.ActionResult
	err    error
}

type fakeActionClient struct {
	mu        sync.Mutex
	requests  []protocol.ActionRequest
	agentIDs  []string
	replies   []actionReply
	inFlight  atomic.Int32
	maxFlight atomic.Int32
	block     <-chan struct{}
	entered   chan<- struct{}
}

func (f *fakeActionClient) RequestAction(
	ctx context.Context,
	agentID string,
	request protocol.ActionRequest,
) (protocol.ActionResult, error) {
	current := f.inFlight.Add(1)
	defer f.inFlight.Add(-1)
	for {
		maximum := f.maxFlight.Load()
		if current <= maximum || f.maxFlight.CompareAndSwap(maximum, current) {
			break
		}
	}

	f.mu.Lock()
	index := len(f.requests)
	f.requests = append(f.requests, request)
	f.agentIDs = append(f.agentIDs, agentID)
	var reply actionReply
	if index < len(f.replies) {
		reply = f.replies[index]
	}
	f.mu.Unlock()

	if f.entered != nil {
		select {
		case f.entered <- struct{}{}:
		default:
		}
	}
	if f.block != nil {
		select {
		case <-ctx.Done():
			return protocol.ActionResult{}, ctx.Err()
		case <-f.block:
		}
	}
	return reply.result, reply.err
}

func (f *fakeActionClient) snapshot() []protocol.ActionRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]protocol.ActionRequest(nil), f.requests...)
}

type observed struct {
	frame       protocol.Frame
	observation domain.Observation
	err         error
}

type fakeObservationSource struct {
	mu          sync.Mutex
	values      []observed
	afterFrames []uint64
	agentIDs    []string
}

func (f *fakeObservationSource) ObserveAfter(
	_ context.Context,
	agentID string,
	afterFrame uint64,
) (protocol.Frame, domain.Observation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	index := len(f.afterFrames)
	f.afterFrames = append(f.afterFrames, afterFrame)
	f.agentIDs = append(f.agentIDs, agentID)
	if index >= len(f.values) {
		return protocol.Frame{}, domain.Observation{}, errors.New("unexpected ObserveAfter")
	}
	value := f.values[index]
	return value.frame, value.observation, value.err
}

func (f *fakeObservationSource) snapshot() ([]uint64, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]uint64(nil), f.afterFrames...), append([]string(nil), f.agentIDs...)
}

func TestExecutorBuildsSafeRequestAndVerifiesFreshObservation(t *testing.T) {
	t.Parallel()

	client := &fakeActionClient{replies: []actionReply{{result: actionResult(
		"session-a-action-1", 11, domain.StateMarketHome, true, "",
	)}}}
	source := &fakeObservationSource{values: []observed{
		snapshot(12, domain.StateMarketHome, .97, fixedNow),
	}}
	executor := newExecutor(t, client, source, navigation.Config{
		ActionTimeout: 1500 * time.Millisecond,
		NewActionID: func(_ string, _, _ int) string {
			return "session-a-action-1"
		},
	})
	click := domain.Point{X: .25, Y: .75}
	request := baseRequest(navigation.Path{{
		From: domain.StateMainMenu,
		To:   domain.StateMarketHome,
		Action: protocol.Action{
			Kind: "CLICK", Point: &click,
		},
		Verify: navigation.VerificationRule{
			State: domain.StateMarketHome, MinConfidence: .9,
		},
	}}, 10, domain.StateMainMenu, .96)

	result, err := executor.Execute(context.Background(), request)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if result.CompletedTransitions != 1 || result.Frame.ID != 12 ||
		result.Observation.State != domain.StateMarketHome || len(result.Attempts) != 1 {
		t.Fatalf("Execute() result = %#v", result)
	}
	requests := client.snapshot()
	if len(requests) != 1 {
		t.Fatalf("action calls = %d, want 1", len(requests))
	}
	got := requests[0]
	if got.ID != "session-a-action-1" || got.SessionID != "session-a" ||
		got.BasedOnFrame != 10 || got.ExpectedState != domain.StateMarketHome {
		t.Fatalf("unsafe action identity/preconditions: %#v", got)
	}
	if got.BasedOnCapturedAt == nil || !got.BasedOnCapturedAt.Equal(fixedNow) ||
		got.BasedOnFrameDigest != protocol.ComputeFrameDigest([]byte{1}) ||
		got.BasedOnState != domain.StateMainMenu {
		t.Fatalf("action is not bound to exact source frame/state: %#v", got)
	}
	if got.MinVerificationConfidence != .9 {
		t.Fatalf("min verification confidence = %v, want .9", got.MinVerificationConfidence)
	}
	if got.ExpectedWidth != 1280 || got.ExpectedHeight != 1024 || got.ExpectedDPIPercent != 100 {
		t.Fatalf("action geometry = %#v", got)
	}
	if want := fixedNow.Add(1500 * time.Millisecond); !got.Deadline.Equal(want) {
		t.Fatalf("deadline = %s, want %s", got.Deadline, want)
	}
	afterFrames, agentIDs := source.snapshot()
	if len(afterFrames) != 1 || afterFrames[0] != 11 || agentIDs[0] != "windows-1" {
		t.Fatalf("ObserveAfter calls = %v, agents = %v", afterFrames, agentIDs)
	}
	if client.maxFlight.Load() != 1 {
		t.Fatalf("max simultaneous RequestAction = %d, want 1", client.maxFlight.Load())
	}
}

func TestExecutorBuildsCanonicalROIBasisFromSourceObservation(t *testing.T) {
	t.Parallel()

	sourceFrame := pixelFrame(t, 15, fixedNow)
	firstRegion := domain.Rectangle{X: 0, Y: 0, Width: .5, Height: .5}
	secondRegion := domain.Rectangle{X: .5, Y: .5, Width: .5, Height: .5}
	sourceObservation := observation(
		sourceFrame.ID,
		domain.StateMainMenu,
		.99,
		fixedNow,
	)
	sourceObservation.Values = map[string]domain.Value{
		"price": {Region: firstRegion},
	}
	sourceObservation.Elements = []domain.UIElement{{Region: secondRegion}}
	client := &fakeActionClient{replies: []actionReply{{err: errors.New("остановка после проверки запроса")}}}
	executor := newExecutor(t, client, &fakeObservationSource{}, navigation.Config{
		NewActionID: func(string, int, int) string { return "roi-action" },
	})
	click := domain.Point{X: .75, Y: .75}
	request := baseRequest(
		navigation.Path{{
			From: domain.StateMainMenu,
			To:   domain.StateMarketHome,
			Action: protocol.Action{
				Kind: "CLICK", Point: &click, Value: "LEFT",
			},
			Verify: navigation.VerificationRule{
				State: domain.StateMarketHome, MinConfidence: .9,
			},
		}},
		sourceFrame.ID,
		domain.StateMainMenu,
		.99,
	)
	request.Frame = sourceFrame
	request.Observation = sourceObservation

	_, err := executor.Execute(context.Background(), request)
	if err == nil || !stringsContain(err.Error(), "остановка после проверки запроса") {
		t.Fatalf("Execute() error = %v", err)
	}
	requests := client.snapshot()
	if len(requests) != 1 || len(requests[0].FrameBasis) != 2 {
		t.Fatalf("ROI-основание не сформировано: %+v", requests)
	}
	if err := protocol.ValidateFrameRegionBasis(requests[0].FrameBasis); err != nil {
		t.Fatalf("ROI-основание неканонично: %v", err)
	}
	if err := protocol.VerifyFrameRegionBasis(sourceFrame, requests[0].FrameBasis); err != nil {
		t.Fatalf("ROI-основание не связано с исходным кадром: %v", err)
	}
}

func TestExecutorRejectsMonetaryActionWithoutROIBasis(t *testing.T) {
	t.Parallel()

	client := &fakeActionClient{}
	executor := newExecutor(t, client, &fakeObservationSource{}, navigation.Config{})
	point := domain.Point{X: .5, Y: .5}
	path := navigation.Path{{
		From:  domain.StatePurchaseDialog,
		To:    domain.StateConfirmation,
		Class: protocol.ActionPurchase,
		Action: protocol.Action{
			Kind: "CLICK", Point: &point, Value: "LEFT",
		},
		Verify: navigation.VerificationRule{
			State: domain.StateConfirmation, MinConfidence: .9,
		},
	}}
	request := baseRequest(path, 16, domain.StatePurchaseDialog, .99)

	_, err := executor.Execute(context.Background(), request)
	if !errors.Is(err, navigation.ErrInvalidRequest) ||
		!stringsContain(err.Error(), "требует непустое ROI-основание") {
		t.Fatalf("денежное действие без ROI вернуло неверную ошибку: %v", err)
	}
	if calls := len(client.snapshot()); calls != 0 {
		t.Fatalf("денежное действие без ROI было отправлено: %d", calls)
	}
}

func TestExecutorRunsMultiStepPathStrictlySequentially(t *testing.T) {
	t.Parallel()

	client := &fakeActionClient{replies: []actionReply{
		{result: actionResult("id-0-0", 101, domain.StateContacts, true, "")},
		{result: actionResult("id-1-0", 103, domain.StateContactPage, true, "")},
	}}
	source := &fakeObservationSource{values: []observed{
		snapshot(102, domain.StateContacts, .94, fixedNow),
		snapshot(104, domain.StateContactPage, .95, fixedNow),
	}}
	executor := newExecutor(t, client, source, navigation.Config{
		NewActionID: func(_ string, step, attempt int) string {
			return "id-" + string(rune('0'+step)) + "-" + string(rune('0'+attempt))
		},
	})
	path := navigation.Path{
		transition(domain.StateMainMenu, domain.StateContacts, 0),
		transition(domain.StateContacts, domain.StateContactPage, 0),
	}
	request := baseRequest(path, 100, domain.StateMainMenu, .99)

	result, err := executor.Execute(context.Background(), request)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if result.CompletedTransitions != 2 || len(result.Attempts) != 2 {
		t.Fatalf("result = %#v", result)
	}
	requests := client.snapshot()
	if requests[0].BasedOnFrame != 100 || requests[1].BasedOnFrame != 102 {
		t.Fatalf("based-on frames = %d, %d", requests[0].BasedOnFrame, requests[1].BasedOnFrame)
	}
	if requests[0].ExpectedState != domain.StateContacts ||
		requests[1].ExpectedState != domain.StateContactPage {
		t.Fatalf("expected states = %s, %s", requests[0].ExpectedState, requests[1].ExpectedState)
	}
	if client.maxFlight.Load() != 1 {
		t.Fatalf("max simultaneous actions = %d", client.maxFlight.Load())
	}
}

func TestExecutorRetriesOnlyConclusiveUnchangedState(t *testing.T) {
	t.Parallel()

	client := &fakeActionClient{replies: []actionReply{
		{result: actionResult("retry-0", 21, domain.StateMainMenu, false, "transition not observed")},
		{result: actionResult("retry-1", 23, domain.StateMarketHome, true, "")},
	}}
	source := &fakeObservationSource{values: []observed{
		snapshot(22, domain.StateMainMenu, .96, fixedNow),
		snapshot(24, domain.StateMarketHome, .97, fixedNow),
	}}
	executor := newExecutor(t, client, source, navigation.Config{
		NewActionID: func(_ string, _, attempt int) string {
			if attempt == 0 {
				return "retry-0"
			}
			return "retry-1"
		},
	})
	request := baseRequest(navigation.Path{
		transition(domain.StateMainMenu, domain.StateMarketHome, 1),
	}, 20, domain.StateMainMenu, .98)

	result, err := executor.Execute(context.Background(), request)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if result.CompletedTransitions != 1 || len(result.Attempts) != 2 {
		t.Fatalf("result = %#v", result)
	}
	requests := client.snapshot()
	if len(requests) != 2 || requests[0].BasedOnFrame != 20 || requests[1].BasedOnFrame != 22 {
		t.Fatalf("requests = %#v", requests)
	}
	afterFrames, _ := source.snapshot()
	if len(afterFrames) != 2 || afterFrames[0] != 21 || afterFrames[1] != 23 {
		t.Fatalf("after frames = %v", afterFrames)
	}
}

func TestExecutorDoesNotRetryAmbiguousTransportFailure(t *testing.T) {
	t.Parallel()

	client := &fakeActionClient{replies: []actionReply{{err: errors.New("connection reset")}}}
	source := &fakeObservationSource{}
	executor := newExecutor(t, client, source, navigation.Config{})
	request := baseRequest(navigation.Path{
		transition(domain.StateMainMenu, domain.StateMarketHome, 3),
	}, 30, domain.StateMainMenu, .99)

	_, err := executor.Execute(context.Background(), request)
	if err == nil || !stringsContain(err.Error(), "connection reset") {
		t.Fatalf("Execute() error = %v", err)
	}
	if calls := len(client.snapshot()); calls != 1 {
		t.Fatalf("action calls = %d, want 1", calls)
	}
	afterFrames, _ := source.snapshot()
	if len(afterFrames) != 0 {
		t.Fatalf("ObserveAfter called after ambiguous failure: %v", afterFrames)
	}
}

func TestExecutorDoesNotRetryAfterInputWasAttempted(t *testing.T) {
	t.Parallel()

	failed := actionResult(
		"input-attempted",
		31,
		domain.StateMainMenu,
		false,
		"SendInput вернул частичный результат",
	)
	failed.RetrySafe = false
	client := &fakeActionClient{replies: []actionReply{{result: failed}}}
	source := &fakeObservationSource{values: []observed{
		snapshot(32, domain.StateMainMenu, .99, fixedNow),
	}}
	executor := newExecutor(t, client, source, navigation.Config{
		NewActionID: func(string, int, int) string { return "input-attempted" },
	})
	request := baseRequest(navigation.Path{
		transition(domain.StateMainMenu, domain.StateMarketHome, 3),
	}, 30, domain.StateMainMenu, .99)

	_, err := executor.Execute(context.Background(), request)
	if !errors.Is(err, navigation.ErrActionRejected) {
		t.Fatalf("частичный ввод вернул неверную ошибку: %v", err)
	}
	if calls := len(client.snapshot()); calls != 1 {
		t.Fatalf("частичный ввод был повторён: calls=%d", calls)
	}
}

func TestExecutorRejectsUnsupportedVerificationBBoxBeforeInput(t *testing.T) {
	t.Parallel()

	client := &fakeActionClient{}
	source := &fakeObservationSource{}
	executor := newExecutor(t, client, source, navigation.Config{})
	path := navigation.Path{
		transition(domain.StateMainMenu, domain.StateMarketHome, 0),
	}
	path[0].Verify.BBox = &domain.Rectangle{X: .1, Y: .2, Width: .3, Height: .4}
	request := baseRequest(path, 33, domain.StateMainMenu, .99)

	_, err := executor.Execute(context.Background(), request)
	if !errors.Is(err, navigation.ErrInvalidRequest) ||
		!stringsContain(err.Error(), "verification bbox должна быть пустой") {
		t.Fatalf("неподдерживаемая область проверки вернула неверную ошибку: %v", err)
	}
	if calls := len(client.snapshot()); calls != 0 {
		t.Fatalf("ввод был отправлен для неподдерживаемой области проверки: calls=%d", calls)
	}
}

func TestExecutorStopsBeforeInputOnUnsafeInitialSnapshot(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		frame       protocol.Frame
		observation domain.Observation
		target      error
	}{
		{
			name:        "unknown",
			frame:       frame(40, fixedNow),
			observation: observation(40, domain.StateUnknown, .99, fixedNow),
			target:      navigation.ErrUnknownState,
		},
		{
			name:        "low confidence",
			frame:       frame(40, fixedNow),
			observation: observation(40, domain.StateMainMenu, .79, fixedNow),
			target:      navigation.ErrLowConfidence,
		},
		{
			name:        "stale timestamps",
			frame:       frame(40, fixedNow.Add(-13*time.Second)),
			observation: observation(40, domain.StateMainMenu, .99, fixedNow.Add(-13*time.Second)),
			target:      navigation.ErrStaleObservation,
		},
		{
			name:        "mismatched frame",
			frame:       frame(40, fixedNow),
			observation: observation(39, domain.StateMainMenu, .99, fixedNow),
			target:      navigation.ErrStaleObservation,
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			client := &fakeActionClient{}
			source := &fakeObservationSource{}
			executor := newExecutor(t, client, source, navigation.Config{})
			request := baseRequest(navigation.Path{
				transition(domain.StateMainMenu, domain.StateMarketHome, 0),
			}, 40, domain.StateMainMenu, .99)
			request.Frame, request.Observation = test.frame, test.observation

			_, err := executor.Execute(context.Background(), request)
			if !errors.Is(err, test.target) {
				t.Fatalf("Execute() error = %v, want %v", err, test.target)
			}
			if calls := len(client.snapshot()); calls != 0 {
				t.Fatalf("action calls = %d, want 0", calls)
			}
		})
	}
}

func TestExecutorStopsOnUnsafePostActionObservation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		value  observed
		target error
	}{
		{
			name:   "unknown",
			value:  snapshot(52, domain.StateUnknown, .99, fixedNow),
			target: navigation.ErrUnknownState,
		},
		{
			name:   "low confidence",
			value:  snapshot(52, domain.StateMarketHome, .80, fixedNow),
			target: navigation.ErrLowConfidence,
		},
		{
			name:   "replayed frame",
			value:  snapshot(51, domain.StateMarketHome, .99, fixedNow),
			target: navigation.ErrStaleObservation,
		},
		{
			name: "stale observation",
			value: snapshot(
				52, domain.StateMarketHome, .99, fixedNow.Add(-13*time.Second),
			),
			target: navigation.ErrStaleObservation,
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			client := &fakeActionClient{replies: []actionReply{{result: actionResult(
				"action", 51, domain.StateMarketHome, true, "",
			)}}}
			source := &fakeObservationSource{values: []observed{test.value}}
			executor := newExecutor(t, client, source, navigation.Config{
				NewActionID: func(string, int, int) string { return "action" },
			})
			request := baseRequest(navigation.Path{
				{
					From: domain.StateMainMenu, To: domain.StateMarketHome,
					Action: protocol.Action{Kind: "KEY", Value: "M"},
					Verify: navigation.VerificationRule{
						State: domain.StateMarketHome, MinConfidence: .90,
					},
					MaxRetry: 2,
				},
			}, 50, domain.StateMainMenu, .99)

			_, err := executor.Execute(context.Background(), request)
			if !errors.Is(err, test.target) {
				t.Fatalf("Execute() error = %v, want %v", err, test.target)
			}
			if calls := len(client.snapshot()); calls != 1 {
				t.Fatalf("action calls = %d, want 1", calls)
			}
		})
	}
}

func TestExecutorStopsOnUnexpectedStateWithoutRetry(t *testing.T) {
	t.Parallel()

	client := &fakeActionClient{replies: []actionReply{{result: actionResult(
		"action", 61, domain.StateMarketHome, true, "",
	)}}}
	source := &fakeObservationSource{values: []observed{
		snapshot(62, domain.StateErrorDialog, .99, fixedNow),
	}}
	executor := newExecutor(t, client, source, navigation.Config{
		NewActionID: func(string, int, int) string { return "action" },
	})
	request := baseRequest(navigation.Path{
		transition(domain.StateMainMenu, domain.StateMarketHome, 3),
	}, 60, domain.StateMainMenu, .99)

	_, err := executor.Execute(context.Background(), request)
	if !errors.Is(err, navigation.ErrStateMismatch) {
		t.Fatalf("Execute() error = %v", err)
	}
	if calls := len(client.snapshot()); calls != 1 {
		t.Fatalf("action calls = %d, want 1", calls)
	}
}

func TestExecutorValidatesActionResultBeforeRefresh(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		result protocol.ActionResult
	}{
		{
			name:   "wrong id",
			result: actionResult("other", 71, domain.StateMarketHome, true, ""),
		},
		{
			name: "missing completed at",
			result: protocol.ActionResult{
				ID: "action", Success: true, ResultFrame: 71, ResultState: domain.StateMarketHome,
			},
		},
		{
			name:   "replayed result frame",
			result: actionResult("action", 70, domain.StateMarketHome, true, ""),
		},
		{
			name:   "wrong result state",
			result: actionResult("action", 71, domain.StateContacts, true, ""),
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			client := &fakeActionClient{replies: []actionReply{{result: test.result}}}
			source := &fakeObservationSource{}
			executor := newExecutor(t, client, source, navigation.Config{
				NewActionID: func(string, int, int) string { return "action" },
			})
			request := baseRequest(navigation.Path{
				transition(domain.StateMainMenu, domain.StateMarketHome, 0),
			}, 70, domain.StateMainMenu, .99)

			_, err := executor.Execute(context.Background(), request)
			if !errors.Is(err, navigation.ErrInvalidActionResult) {
				t.Fatalf("Execute() error = %v", err)
			}
			afterFrames, _ := source.snapshot()
			if len(afterFrames) != 0 {
				t.Fatalf("ObserveAfter calls = %v, want none", afterFrames)
			}
		})
	}
}

func TestExecutorHonorsContextBeforeAndDuringAction(t *testing.T) {
	t.Parallel()

	t.Run("before", func(t *testing.T) {
		client := &fakeActionClient{}
		executor := newExecutor(t, client, &fakeObservationSource{}, navigation.Config{})
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := executor.Execute(ctx, baseRequest(navigation.Path{
			transition(domain.StateMainMenu, domain.StateMarketHome, 0),
		}, 80, domain.StateMainMenu, .99))
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Execute() error = %v", err)
		}
		if calls := len(client.snapshot()); calls != 0 {
			t.Fatalf("action calls = %d", calls)
		}
	})

	t.Run("during", func(t *testing.T) {
		block := make(chan struct{})
		entered := make(chan struct{}, 1)
		client := &fakeActionClient{block: block, entered: entered}
		executor := newExecutor(t, client, &fakeObservationSource{}, navigation.Config{})
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() {
			_, err := executor.Execute(ctx, baseRequest(navigation.Path{
				transition(domain.StateMainMenu, domain.StateMarketHome, 0),
			}, 80, domain.StateMainMenu, .99))
			done <- err
		}()
		<-entered
		cancel()
		err := <-done
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Execute() error = %v", err)
		}
	})
}

func TestExecutorCapsActionDeadlineByContext(t *testing.T) {
	t.Parallel()

	client := &fakeActionClient{replies: []actionReply{{result: actionResult(
		"action", 91, domain.StateMarketHome, true, "",
	)}}}
	source := &fakeObservationSource{values: []observed{
		snapshot(92, domain.StateMarketHome, .99, fixedNow),
	}}
	executor := newExecutor(t, client, source, navigation.Config{
		ActionTimeout: 10 * time.Second,
		NewActionID:   func(string, int, int) string { return "action" },
	})
	deadline := fixedNow.Add(time.Second)
	ctx := fixedDeadlineContext{Context: context.Background(), deadline: deadline}
	_, err := executor.Execute(ctx, baseRequest(navigation.Path{
		transition(domain.StateMainMenu, domain.StateMarketHome, 0),
	}, 90, domain.StateMainMenu, .99))
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	requests := client.snapshot()
	if !requests[0].Deadline.Equal(deadline) {
		t.Fatalf("deadline = %s, want %s", requests[0].Deadline, deadline)
	}
}

func TestExecutorUsesTransitionVerificationTimeout(t *testing.T) {
	t.Parallel()

	client := &fakeActionClient{replies: []actionReply{{result: actionResult(
		"action", 96, domain.StateMarketHome, true, "",
	)}}}
	source := &fakeObservationSource{values: []observed{
		snapshot(97, domain.StateMarketHome, .99, fixedNow),
	}}
	executor := newExecutor(t, client, source, navigation.Config{
		ActionTimeout: 10 * time.Second,
		NewActionID:   func(string, int, int) string { return "action" },
	})
	path := navigation.Path{
		transition(domain.StateMainMenu, domain.StateMarketHome, 0),
	}
	path[0].Verify.Timeout = 750 * time.Millisecond
	_, err := executor.Execute(
		context.Background(),
		baseRequest(path, 95, domain.StateMainMenu, .99),
	)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	requests := client.snapshot()
	if len(requests) != 1 {
		t.Fatalf("action requests = %d, want 1", len(requests))
	}
	want := fixedNow.Add(750 * time.Millisecond)
	if !requests[0].Deadline.Equal(want) {
		t.Fatalf("deadline = %s, want %s", requests[0].Deadline, want)
	}
}

func TestExecutorRejectsConcurrentRouteInsteadOfQueueing(t *testing.T) {
	t.Parallel()

	block := make(chan struct{})
	entered := make(chan struct{}, 1)
	client := &fakeActionClient{
		block:   block,
		entered: entered,
		replies: []actionReply{{result: actionResult(
			"first", 101, domain.StateMarketHome, true, "",
		)}},
	}
	source := &fakeObservationSource{values: []observed{
		snapshot(102, domain.StateMarketHome, .99, fixedNow),
	}}
	executor := newExecutor(t, client, source, navigation.Config{
		NewActionID: func(string, int, int) string { return "first" },
	})
	request := baseRequest(navigation.Path{
		transition(domain.StateMainMenu, domain.StateMarketHome, 0),
	}, 100, domain.StateMainMenu, .99)
	firstDone := make(chan error, 1)
	go func() {
		_, err := executor.Execute(context.Background(), request)
		firstDone <- err
	}()
	<-entered

	_, err := executor.Execute(context.Background(), request)
	if !errors.Is(err, navigation.ErrBusy) {
		t.Fatalf("concurrent Execute() error = %v", err)
	}
	close(block)
	if err := <-firstDone; err != nil {
		t.Fatalf("first Execute() error = %v", err)
	}
	if calls := len(client.snapshot()); calls != 1 {
		t.Fatalf("action calls = %d, want 1", calls)
	}
}

func TestExecutorRejectsInvalidDeclarativePathBeforeInput(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		path navigation.Path
	}{
		{
			name: "discontinuous",
			path: navigation.Path{
				transition(domain.StateMainMenu, domain.StateContacts, 0),
				transition(domain.StateMarketHome, domain.StateItemCard, 0),
			},
		},
		{
			name: "unknown",
			path: navigation.Path{
				transition(domain.StateUnknown, domain.StateContacts, 0),
			},
		},
		{
			name: "unbounded retry",
			path: navigation.Path{
				transition(domain.StateMainMenu, domain.StateContacts, 4),
			},
		},
		{
			name: "verify wrong target",
			path: navigation.Path{{
				From: domain.StateMainMenu, To: domain.StateContacts,
				Action: protocol.Action{Kind: "KEY", Value: "C"},
				Verify: navigation.VerificationRule{State: domain.StateMarketHome},
			}},
		},
		{
			name: "invalid action",
			path: navigation.Path{{
				From: domain.StateMainMenu, To: domain.StateContacts,
				Action: protocol.Action{Kind: "CLICK", Value: "LASER"},
				Verify: navigation.VerificationRule{State: domain.StateContacts},
			}},
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			client := &fakeActionClient{}
			executor := newExecutor(t, client, &fakeObservationSource{}, navigation.Config{})
			initialState := domain.StateMainMenu
			if len(test.path) > 0 {
				initialState = test.path[0].From
			}
			request := baseRequest(test.path, 110, initialState, .99)
			_, err := executor.Execute(context.Background(), request)
			if !errors.Is(err, navigation.ErrInvalidRequest) {
				t.Fatalf("Execute() error = %v", err)
			}
			if calls := len(client.snapshot()); calls != 0 {
				t.Fatalf("action calls = %d", calls)
			}
		})
	}
}

func TestExecutorStopsAfterRetryBudget(t *testing.T) {
	t.Parallel()

	client := &fakeActionClient{replies: []actionReply{
		{result: actionResult("attempt-0", 121, domain.StateMainMenu, false, "no transition")},
		{result: actionResult("attempt-1", 123, domain.StateMainMenu, false, "no transition")},
	}}
	source := &fakeObservationSource{values: []observed{
		snapshot(122, domain.StateMainMenu, .99, fixedNow),
		snapshot(124, domain.StateMainMenu, .99, fixedNow),
	}}
	executor := newExecutor(t, client, source, navigation.Config{
		NewActionID: func(_ string, _, attempt int) string {
			if attempt == 0 {
				return "attempt-0"
			}
			return "attempt-1"
		},
	})
	request := baseRequest(navigation.Path{
		transition(domain.StateMainMenu, domain.StateMarketHome, 1),
	}, 120, domain.StateMainMenu, .99)

	result, err := executor.Execute(context.Background(), request)
	if !errors.Is(err, navigation.ErrActionRejected) {
		t.Fatalf("Execute() error = %v", err)
	}
	if len(result.Attempts) != 2 || len(client.snapshot()) != 2 {
		t.Fatalf("attempts = %d, calls = %d", len(result.Attempts), len(client.snapshot()))
	}
}

func newExecutor(
	t *testing.T,
	client navigation.ActionClient,
	source navigation.ObservationSource,
	config navigation.Config,
) *navigation.Executor {
	t.Helper()
	if config.Now == nil {
		config.Now = func() time.Time { return fixedNow }
	}
	executor, err := navigation.NewExecutor(client, source, config)
	if err != nil {
		t.Fatalf("NewExecutor() error = %v", err)
	}
	return executor
}

func baseRequest(
	path navigation.Path,
	frameID uint64,
	state domain.ScreenState,
	confidence float64,
) navigation.Request {
	return navigation.Request{
		AgentID:     "windows-1",
		SessionID:   "session-a",
		Path:        path,
		Frame:       frame(frameID, fixedNow),
		Observation: observation(frameID, state, confidence, fixedNow),
		Geometry: navigation.Geometry{
			Width: 1280, Height: 1024, DPIPercent: 100,
		},
	}
}

func transition(from, to domain.ScreenState, maxRetry int) navigation.Transition {
	return navigation.Transition{
		From: from,
		To:   to,
		Action: protocol.Action{
			Kind: "KEY", Value: "ENTER",
		},
		Verify: navigation.VerificationRule{
			State: to, MinConfidence: .90,
		},
		MaxRetry: maxRetry,
	}
}

func snapshot(
	frameID uint64,
	state domain.ScreenState,
	confidence float64,
	at time.Time,
) observed {
	return observed{
		frame:       frame(frameID, at),
		observation: observation(frameID, state, confidence, at),
	}
}

func frame(id uint64, at time.Time) protocol.Frame {
	return protocol.Frame{ID: id, CapturedAt: at, Encoding: "image/png", Data: []byte{1}}
}

func pixelFrame(t *testing.T, id uint64, at time.Time) protocol.Frame {
	t.Helper()

	pixels := image.NewNRGBA(image.Rect(0, 0, 4, 4))
	for y := 0; y < 4; y++ {
		for x := 0; x < 4; x++ {
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
	data := encoded.Bytes()
	return protocol.Frame{
		ID:            id,
		CapturedAt:    at,
		ContentDigest: protocol.ComputeFrameDigest(data),
		Region:        domain.Rectangle{Width: 1, Height: 1},
		Encoding:      "png",
		Data:          data,
	}
}

func observation(
	frameID uint64,
	state domain.ScreenState,
	confidence float64,
	at time.Time,
) domain.Observation {
	return domain.Observation{
		FrameID: frameID, State: state, Confidence: confidence, CreatedAt: at,
	}
}

func actionResult(
	id string,
	frameID uint64,
	state domain.ScreenState,
	success bool,
	message string,
) protocol.ActionResult {
	return protocol.ActionResult{
		ID: id, Success: success, ResultFrame: frameID, ResultState: state,
		RetrySafe:              !success,
		VerificationConfidence: .95,
		Frame:                  ptr(frame(frameID, fixedNow.Add(250*time.Millisecond))),
		Error:                  message,
		CompletedAt:            fixedNow.Add(500 * time.Millisecond),
	}
}

func ptr[T any](value T) *T {
	return &value
}

func stringsContain(value, substring string) bool {
	for index := 0; index+len(substring) <= len(value); index++ {
		if value[index:index+len(substring)] == substring {
			return true
		}
	}
	return false
}

type fixedDeadlineContext struct {
	context.Context
	deadline time.Time
}

func (c fixedDeadlineContext) Deadline() (time.Time, bool) {
	return c.deadline, true
}

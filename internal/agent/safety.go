package agent

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/arena-trading-agent/arena-trading-agent/internal/protocol"
)

// SafetySupervisor owns the local stop/pause state. Emergency stop is sticky
// for the lifetime of the process; resuming after it requires a deliberate
// restart and therefore a new preflight.
type SafetySupervisor struct {
	stopped atomic.Bool
	paused  atomic.Bool

	mu              sync.Mutex
	reason          string
	consecutiveFail int
	maxFailures     int
	client          *Client
}

// NewSafetySupervisor creates a supervisor with the required three-error
// emergency threshold.
func NewSafetySupervisor(maxFailures int) *SafetySupervisor {
	if maxFailures <= 0 {
		maxFailures = 3
	}
	return &SafetySupervisor{maxFailures: maxFailures}
}

// AttachTransport enables ordered delivery of safety events.
func (s *SafetySupervisor) AttachTransport(client *Client) {
	s.mu.Lock()
	s.client = client
	s.mu.Unlock()
}

// Stopped reports sticky emergency state.
func (s *SafetySupervisor) Stopped() bool { return s.stopped.Load() }

// Paused reports that no input may be executed.
func (s *SafetySupervisor) Paused() bool { return s.paused.Load() || s.Stopped() }

// Reason returns the current human-readable safety reason.
func (s *SafetySupervisor) Reason() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reason
}

// Pause stops subsequent actions without making the condition sticky.
func (s *SafetySupervisor) Pause(reason string) {
	const methodCtx = "agent.SafetySupervisor.Pause"

	if reason == "" {
		reason = "автоматика приостановлена без указанной причины"
	}
	reason = methodCtx + ": " + reason
	s.paused.Store(true)
	s.mu.Lock()
	s.reason = reason
	client := s.client
	s.mu.Unlock()
	if client != nil {
		_ = client.PublishEvent(protocol.AgentEvent{
			Kind: "AUTOMATION_PAUSED", Severity: "warning", Message: reason, At: time.Now().UTC(),
		})
	}
}

// Emergency performs a sticky local stop before attempting network delivery.
func (s *SafetySupervisor) Emergency(reason string, userInitiated bool) {
	const methodCtx = "agent.SafetySupervisor.Emergency"

	if reason == "" {
		reason = "выполнена аварийная остановка без указанной причины"
	}
	reason = methodCtx + ": " + reason
	s.stopped.Store(true)
	s.paused.Store(true)
	s.mu.Lock()
	s.reason = reason
	client := s.client
	s.mu.Unlock()
	if client != nil {
		client.PublishEmergencyStop(reason, userInitiated)
	}
}

// RecordActionResult resets the failure streak after success and triggers the
// mandatory emergency stop after repeated failures.
func (s *SafetySupervisor) RecordActionResult(result protocol.ActionResult) {
	const methodCtx = "agent.SafetySupervisor.RecordActionResult"

	s.mu.Lock()
	if result.Success {
		s.consecutiveFail = 0
		s.mu.Unlock()
		return
	}
	s.consecutiveFail++
	failures := s.consecutiveFail
	limit := s.maxFailures
	s.mu.Unlock()
	if failures >= limit {
		s.Emergency(methodCtx+": три последовательные ошибки исполнения действий", false)
	}
}

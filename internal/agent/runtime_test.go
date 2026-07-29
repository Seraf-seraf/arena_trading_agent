package agent_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/arena-trading-agent/arena-trading-agent/internal/agent"
	"github.com/arena-trading-agent/arena-trading-agent/internal/protocol"
)

func TestActionExecutorRejectsStaleFrameBeforeInput(t *testing.T) {
	executor := agent.NewActionExecutor(nil, nil, nil, nil, func() uint64 { return 42 }, func() bool { return false })
	result := executor.Execute(context.Background(), protocol.ActionRequest{ID: "action-1", BasedOnFrame: 41, Deadline: time.Now().Add(time.Minute)})
	if result.Success {
		t.Fatal("команда на устаревшем кадре не должна исполняться")
	}
	if !strings.Contains(result.Error, "устаревшем кадре") {
		t.Fatalf("неожиданная ошибка: %q", result.Error)
	}
}

func TestActionExecutorRejectsEmergencyStopBeforeInput(t *testing.T) {
	executor := agent.NewActionExecutor(nil, nil, nil, nil, func() uint64 { return 42 }, func() bool { return true })
	result := executor.Execute(context.Background(), protocol.ActionRequest{ID: "action-1", BasedOnFrame: 42})
	if result.Success || !strings.Contains(result.Error, "аварийной кнопкой") {
		t.Fatalf("аварийная остановка не отклонила команду: %+v", result)
	}
}

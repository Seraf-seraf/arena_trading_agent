//go:build e2e

package e2e_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
	"time"
)

type health struct {
	Status string `json:"status"`
	Agents int    `json:"agents"`
}

func TestControllerAndAgentLifecycle(t *testing.T) {
	repositoryRoot := filepath.Clean(filepath.Join(filepath.Dir(currentFile(t)), "..", ".."))
	controllerBinary := filepath.Join(t.TempDir(), "controller")
	agentBinary := filepath.Join(t.TempDir(), "windows-agent")
	buildBinary(t, repositoryRoot, controllerBinary, "./cmd/controller")
	buildBinary(t, repositoryRoot, agentBinary, "./cmd/windows-agent")

	address := availableAddress(t)
	controllerLogs := &bytes.Buffer{}
	controllerProcess := startProcess(t, controllerLogs, controllerBinary, "-listen", address)
	t.Cleanup(func() { stopProcess(t, controllerProcess, controllerLogs) })

	healthURL := "http://" + address + "/healthz"
	waitForHealth(t, healthURL, func(value health) bool {
		return value.Status == "ok" && value.Agents == 0
	})

	agentLogs := &bytes.Buffer{}
	agentProcess := startProcess(t, agentLogs, agentBinary,
		"-controller", "ws://"+address+"/ws/agent",
		"-agent-id", "e2e-windows-agent",
	)

	waitForHealth(t, healthURL, func(value health) bool { return value.Agents == 1 })
	time.Sleep(16 * time.Second)
	waitForHealth(t, healthURL, func(value health) bool { return value.Agents == 1 })
	stopProcess(t, agentProcess, agentLogs)
	waitForHealth(t, healthURL, func(value health) bool { return value.Agents == 0 })
}

func currentFile(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("не удалось определить путь e2e-теста")
	}
	return file
}

func buildBinary(t *testing.T, repositoryRoot, output, packagePath string) {
	t.Helper()
	command := exec.Command("go", "build", "-o", output, packagePath)
	command.Dir = repositoryRoot
	if outputBytes, err := command.CombinedOutput(); err != nil {
		t.Fatalf("не удалось собрать %s: %v\n%s", packagePath, err, outputBytes)
	}
}

func availableAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("не удалось выбрать свободный порт: %v", err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("не удалось освободить тестовый порт: %v", err)
	}
	return address
}

func startProcess(t *testing.T, logs *bytes.Buffer, binary string, arguments ...string) *exec.Cmd {
	t.Helper()
	command := exec.Command(binary, arguments...)
	command.Stdout = logs
	command.Stderr = logs
	if err := command.Start(); err != nil {
		t.Fatalf("не удалось запустить %s: %v", binary, err)
	}
	return command
}

func stopProcess(t *testing.T, command *exec.Cmd, logs *bytes.Buffer) {
	t.Helper()
	if command == nil || command.Process == nil || command.ProcessState != nil {
		return
	}
	if err := command.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("не удалось отправить SIGTERM процессу: %v\n%s", err, logs.String())
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("процесс завершился с ошибкой: %v\n%s", err, logs.String())
		}
	case <-time.After(5 * time.Second):
		_ = command.Process.Kill()
		t.Fatalf("процесс не завершился после SIGTERM\n%s", logs.String())
	}
}

func waitForHealth(t *testing.T, url string, matches func(health) bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var lastError error
	for time.Now().Before(deadline) {
		response, err := http.Get(url)
		if err == nil {
			var value health
			decodeErr := json.NewDecoder(response.Body).Decode(&value)
			closeErr := response.Body.Close()
			if decodeErr == nil && closeErr == nil && response.StatusCode == http.StatusOK {
				if matches(value) {
					return
				}
				lastError = fmt.Errorf("неожиданное состояние: %+v", value)
			} else {
				lastError = fmt.Errorf("health endpoint вернул некорректный ответ: status=%d decode=%v close=%v", response.StatusCode, decodeErr, closeErr)
			}
		} else {
			lastError = err
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("health endpoint не перешёл в ожидаемое состояние: %v", lastError)
}

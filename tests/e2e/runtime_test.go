//go:build e2e

package e2e_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
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

type managedProcess struct {
	command *exec.Cmd
	logs    *bytes.Buffer
	done    chan error
}

func TestControllerAndAgentLifecycle(t *testing.T) {
	const methodCtx = "e2e.TestControllerAndAgentLifecycle"

	repositoryRoot, err := filepath.Abs(filepath.Join(filepath.Dir(currentFile(t)), "..", ".."))
	if err != nil {
		t.Fatalf("%s: не удалось определить абсолютный путь репозитория: %v", methodCtx, err)
	}
	runtimeRoot := t.TempDir()
	processDirectory := filepath.Join(runtimeRoot, "process")
	if err := os.Mkdir(processDirectory, 0o700); err != nil {
		t.Fatalf("%s: не удалось создать временный рабочий каталог процессов: %v", methodCtx, err)
	}
	controllerBinary := filepath.Join(runtimeRoot, "controller")
	agentBinary := filepath.Join(runtimeRoot, "windows-agent")
	configPath := filepath.Join(repositoryRoot, "configs", "runtime.example.json")
	databasePath := filepath.Join(runtimeRoot, "arena.db")
	recordingsPath := filepath.Join(runtimeRoot, "recordings")
	buildBinary(t, repositoryRoot, controllerBinary, "./cmd/controller")
	buildBinary(t, repositoryRoot, agentBinary, "./cmd/windows-agent")

	address := availableAddress(t)
	controllerLogs := &bytes.Buffer{}
	controllerProcess := startProcess(
		t,
		processDirectory,
		controllerLogs,
		controllerBinary,
		"-listen", address,
		"-config", configPath,
		"-db", databasePath,
		"-recordings", recordingsPath,
		"-lm-studio", "http://127.0.0.1:0",
		"-lm-model", "qwen3.5-0.8b",
		"-lm-auto-load=false",
		"-ocr", "http://127.0.0.1:0",
	)
	t.Cleanup(func() { stopProcess(t, controllerProcess) })

	healthURL := "http://" + address + "/healthz"
	waitForHealth(t, healthURL, func(value health) bool {
		return value.Status == "ok" && value.Agents == 0
	})
	if _, err := os.Stat(databasePath); err != nil {
		t.Fatalf("%s: временная SQLite не создана: %v\n%s", methodCtx, err, controllerLogs.String())
	}
	if info, err := os.Stat(recordingsPath); err != nil {
		t.Fatalf("%s: временный каталог записей не создан: %v\n%s", methodCtx, err, controllerLogs.String())
	} else if !info.IsDir() {
		t.Fatalf("%s: путь записей не является каталогом: %s", methodCtx, recordingsPath)
	}

	agentLogs := &bytes.Buffer{}
	agentProcess := startProcess(t, processDirectory, agentLogs, agentBinary,
		"-controller", "ws://"+address+"/ws/agent",
		"-agent-id", "e2e-windows-agent",
	)
	t.Cleanup(func() { stopProcess(t, agentProcess) })

	waitForHealth(t, healthURL, func(value health) bool { return value.Agents == 1 })
	time.Sleep(16 * time.Second)
	waitForHealth(t, healthURL, func(value health) bool { return value.Agents == 1 })
	stopProcess(t, agentProcess)
	waitForHealth(t, healthURL, func(value health) bool { return value.Agents == 0 })
}

func currentFile(t *testing.T) string {
	const methodCtx = "e2e.currentFile"

	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatalf("%s: не удалось определить путь e2e-теста", methodCtx)
	}
	return file
}

func buildBinary(t *testing.T, repositoryRoot, output, packagePath string) {
	const methodCtx = "e2e.buildBinary"

	t.Helper()
	command := exec.Command("go", "build", "-o", output, packagePath)
	command.Dir = repositoryRoot
	if outputBytes, err := command.CombinedOutput(); err != nil {
		t.Fatalf("%s: не удалось собрать %s: %v\n%s", methodCtx, packagePath, err, outputBytes)
	}
}

func availableAddress(t *testing.T) string {
	const methodCtx = "e2e.availableAddress"

	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("%s: не удалось выбрать свободный порт: %v", methodCtx, err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("%s: не удалось освободить тестовый порт: %v", methodCtx, err)
	}
	return address
}

func startProcess(
	t *testing.T,
	workingDirectory string,
	logs *bytes.Buffer,
	binary string,
	arguments ...string,
) *managedProcess {
	const methodCtx = "e2e.startProcess"

	t.Helper()
	command := exec.Command(binary, arguments...)
	command.Dir = workingDirectory
	command.Stdout = logs
	command.Stderr = logs
	if err := command.Start(); err != nil {
		t.Fatalf("%s: не удалось запустить %s: %v", methodCtx, binary, err)
	}
	process := &managedProcess{
		command: command,
		logs:    logs,
		done:    make(chan error, 1),
	}
	go func() {
		process.done <- command.Wait()
		close(process.done)
	}()
	return process
}

func stopProcess(t *testing.T, process *managedProcess) {
	const methodCtx = "e2e.stopProcess"

	t.Helper()
	if process == nil || process.command == nil || process.command.Process == nil {
		return
	}
	select {
	case err := <-process.done:
		if err != nil {
			t.Fatalf("%s: процесс завершился до остановки с ошибкой: %v\n%s", methodCtx, err, process.logs.String())
		}
		return
	default:
	}
	if err := process.command.Process.Signal(syscall.SIGTERM); err != nil &&
		!errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("%s: не удалось отправить SIGTERM процессу: %v\n%s", methodCtx, err, process.logs.String())
	}
	select {
	case err := <-process.done:
		if err != nil {
			t.Fatalf("%s: процесс завершился с ошибкой: %v\n%s", methodCtx, err, process.logs.String())
		}
	case <-time.After(5 * time.Second):
		if err := process.command.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			t.Fatalf("%s: процесс не завершился после SIGTERM и не был принудительно остановлен: %v\n%s", methodCtx, err, process.logs.String())
		}
		<-process.done
		t.Fatalf("%s: процесс не завершился за 5 секунд после SIGTERM\n%s", methodCtx, process.logs.String())
	}
}

func waitForHealth(t *testing.T, url string, matches func(health) bool) {
	const methodCtx = "e2e.waitForHealth"

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
				lastError = fmt.Errorf("%s: неожиданное состояние: %+v", methodCtx, value)
			} else {
				lastError = fmt.Errorf(
					"%s: endpoint состояния вернул некорректный ответ: status=%d decode=%v close=%v",
					methodCtx,
					response.StatusCode,
					decodeErr,
					closeErr,
				)
			}
		} else {
			lastError = err
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("%s: endpoint состояния не перешёл в ожидаемое состояние: %v", methodCtx, lastError)
}

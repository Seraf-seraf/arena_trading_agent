//go:build windows

package winplatform

import (
	"context"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"unsafe"

	"github.com/arena-trading-agent/arena-trading-agent/internal/protocol"
)

// WindowManager finds the game window and translates its client area to
// physical screen coordinates. It safely re-resolves a stale cached HWND.
type WindowManager struct {
	criteria WindowCriteria
	mu       sync.Mutex
	handle   uintptr
}

// NewWindowManager constructs a manager for the supplied executable/title.
func NewWindowManager(criteria WindowCriteria) *WindowManager {
	return &WindowManager{criteria: criteria}
}

// EnableDPIAwareness opts the current process into per-monitor-v2 DPI
// awareness where available. It should be called before windows are queried.
func EnableDPIAwareness() error {
	const methodCtx = "winplatform.EnableDPIAwareness"

	if err := procSetProcessDPIAwarenessCtx.Find(); err == nil {
		// DPI_AWARENESS_CONTEXT_PER_MONITOR_AWARE_V2 is the pseudo-handle -4.
		result, _, callErr := procSetProcessDPIAwarenessCtx.Call(^uintptr(3))
		if result != 0 {
			return nil
		}
		// ERROR_ACCESS_DENIED means awareness was set earlier and cannot be
		// changed. The process is already committed to a coordinate mode.
		if errno, ok := callErr.(syscall.Errno); ok && errno == syscall.ERROR_ACCESS_DENIED {
			return nil
		}
		return fmt.Errorf("%s: не удалось включить DPI awareness: %w", methodCtx, windowsCallError("SetProcessDpiAwarenessContext", callErr))
	}

	result, _, callErr := procSetProcessDPIAware.Call()
	if result == 0 {
		return fmt.Errorf("%s: не удалось включить DPI awareness: %w", methodCtx, windowsCallError("SetProcessDPIAware", callErr))
	}
	return nil
}

// Invalidate discards the cached HWND so the next call performs enumeration.
func (manager *WindowManager) Invalidate() {
	manager.mu.Lock()
	manager.handle = 0
	manager.mu.Unlock()
}

// Snapshot returns the current window geometry and safety status.
func (manager *WindowManager) Snapshot(ctx context.Context) (WindowSnapshot, error) {
	const methodCtx = "winplatform.WindowManager.Snapshot"

	if err := ctx.Err(); err != nil {
		return WindowSnapshot{}, fmt.Errorf("%s: контекст завершён: %w", methodCtx, err)
	}
	if strings.TrimSpace(manager.criteria.TitleContains) == "" &&
		strings.TrimSpace(manager.criteria.ProcessName) == "" {
		return WindowSnapshot{}, fmt.Errorf("%s: критерии окна должны содержать имя процесса или заголовок", methodCtx)
	}

	manager.mu.Lock()
	cached := manager.handle
	manager.mu.Unlock()
	if cached != 0 && isUsableWindow(cached) {
		snapshot, err := snapshotWindow(cached)
		if err == nil && matchesWindow(manager.criteria, snapshot.Title, snapshot.ProcessName) {
			return snapshot, nil
		}
		manager.Invalidate()
	}

	snapshot, err := manager.find(ctx)
	if err != nil {
		return WindowSnapshot{}, fmt.Errorf("%s: не удалось найти окно игры: %w", methodCtx, err)
	}
	manager.mu.Lock()
	manager.handle = snapshot.Handle
	manager.mu.Unlock()
	return snapshot, nil
}

// Status implements agent.WindowManager.
func (manager *WindowManager) Status(ctx context.Context) (protocol.WindowStatus, error) {
	const methodCtx = "winplatform.WindowManager.Status"

	snapshot, err := manager.Snapshot(ctx)
	if err != nil {
		return protocol.WindowStatus{}, fmt.Errorf("%s: не удалось получить снимок окна: %w", methodCtx, err)
	}
	dpiPercent := int((snapshot.DPI*100 + 48) / 96)
	return protocol.WindowStatus{
		Active:     snapshot.Active,
		Minimized:  snapshot.Minimized,
		Width:      snapshot.ClientArea.Width,
		Height:     snapshot.ClientArea.Height,
		DPIPercent: dpiPercent,
	}, nil
}

func (manager *WindowManager) find(ctx context.Context) (WindowSnapshot, error) {
	const methodCtx = "winplatform.WindowManager.find"

	var (
		candidates  []WindowSnapshot
		callbackErr error
	)
	callback := syscall.NewCallback(func(handle uintptr, _ uintptr) uintptr {
		if err := ctx.Err(); err != nil {
			callbackErr = err
			return 0
		}
		if !isUsableWindow(handle) {
			return 1
		}
		snapshot, err := snapshotWindow(handle)
		if err != nil || !matchesWindow(manager.criteria, snapshot.Title, snapshot.ProcessName) {
			return 1
		}
		candidates = append(candidates, snapshot)
		return 1
	})
	result, _, callErr := procEnumWindows.Call(callback, 0)
	runtime.KeepAlive(callback)
	if callbackErr != nil {
		return WindowSnapshot{}, fmt.Errorf("%s: перечисление окон прервано: %w", methodCtx, callbackErr)
	}
	if result == 0 {
		return WindowSnapshot{}, fmt.Errorf("%s: не удалось перечислить окна: %w", methodCtx, windowsCallError("EnumWindows", callErr))
	}
	if len(candidates) == 0 {
		return WindowSnapshot{}, fmt.Errorf(
			"%s: окно игры не найдено (процесс=%q, заголовок содержит=%q)",
			methodCtx,
			manager.criteria.ProcessName,
			manager.criteria.TitleContains,
		)
	}

	best := candidates[0]
	for _, candidate := range candidates[1:] {
		if windowScore(candidate) > windowScore(best) {
			best = candidate
		}
	}
	return best, nil
}

func snapshotWindow(handle uintptr) (WindowSnapshot, error) {
	const methodCtx = "winplatform.snapshotWindow"

	title := windowTitle(handle)
	processName, _ := windowProcessName(handle)

	var client winRect
	result, _, callErr := procGetClientRect.Call(handle, uintptr(unsafe.Pointer(&client)))
	if result == 0 {
		return WindowSnapshot{}, fmt.Errorf("%s: не удалось получить клиентскую область: %w", methodCtx, windowsCallError("GetClientRect", callErr))
	}
	origin := winPoint{}
	result, _, callErr = procClientToScreen.Call(handle, uintptr(unsafe.Pointer(&origin)))
	if result == 0 {
		return WindowSnapshot{}, fmt.Errorf("%s: не удалось преобразовать координаты окна: %w", methodCtx, windowsCallError("ClientToScreen", callErr))
	}
	width := int(client.Right - client.Left)
	height := int(client.Bottom - client.Top)
	if width < 0 || height < 0 {
		return WindowSnapshot{}, fmt.Errorf("%s: окно игры вернуло некорректную клиентскую область %dx%d", methodCtx, width, height)
	}

	foreground, _, _ := procGetForegroundWindow.Call()
	minimized, _, _ := procIsIconic.Call(handle)
	return WindowSnapshot{
		Handle:      handle,
		Title:       title,
		ProcessName: processName,
		ClientArea: PixelRectangle{
			Left:   int(origin.X),
			Top:    int(origin.Y),
			Width:  width,
			Height: height,
		},
		Active:    foreground == handle,
		Minimized: minimized != 0,
		DPI:       windowDPI(handle),
	}, nil
}

func isUsableWindow(handle uintptr) bool {
	valid, _, _ := procIsWindow.Call(handle)
	if valid == 0 {
		return false
	}
	visible, _, _ := procIsWindowVisible.Call(handle)
	return visible != 0
}

func windowTitle(handle uintptr) string {
	length, _, _ := procGetWindowTextLengthW.Call(handle)
	if length == 0 {
		return ""
	}
	buffer := make([]uint16, int(length)+1)
	copied, _, _ := procGetWindowTextW.Call(
		handle,
		uintptr(unsafe.Pointer(&buffer[0])),
		uintptr(len(buffer)),
	)
	if copied == 0 {
		return ""
	}
	return syscall.UTF16ToString(buffer[:copied])
}

func windowProcessName(handle uintptr) (string, error) {
	const methodCtx = "winplatform.windowProcessName"

	var processID uint32
	_, _, callErr := procGetWindowThreadProcessID.Call(handle, uintptr(unsafe.Pointer(&processID)))
	if processID == 0 {
		return "", fmt.Errorf("%s: не удалось получить идентификатор процесса окна: %w", methodCtx, windowsCallError("GetWindowThreadProcessId", callErr))
	}
	process, _, callErr := procOpenProcess.Call(processQueryLimitedInformation, 0, uintptr(processID))
	if process == 0 {
		return "", fmt.Errorf("%s: не удалось открыть процесс окна: %w", methodCtx, windowsCallError("OpenProcess", callErr))
	}
	defer procCloseHandle.Call(process)

	buffer := make([]uint16, 32768)
	size := uint32(len(buffer))
	result, _, callErr := procQueryFullProcessImageNameW.Call(
		process,
		0,
		uintptr(unsafe.Pointer(&buffer[0])),
		uintptr(unsafe.Pointer(&size)),
	)
	if result == 0 {
		return "", fmt.Errorf("%s: не удалось получить имя образа процесса: %w", methodCtx, windowsCallError("QueryFullProcessImageNameW", callErr))
	}
	return normalizeExecutable(syscall.UTF16ToString(buffer[:size])), nil
}

func windowDPI(handle uintptr) uint32 {
	if err := procGetDPIForWindow.Find(); err == nil {
		dpi, _, _ := procGetDPIForWindow.Call(handle)
		if dpi != 0 {
			return uint32(dpi)
		}
	}
	deviceContext, _, _ := procGetDC.Call(handle)
	if deviceContext != 0 {
		defer procReleaseDC.Call(handle, deviceContext)
		dpi, _, _ := procGetDeviceCaps.Call(deviceContext, logPixelsX)
		if dpi != 0 {
			return uint32(dpi)
		}
	}
	return 96
}

func windowScore(snapshot WindowSnapshot) int64 {
	score := int64(snapshot.ClientArea.Width) * int64(snapshot.ClientArea.Height)
	if !snapshot.Minimized {
		score += 1 << 50
	}
	if snapshot.Active {
		score += 1 << 60
	}
	return score
}

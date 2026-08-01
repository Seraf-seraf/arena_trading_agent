//go:build !windows

package winplatform

import (
	"context"
	"fmt"

	"github.com/arena-trading-agent/arena-trading-agent/internal/domain"
	"github.com/arena-trading-agent/arena-trading-agent/internal/protocol"
)

// WindowManager is a non-Windows compatibility placeholder.
type WindowManager struct {
	criteria WindowCriteria
}

// NewWindowManager constructs a non-Windows placeholder.
func NewWindowManager(criteria WindowCriteria) *WindowManager {
	return &WindowManager{criteria: criteria}
}

// EnableDPIAwareness is unavailable outside Windows.
func EnableDPIAwareness() error {
	const methodCtx = "winplatform.EnableDPIAwareness"

	return fmt.Errorf("%s: невозможно настроить масштабирование: %w", methodCtx, ErrUnsupported)
}

// Invalidate is a no-op outside Windows.
func (manager *WindowManager) Invalidate() {}

// Snapshot is unavailable outside Windows.
func (manager *WindowManager) Snapshot(context.Context) (WindowSnapshot, error) {
	const methodCtx = "winplatform.WindowManager.Snapshot"

	return WindowSnapshot{}, fmt.Errorf("%s: невозможно получить снимок окна: %w", methodCtx, ErrUnsupported)
}

// Status is unavailable outside Windows.
func (manager *WindowManager) Status(context.Context) (protocol.WindowStatus, error) {
	const methodCtx = "winplatform.WindowManager.Status"

	return protocol.WindowStatus{}, fmt.Errorf("%s: невозможно получить состояние окна: %w", methodCtx, ErrUnsupported)
}

// GDICapture is a non-Windows compatibility placeholder.
type GDICapture struct {
	window *WindowManager
}

// NewGDICapture constructs a non-Windows placeholder.
func NewGDICapture(window *WindowManager) *GDICapture {
	return &GDICapture{window: window}
}

// Capture is unavailable outside Windows.
func (capture *GDICapture) Capture(context.Context) (protocol.Frame, error) {
	const methodCtx = "winplatform.GDICapture.Capture"

	return protocol.Frame{}, fmt.Errorf("%s: невозможно захватить кадр: %w", methodCtx, ErrUnsupported)
}

// CaptureRegion is unavailable outside Windows.
func (capture *GDICapture) CaptureRegion(context.Context, domain.Rectangle) (protocol.Frame, error) {
	const methodCtx = "winplatform.GDICapture.CaptureRegion"

	return protocol.Frame{}, fmt.Errorf("%s: невозможно захватить область кадра: %w", methodCtx, ErrUnsupported)
}

// DesktopCapture is a non-Windows compatibility placeholder.
type DesktopCapture struct {
	window *WindowManager
}

// NewDesktopCapture constructs a non-Windows placeholder.
func NewDesktopCapture(window *WindowManager) *DesktopCapture {
	return &DesktopCapture{window: window}
}

// Capture is unavailable outside Windows.
func (capture *DesktopCapture) Capture(context.Context) (protocol.Frame, error) {
	const methodCtx = "winplatform.DesktopCapture.Capture"

	return protocol.Frame{}, fmt.Errorf("%s: невозможно захватить кадр рабочего стола: %w", methodCtx, ErrUnsupported)
}

// CaptureRegion is unavailable outside Windows.
func (capture *DesktopCapture) CaptureRegion(context.Context, domain.Rectangle) (protocol.Frame, error) {
	const methodCtx = "winplatform.DesktopCapture.CaptureRegion"

	return protocol.Frame{}, fmt.Errorf("%s: невозможно захватить область рабочего стола: %w", methodCtx, ErrUnsupported)
}

// NewAdaptiveCapture constructs a non-Windows placeholder.
func NewAdaptiveCapture(window *WindowManager) *AdaptiveCapture {
	return newAdaptiveCapture(NewDesktopCapture(window), nil)
}

// SendInputDriver is a non-Windows compatibility placeholder.
type SendInputDriver struct {
	window *WindowManager
}

// NewSendInputDriver constructs a non-Windows placeholder.
func NewSendInputDriver(window *WindowManager) *SendInputDriver {
	return &SendInputDriver{window: window}
}

// Move is unavailable outside Windows.
func (driver *SendInputDriver) Move(context.Context, domain.Point) error {
	const methodCtx = "winplatform.SendInputDriver.Move"

	return fmt.Errorf("%s: невозможно переместить указатель: %w", methodCtx, ErrUnsupported)
}

// Click is unavailable outside Windows.
func (driver *SendInputDriver) Click(context.Context, string) error {
	const methodCtx = "winplatform.SendInputDriver.Click"

	return fmt.Errorf("%s: невозможно выполнить щелчок: %w", methodCtx, ErrUnsupported)
}

// Scroll is unavailable outside Windows.
func (driver *SendInputDriver) Scroll(context.Context, int) error {
	const methodCtx = "winplatform.SendInputDriver.Scroll"

	return fmt.Errorf("%s: невозможно выполнить прокрутку: %w", methodCtx, ErrUnsupported)
}

// Key is unavailable outside Windows.
func (driver *SendInputDriver) Key(context.Context, string) error {
	const methodCtx = "winplatform.SendInputDriver.Key"

	return fmt.Errorf("%s: невозможно нажать клавишу: %w", methodCtx, ErrUnsupported)
}

// Text is unavailable outside Windows.
func (driver *SendInputDriver) Text(context.Context, string) error {
	const methodCtx = "winplatform.SendInputDriver.Text"

	return fmt.Errorf("%s: невозможно ввести текст: %w", methodCtx, ErrUnsupported)
}

// SafetyMonitor is a non-Windows compatibility placeholder.
type SafetyMonitor struct{}

// NewSafetyMonitor constructs a non-Windows placeholder.
func NewSafetyMonitor(SafetyOptions) *SafetyMonitor {
	return &SafetyMonitor{}
}

// Run is unavailable outside Windows.
func (monitor *SafetyMonitor) Run(context.Context, func(SafetyEvent)) error {
	const methodCtx = "winplatform.SafetyMonitor.Run"

	return fmt.Errorf("%s: невозможно запустить монитор безопасности: %w", methodCtx, ErrUnsupported)
}

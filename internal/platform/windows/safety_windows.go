//go:build windows

package winplatform

import (
	"context"
	"fmt"
	"time"
)

// SafetyMonitor detects the emergency chord. Ordinary physical input does not
// pause automation; the game window and action executor remain responsible for
// rejecting input when the target window is inactive.
type SafetyMonitor struct {
	options SafetyOptions
}

// NewSafetyMonitor constructs a monitor. Zero-valued timing options receive
// conservative defaults.
func NewSafetyMonitor(options SafetyOptions) *SafetyMonitor {
	if options.PollInterval <= 0 {
		options.PollInterval = 50 * time.Millisecond
	}
	if options.EmergencyHotkey.VirtualKey == 0 {
		options.EmergencyHotkey = DefaultEmergencyHotkey()
	}
	return &SafetyMonitor{options: options}
}

// Run polls until ctx is cancelled. Handler should return quickly; it is
// invoked synchronously to preserve event order.
func (monitor *SafetyMonitor) Run(ctx context.Context, handler func(SafetyEvent)) error {
	const methodCtx = "winplatform.SafetyMonitor.Run"

	if handler == nil {
		return fmt.Errorf("%s: обработчик событий безопасности обязателен", methodCtx)
	}
	ticker := time.NewTicker(monitor.options.PollInterval)
	defer ticker.Stop()

	var hotkeyLatched bool
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			hotkeyPressed := isHotkeyPressed(monitor.options.EmergencyHotkey)
			if hotkeyPressed && !hotkeyLatched {
				handler(SafetyEvent{
					Kind: SafetyEmergencyHotkey,
					At:   time.Now().UTC(),
				})
			}
			hotkeyLatched = hotkeyPressed
		}
	}
}

func isHotkeyPressed(hotkey Hotkey) bool {
	if hotkey.VirtualKey == 0 || !isVirtualKeyDown(hotkey.VirtualKey) {
		return false
	}
	if hotkey.Control && !isVirtualKeyDown(0x11) {
		return false
	}
	if hotkey.Alt && !isVirtualKeyDown(0x12) {
		return false
	}
	if hotkey.Shift && !isVirtualKeyDown(0x10) {
		return false
	}
	if hotkey.Windows && !isVirtualKeyDown(0x5b) && !isVirtualKeyDown(0x5c) {
		return false
	}
	return true
}

func isVirtualKeyDown(key uint16) bool {
	state, _, _ := procGetAsyncKeyState.Call(uintptr(key))
	return uint16(state)&0x8000 != 0
}

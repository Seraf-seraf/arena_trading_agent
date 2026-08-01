// Package winplatform contains the Windows-specific eyes, hands, and safety
// adapters used by the local agent runtime.
//
// The package name deliberately differs from its directory name so callers can
// use it alongside golang.org/x/sys/windows without aliases.
package winplatform

import (
	"errors"
	"time"
)

// ErrUnsupported is returned by the non-Windows compatibility implementation.
var ErrUnsupported = errors.New("адаптеры платформы Windows недоступны в этой операционной системе")

// WindowCriteria identifies the game window. When both fields are set, both
// must match. ProcessName is matched case-insensitively against the executable
// basename; TitleContains is a case-insensitive substring.
type WindowCriteria struct {
	ProcessName   string
	TitleContains string
}

// PixelRectangle is a rectangle in physical screen or client pixels.
type PixelRectangle struct {
	Left   int
	Top    int
	Width  int
	Height int
}

// Contains reports whether a screen-space point belongs to the rectangle.
func (r PixelRectangle) Contains(x, y int) bool {
	return r.Width > 0 && r.Height > 0 &&
		x >= r.Left && x < r.Left+r.Width &&
		y >= r.Top && y < r.Top+r.Height
}

// WindowSnapshot is an internally consistent view of the selected game
// window. ClientArea is expressed in physical screen pixels.
type WindowSnapshot struct {
	Handle      uintptr
	Title       string
	ProcessName string
	ClientArea  PixelRectangle
	Active      bool
	Minimized   bool
	DPI         uint32
}

// Hotkey describes a global key chord polled by SafetyMonitor.
type Hotkey struct {
	VirtualKey uint16
	Control    bool
	Alt        bool
	Shift      bool
	Windows    bool
}

// DefaultEmergencyHotkey returns Ctrl+Alt+F12.
func DefaultEmergencyHotkey() Hotkey {
	return Hotkey{VirtualKey: 0x7b, Control: true, Alt: true}
}

// SafetyOptions controls emergency-stop detection.
type SafetyOptions struct {
	PollInterval    time.Duration
	EmergencyHotkey Hotkey
}

// SafetyEventKind identifies why the safety monitor interrupted automation.
type SafetyEventKind string

const (
	// SafetyEmergencyHotkey is emitted when the emergency chord is pressed.
	SafetyEmergencyHotkey SafetyEventKind = "EMERGENCY_HOTKEY"
)

// SafetyEvent is delivered synchronously by SafetyMonitor.Run.
type SafetyEvent struct {
	Kind SafetyEventKind
	At   time.Time
}

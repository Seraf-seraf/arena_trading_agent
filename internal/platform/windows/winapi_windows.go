//go:build windows

package winplatform

import (
	"fmt"
	"syscall"
	"unsafe"
)

const (
	processQueryLimitedInformation = 0x1000

	logPixelsX = 88

	smXVirtualScreen  = 76
	smYVirtualScreen  = 77
	smCXVirtualScreen = 78
	smCYVirtualScreen = 79

	inputMouse    = 0
	inputKeyboard = 1

	mouseEventMove        = 0x0001
	mouseEventLeftDown    = 0x0002
	mouseEventLeftUp      = 0x0004
	mouseEventRightDown   = 0x0008
	mouseEventRightUp     = 0x0010
	mouseEventMiddleDown  = 0x0020
	mouseEventMiddleUp    = 0x0040
	mouseEventXDown       = 0x0080
	mouseEventXUp         = 0x0100
	mouseEventWheel       = 0x0800
	mouseEventVirtualDesk = 0x4000
	mouseEventAbsolute    = 0x8000

	xButton1 = 0x0001
	xButton2 = 0x0002

	keyEventExtendedKey = 0x0001
	keyEventKeyUp       = 0x0002
	keyEventUnicode     = 0x0004

	srccopy      = 0x00cc0020
	captureBLT   = 0x40000000
	dibRGBColors = 0
	biRGB        = 0
)

var (
	user32DLL   = syscall.NewLazyDLL("user32.dll")
	kernel32DLL = syscall.NewLazyDLL("kernel32.dll")
	gdi32DLL    = syscall.NewLazyDLL("gdi32.dll")

	procEnumWindows               = user32DLL.NewProc("EnumWindows")
	procIsWindow                  = user32DLL.NewProc("IsWindow")
	procIsWindowVisible           = user32DLL.NewProc("IsWindowVisible")
	procGetWindowTextLengthW      = user32DLL.NewProc("GetWindowTextLengthW")
	procGetWindowTextW            = user32DLL.NewProc("GetWindowTextW")
	procGetWindowThreadProcessID  = user32DLL.NewProc("GetWindowThreadProcessId")
	procGetClientRect             = user32DLL.NewProc("GetClientRect")
	procClientToScreen            = user32DLL.NewProc("ClientToScreen")
	procGetForegroundWindow       = user32DLL.NewProc("GetForegroundWindow")
	procIsIconic                  = user32DLL.NewProc("IsIconic")
	procGetDPIForWindow           = user32DLL.NewProc("GetDpiForWindow")
	procSetProcessDPIAware        = user32DLL.NewProc("SetProcessDPIAware")
	procSetProcessDPIAwarenessCtx = user32DLL.NewProc("SetProcessDpiAwarenessContext")
	procGetDC                     = user32DLL.NewProc("GetDC")
	procReleaseDC                 = user32DLL.NewProc("ReleaseDC")
	procSendInput                 = user32DLL.NewProc("SendInput")
	procGetSystemMetrics          = user32DLL.NewProc("GetSystemMetrics")
	procGetCursorPos              = user32DLL.NewProc("GetCursorPos")
	procGetAsyncKeyState          = user32DLL.NewProc("GetAsyncKeyState")

	procOpenProcess                = kernel32DLL.NewProc("OpenProcess")
	procCloseHandle                = kernel32DLL.NewProc("CloseHandle")
	procQueryFullProcessImageNameW = kernel32DLL.NewProc("QueryFullProcessImageNameW")

	procCreateCompatibleDC     = gdi32DLL.NewProc("CreateCompatibleDC")
	procDeleteDC               = gdi32DLL.NewProc("DeleteDC")
	procCreateCompatibleBitmap = gdi32DLL.NewProc("CreateCompatibleBitmap")
	procSelectObject           = gdi32DLL.NewProc("SelectObject")
	procDeleteObject           = gdi32DLL.NewProc("DeleteObject")
	procBitBlt                 = gdi32DLL.NewProc("BitBlt")
	procGetDIBits              = gdi32DLL.NewProc("GetDIBits")
	procGetDeviceCaps          = gdi32DLL.NewProc("GetDeviceCaps")
)

type winRect struct {
	Left   int32
	Top    int32
	Right  int32
	Bottom int32
}

type winPoint struct {
	X int32
	Y int32
}

type bitmapInfoHeader struct {
	Size          uint32
	Width         int32
	Height        int32
	Planes        uint16
	BitCount      uint16
	Compression   uint32
	SizeImage     uint32
	XPelsPerMeter int32
	YPelsPerMeter int32
	ClrUsed       uint32
	ClrImportant  uint32
}

type rgbQuad struct {
	Blue     byte
	Green    byte
	Red      byte
	Reserved byte
}

type bitmapInfo struct {
	Header bitmapInfoHeader
	Colors [1]rgbQuad
}

type mouseInput struct {
	DX        int32
	DY        int32
	MouseData uint32
	Flags     uint32
	Time      uint32
	ExtraInfo uintptr
}

type keyboardInput struct {
	VirtualKey uint16
	ScanCode   uint16
	Flags      uint32
	Time       uint32
	ExtraInfo  uintptr
}

// inputUnion has the size and alignment of the largest Win32 INPUT union
// member on both 32-bit and 64-bit architectures.
type inputUnion struct {
	alignment uintptr
	remaining [unsafe.Sizeof(mouseInput{}) - unsafe.Sizeof(uintptr(0))]byte
}

type winInput struct {
	Type uint32
	Data inputUnion
}

func windowsCallError(operation string, callErr error) error {
	const methodCtx = "winplatform.windowsCallError"

	if errno, ok := callErr.(syscall.Errno); !ok || errno == 0 {
		return fmt.Errorf("%s: операция WinAPI %s завершилась ошибкой", methodCtx, operation)
	}
	return fmt.Errorf("%s: операция WinAPI %s: %w", methodCtx, operation, callErr)
}

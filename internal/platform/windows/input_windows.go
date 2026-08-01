//go:build windows

package winplatform

import (
	"context"
	"fmt"
	"math"
	"strings"
	"sync"
	"unicode/utf16"
	"unsafe"

	"github.com/arena-trading-agent/arena-trading-agent/internal/domain"
)

const agentInputMarker = uintptr(0x4152454e)

// SendInputDriver implements normalized mouse and keyboard input with the
// Win32 SendInput API. Every operation rechecks foreground state.
type SendInputDriver struct {
	window *WindowManager
	mu     sync.Mutex
}

// NewSendInputDriver constructs a guarded input adapter.
func NewSendInputDriver(window *WindowManager) *SendInputDriver {
	return &SendInputDriver{window: window}
}

// Move moves the cursor to a normalized client-area point.
func (driver *SendInputDriver) Move(ctx context.Context, point domain.Point) error {
	const methodCtx = "winplatform.SendInputDriver.Move"

	driver.mu.Lock()
	defer driver.mu.Unlock()

	snapshot, err := driver.activeWindow(ctx)
	if err != nil {
		return fmt.Errorf("%s: окно не прошло проверку: %w", methodCtx, err)
	}
	clientX, clientY, err := clientPoint(point, snapshot.ClientArea.Width, snapshot.ClientArea.Height)
	if err != nil {
		return fmt.Errorf("%s: не удалось вычислить координату клиентской области: %w", methodCtx, err)
	}
	virtual, err := virtualDesktop()
	if err != nil {
		return fmt.Errorf("%s: не удалось определить виртуальный рабочий стол: %w", methodCtx, err)
	}
	screenX := snapshot.ClientArea.Left + clientX
	screenY := snapshot.ClientArea.Top + clientY
	if !virtual.Contains(screenX, screenY) {
		return fmt.Errorf("%s: целевая точка (%d,%d) находится вне виртуального рабочего стола %+v", methodCtx, screenX, screenY, virtual)
	}

	absoluteX := int32(math.Round(float64(screenX-virtual.Left) * 65535 / float64(virtual.Width-1)))
	absoluteY := int32(math.Round(float64(screenY-virtual.Top) * 65535 / float64(virtual.Height-1)))
	if err := driver.send(ctx, []winInput{newMouseInput(mouseInput{
		DX:        absoluteX,
		DY:        absoluteY,
		Flags:     mouseEventMove | mouseEventAbsolute | mouseEventVirtualDesk,
		ExtraInfo: agentInputMarker,
	})}); err != nil {
		return fmt.Errorf("%s: не удалось отправить перемещение указателя: %w", methodCtx, err)
	}
	return nil
}

// Click presses and releases a mouse button at the current cursor location.
// The cursor must still be within the game client area.
func (driver *SendInputDriver) Click(ctx context.Context, button string) error {
	const methodCtx = "winplatform.SendInputDriver.Click"

	driver.mu.Lock()
	defer driver.mu.Unlock()

	snapshot, err := driver.activeWindow(ctx)
	if err != nil {
		return fmt.Errorf("%s: окно не прошло проверку: %w", methodCtx, err)
	}
	var cursor winPoint
	result, _, callErr := procGetCursorPos.Call(uintptr(unsafe.Pointer(&cursor)))
	if result == 0 {
		return fmt.Errorf("%s: не удалось получить положение указателя: %w", methodCtx, windowsCallError("GetCursorPos", callErr))
	}
	if !snapshot.ClientArea.Contains(int(cursor.X), int(cursor.Y)) {
		return fmt.Errorf(
			"%s: клик запрещён: указатель (%d,%d) находится вне клиентской области игры %+v",
			methodCtx,
			cursor.X,
			cursor.Y,
			snapshot.ClientArea,
		)
	}

	down, up, data, err := mouseButton(button)
	if err != nil {
		return fmt.Errorf("%s: не удалось определить кнопку мыши: %w", methodCtx, err)
	}
	if err := driver.send(ctx, []winInput{
		newMouseInput(mouseInput{MouseData: data, Flags: down, ExtraInfo: agentInputMarker}),
		newMouseInput(mouseInput{MouseData: data, Flags: up, ExtraInfo: agentInputMarker}),
	}); err != nil {
		return fmt.Errorf("%s: не удалось отправить клик: %w", methodCtx, err)
	}
	return nil
}

// Scroll sends a vertical wheel delta in standard Win32 WHEEL_DELTA units
// (one detent is normally +/-120).
func (driver *SendInputDriver) Scroll(ctx context.Context, delta int) error {
	const methodCtx = "winplatform.SendInputDriver.Scroll"

	driver.mu.Lock()
	defer driver.mu.Unlock()

	if delta == 0 {
		return nil
	}
	if int64(delta) < math.MinInt32 || int64(delta) > math.MaxInt32 {
		return fmt.Errorf("%s: смещение прокрутки %d выходит за диапазон Win32", methodCtx, delta)
	}
	if _, err := driver.activeWindow(ctx); err != nil {
		return fmt.Errorf("%s: окно не прошло проверку: %w", methodCtx, err)
	}
	if err := driver.send(ctx, []winInput{newMouseInput(mouseInput{
		MouseData: uint32(int32(delta)),
		Flags:     mouseEventWheel,
		ExtraInfo: agentInputMarker,
	})}); err != nil {
		return fmt.Errorf("%s: не удалось отправить прокрутку: %w", methodCtx, err)
	}
	return nil
}

// Key presses and releases a named key or chord such as "ENTER" or
// "CTRL+A". Names are parsed case-insensitively.
func (driver *SendInputDriver) Key(ctx context.Context, value string) error {
	const methodCtx = "winplatform.SendInputDriver.Key"

	driver.mu.Lock()
	defer driver.mu.Unlock()

	if _, err := driver.activeWindow(ctx); err != nil {
		return fmt.Errorf("%s: окно не прошло проверку: %w", methodCtx, err)
	}
	keys, err := parseKeyChord(value)
	if err != nil {
		return fmt.Errorf("%s: не удалось разобрать сочетание клавиш: %w", methodCtx, err)
	}
	inputs := make([]winInput, 0, len(keys)*2)
	for _, key := range keys {
		inputs = append(inputs, newKeyboardInput(key, 0, keyboardFlags(key)))
	}
	for index := len(keys) - 1; index >= 0; index-- {
		key := keys[index]
		inputs = append(inputs, newKeyboardInput(key, 0, keyboardFlags(key)|keyEventKeyUp))
	}
	if err := driver.send(ctx, inputs); err != nil {
		return fmt.Errorf("%s: не удалось отправить клавиши: %w", methodCtx, err)
	}
	return nil
}

// Text types arbitrary Unicode text through KEYEVENTF_UNICODE events.
func (driver *SendInputDriver) Text(ctx context.Context, value string) error {
	const methodCtx = "winplatform.SendInputDriver.Text"

	driver.mu.Lock()
	defer driver.mu.Unlock()

	if value == "" {
		return nil
	}
	if _, err := driver.activeWindow(ctx); err != nil {
		return fmt.Errorf("%s: окно не прошло проверку: %w", methodCtx, err)
	}
	codeUnits := utf16.Encode([]rune(value))
	const codeUnitsPerBatch = 64
	for offset := 0; offset < len(codeUnits); offset += codeUnitsPerBatch {
		if _, err := driver.activeWindow(ctx); err != nil {
			return fmt.Errorf(
				"%s: окно перестало быть допустимым перед очередным пакетом текста: %w",
				methodCtx,
				err,
			)
		}
		end := offset + codeUnitsPerBatch
		if end > len(codeUnits) {
			end = len(codeUnits)
		}
		inputs := make([]winInput, 0, (end-offset)*2)
		for _, codeUnit := range codeUnits[offset:end] {
			inputs = append(inputs, newKeyboardInput(0, codeUnit, keyEventUnicode))
			inputs = append(inputs, newKeyboardInput(0, codeUnit, keyEventUnicode|keyEventKeyUp))
		}
		if err := driver.send(ctx, inputs); err != nil {
			return fmt.Errorf("%s: не удалось отправить пакет текста: %w", methodCtx, err)
		}
	}
	return nil
}

func (driver *SendInputDriver) activeWindow(ctx context.Context) (WindowSnapshot, error) {
	const methodCtx = "winplatform.SendInputDriver.activeWindow"

	if driver.window == nil {
		return WindowSnapshot{}, fmt.Errorf("%s: драйверу SendInput требуется диспетчер окна", methodCtx)
	}
	if err := ctx.Err(); err != nil {
		return WindowSnapshot{}, fmt.Errorf("%s: контекст завершён: %w", methodCtx, err)
	}
	snapshot, err := driver.window.Snapshot(ctx)
	if err != nil {
		return WindowSnapshot{}, fmt.Errorf("%s: не удалось определить окно ввода: %w", methodCtx, err)
	}
	if !snapshot.Active || snapshot.Minimized {
		return WindowSnapshot{}, fmt.Errorf("%s: ввод запрещён: окно игры неактивно или свёрнуто", methodCtx)
	}
	if snapshot.ClientArea.Width <= 0 || snapshot.ClientArea.Height <= 0 {
		return WindowSnapshot{}, fmt.Errorf("%s: ввод запрещён: клиентская область игры пуста", methodCtx)
	}
	return snapshot, nil
}

func (driver *SendInputDriver) send(ctx context.Context, inputs []winInput) error {
	const methodCtx = "winplatform.SendInputDriver.send"

	if len(inputs) == 0 {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%s: контекст завершён: %w", methodCtx, err)
	}

	sent, _, callErr := procSendInput.Call(
		uintptr(len(inputs)),
		uintptr(unsafe.Pointer(&inputs[0])),
		unsafe.Sizeof(winInput{}),
	)
	if int(sent) != len(inputs) {
		if sent == 0 {
			return fmt.Errorf("%s: вызов SendInput завершился ошибкой: %w", methodCtx, windowsCallError("SendInput", callErr))
		}
		return fmt.Errorf("%s: SendInput доставил %d из %d событий", methodCtx, sent, len(inputs))
	}
	return nil
}

func newMouseInput(value mouseInput) winInput {
	input := winInput{Type: inputMouse}
	*(*mouseInput)(unsafe.Pointer(&input.Data)) = value
	return input
}

func newKeyboardInput(virtualKey, scanCode uint16, flags uint32) winInput {
	input := winInput{Type: inputKeyboard}
	*(*keyboardInput)(unsafe.Pointer(&input.Data)) = keyboardInput{
		VirtualKey: virtualKey,
		ScanCode:   scanCode,
		Flags:      flags,
		ExtraInfo:  agentInputMarker,
	}
	return input
}

func mouseButton(button string) (uint32, uint32, uint32, error) {
	const methodCtx = "winplatform.mouseButton"

	switch strings.ToUpper(strings.TrimSpace(button)) {
	case "", "LEFT", "PRIMARY":
		return mouseEventLeftDown, mouseEventLeftUp, 0, nil
	case "RIGHT", "SECONDARY":
		return mouseEventRightDown, mouseEventRightUp, 0, nil
	case "MIDDLE":
		return mouseEventMiddleDown, mouseEventMiddleUp, 0, nil
	case "X1":
		return mouseEventXDown, mouseEventXUp, xButton1, nil
	case "X2":
		return mouseEventXDown, mouseEventXUp, xButton2, nil
	default:
		return 0, 0, 0, fmt.Errorf("%s: неподдерживаемая кнопка мыши %q", methodCtx, button)
	}
}

func virtualDesktop() (PixelRectangle, error) {
	const methodCtx = "winplatform.virtualDesktop"

	x, _, _ := procGetSystemMetrics.Call(smXVirtualScreen)
	y, _, _ := procGetSystemMetrics.Call(smYVirtualScreen)
	width, _, _ := procGetSystemMetrics.Call(smCXVirtualScreen)
	height, _, _ := procGetSystemMetrics.Call(smCYVirtualScreen)
	result := PixelRectangle{
		Left:   int(int32(x)),
		Top:    int(int32(y)),
		Width:  int(int32(width)),
		Height: int(int32(height)),
	}
	if result.Width <= 1 || result.Height <= 1 {
		return PixelRectangle{}, fmt.Errorf("%s: некорректная геометрия виртуального рабочего стола %+v", methodCtx, result)
	}
	return result, nil
}

func keyboardFlags(virtualKey uint16) uint32 {
	switch virtualKey {
	case 0x21, 0x22, 0x23, 0x24, 0x25, 0x26, 0x27, 0x28, 0x2d, 0x2e,
		0x5b, 0x5c, 0x6f:
		return keyEventExtendedKey
	default:
		return 0
	}
}

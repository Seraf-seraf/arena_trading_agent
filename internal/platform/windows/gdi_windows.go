//go:build windows

package winplatform

import (
	"context"
	"fmt"
	"image"
	"image/png"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/arena-trading-agent/arena-trading-agent/internal/domain"
	"github.com/arena-trading-agent/arena-trading-agent/internal/protocol"
)

var fullClientRegion = domain.Rectangle{X: 0, Y: 0, Width: 1, Height: 1}

// GDICapture captures the game client area with GDI BitBlt and encodes frames
// as PNG. It is the reliable baseline adapter; DXGI can replace it later
// without changing agent.CaptureDriver.
type GDICapture struct {
	window  *WindowManager
	frameID atomic.Uint64
}

// NewGDICapture constructs a GDI capture adapter.
func NewGDICapture(window *WindowManager) *GDICapture {
	capture := &GDICapture{window: window}
	capture.frameID.Store(uint64(time.Now().UTC().UnixNano()))
	return capture
}

// Capture captures the full client area.
func (capture *GDICapture) Capture(ctx context.Context) (protocol.Frame, error) {
	const methodCtx = "winplatform.GDICapture.Capture"

	frame, err := capture.CaptureRegion(ctx, fullClientRegion)
	if err != nil {
		return protocol.Frame{}, fmt.Errorf("%s: захват кадра завершился ошибкой: %w", methodCtx, err)
	}
	return frame, nil
}

// CaptureRegion captures a normalized subsection of the client area.
func (capture *GDICapture) CaptureRegion(ctx context.Context, region domain.Rectangle) (protocol.Frame, error) {
	const methodCtx = "winplatform.GDICapture.CaptureRegion"

	if capture.window == nil {
		return protocol.Frame{}, fmt.Errorf("%s: для захвата GDI требуется диспетчер окна", methodCtx)
	}
	if err := ctx.Err(); err != nil {
		return protocol.Frame{}, fmt.Errorf("%s: контекст завершён до захвата: %w", methodCtx, err)
	}
	snapshot, err := capture.window.Snapshot(ctx)
	if err != nil {
		return protocol.Frame{}, fmt.Errorf("%s: не удалось определить окно захвата: %w", methodCtx, err)
	}
	if err := validateGDICaptureSnapshot(snapshot); err != nil {
		return protocol.Frame{}, fmt.Errorf("%s: окно не прошло проверку безопасности GDI-захвата: %w", methodCtx, err)
	}
	pixels, err := regionPixels(region, snapshot.ClientArea.Width, snapshot.ClientArea.Height)
	if err != nil {
		return protocol.Frame{}, fmt.Errorf("%s: не удалось вычислить пиксельную область: %w", methodCtx, err)
	}
	data, err := captureGDI(snapshot.Handle, pixels)
	if err != nil {
		return protocol.Frame{}, fmt.Errorf("%s: низкоуровневый захват GDI завершился ошибкой: %w", methodCtx, err)
	}
	if err := ctx.Err(); err != nil {
		return protocol.Frame{}, fmt.Errorf("%s: контекст завершён после захвата: %w", methodCtx, err)
	}
	return protocol.Frame{
		ID:         capture.frameID.Add(1),
		CapturedAt: time.Now().UTC(),
		Region:     region,
		Encoding:   "png",
		Data:       data,
	}, nil
}

func captureGDI(window uintptr, pixels PixelRectangle) ([]byte, error) {
	const methodCtx = "winplatform.captureGDI"

	if err := validateCaptureDimensions(pixels); err != nil {
		return nil, fmt.Errorf("%s: небезопасные размеры буфера: %w", methodCtx, err)
	}
	sourceDC, _, callErr := procGetDC.Call(window)
	if sourceDC == 0 {
		return nil, fmt.Errorf("%s: не удалось получить контекст устройства: %w", methodCtx, windowsCallError("GetDC", callErr))
	}
	defer procReleaseDC.Call(window, sourceDC)

	memoryDC, _, callErr := procCreateCompatibleDC.Call(sourceDC)
	if memoryDC == 0 {
		return nil, fmt.Errorf("%s: не удалось создать совместимый контекст устройства: %w", methodCtx, windowsCallError("CreateCompatibleDC", callErr))
	}
	defer procDeleteDC.Call(memoryDC)

	bitmap, _, callErr := procCreateCompatibleBitmap.Call(
		sourceDC,
		uintptr(pixels.Width),
		uintptr(pixels.Height),
	)
	if bitmap == 0 {
		return nil, fmt.Errorf("%s: не удалось создать совместимое растровое изображение: %w", methodCtx, windowsCallError("CreateCompatibleBitmap", callErr))
	}
	defer procDeleteObject.Call(bitmap)

	previous, _, callErr := procSelectObject.Call(memoryDC, bitmap)
	if previous == 0 || previous == ^uintptr(0) {
		return nil, fmt.Errorf("%s: не удалось выбрать объект захвата: %w", methodCtx, windowsCallError("SelectObject", callErr))
	}
	bitmapSelected := true
	defer func() {
		if bitmapSelected {
			procSelectObject.Call(memoryDC, previous)
		}
	}()

	result, _, callErr := procBitBlt.Call(
		memoryDC,
		0,
		0,
		uintptr(pixels.Width),
		uintptr(pixels.Height),
		sourceDC,
		uintptr(pixels.Left),
		uintptr(pixels.Top),
		srccopy|captureBLT,
	)
	if result == 0 {
		return nil, fmt.Errorf("%s: не удалось скопировать изображение окна: %w", methodCtx, windowsCallError("BitBlt", callErr))
	}
	replaced, _, callErr := procSelectObject.Call(memoryDC, previous)
	if replaced == 0 || replaced == ^uintptr(0) {
		return nil, fmt.Errorf("%s: не удалось восстановить объект контекста устройства: %w", methodCtx, windowsCallError("SelectObject restore", callErr))
	}
	bitmapSelected = false

	raw := make([]byte, pixels.Width*pixels.Height*4)
	info := bitmapInfo{
		Header: bitmapInfoHeader{
			Size:        uint32(unsafe.Sizeof(bitmapInfoHeader{})),
			Width:       int32(pixels.Width),
			Height:      -int32(pixels.Height),
			Planes:      1,
			BitCount:    32,
			Compression: biRGB,
			SizeImage:   uint32(len(raw)),
		},
	}
	lines, _, callErr := procGetDIBits.Call(
		memoryDC,
		bitmap,
		0,
		uintptr(pixels.Height),
		uintptr(unsafe.Pointer(&raw[0])),
		uintptr(unsafe.Pointer(&info)),
		dibRGBColors,
	)
	if int(lines) != pixels.Height {
		if lines == 0 {
			return nil, fmt.Errorf("%s: не удалось прочитать пиксели кадра: %w", methodCtx, windowsCallError("GetDIBits", callErr))
		}
		return nil, fmt.Errorf("%s: GetDIBits вернул %d из %d строк развёртки", methodCtx, lines, pixels.Height)
	}

	for offset := 0; offset < len(raw); offset += 4 {
		raw[offset], raw[offset+2] = raw[offset+2], raw[offset]
		raw[offset+3] = 0xff
	}
	frame := &image.NRGBA{
		Pix: raw, Stride: pixels.Width * 4,
		Rect: image.Rect(0, 0, pixels.Width, pixels.Height),
	}
	encoded := captureEncodeBuffer{maximum: maxEncodedFrameBytes}
	encoder := png.Encoder{CompressionLevel: png.BestSpeed}
	if err := encoder.Encode(&encoded, frame); err != nil {
		return nil, fmt.Errorf("%s: не удалось закодировать кадр GDI в PNG: %w", methodCtx, err)
	}
	return encoded.data, nil
}

type captureEncodeBuffer struct {
	data    []byte
	maximum int
}

func (buffer *captureEncodeBuffer) Write(value []byte) (int, error) {
	const methodCtx = "winplatform.captureEncodeBuffer.Write"

	if buffer == nil || buffer.maximum <= 0 {
		return 0, fmt.Errorf("%s: буфер кодирования не настроен", methodCtx)
	}
	if len(value) > buffer.maximum-len(buffer.data) {
		return 0, fmt.Errorf(
			"%s: закодированный кадр превышает предел %d байт",
			methodCtx,
			buffer.maximum,
		)
	}
	buffer.data = append(buffer.data, value...)
	return len(value), nil
}

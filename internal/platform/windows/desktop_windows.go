//go:build windows

package winplatform

import (
	"context"
	"fmt"
	"time"

	"github.com/arena-trading-agent/arena-trading-agent/internal/domain"
	"github.com/arena-trading-agent/arena-trading-agent/internal/protocol"
)

// DesktopCapture reads the composed desktop pixels occupied by the foreground
// game client.
type DesktopCapture struct {
	window *WindowManager
}

// NewDesktopCapture constructs the foreground-only capture driver.
func NewDesktopCapture(window *WindowManager) *DesktopCapture {
	return &DesktopCapture{window: window}
}

// NewAdaptiveCapture builds the foreground desktop production capture.
func NewAdaptiveCapture(window *WindowManager) *AdaptiveCapture {
	return newAdaptiveCapture(NewDesktopCapture(window), nil)
}

// Capture captures the full visible client area.
func (capture *DesktopCapture) Capture(ctx context.Context) (protocol.Frame, error) {
	const methodCtx = "winplatform.DesktopCapture.Capture"

	frame, err := capture.CaptureRegion(ctx, fullClientRegion)
	if err != nil {
		return protocol.Frame{}, fmt.Errorf("%s: захват кадра завершился ошибкой: %w", methodCtx, err)
	}
	return frame, nil
}

// CaptureRegion refuses background windows so another application's contents
// can never be mistaken for the game.
func (capture *DesktopCapture) CaptureRegion(ctx context.Context, region domain.Rectangle) (protocol.Frame, error) {
	const methodCtx = "winplatform.DesktopCapture.CaptureRegion"

	if capture.window == nil {
		return protocol.Frame{}, fmt.Errorf("%s: для захвата рабочего стола требуется диспетчер окна", methodCtx)
	}
	snapshot, err := capture.window.Snapshot(ctx)
	if err != nil {
		return protocol.Frame{}, fmt.Errorf("%s: не удалось получить снимок окна: %w", methodCtx, err)
	}
	if !snapshot.Active || snapshot.Minimized {
		return protocol.Frame{}, fmt.Errorf("%s: захват рабочего стола требует активное окно игры", methodCtx)
	}
	pixels, err := regionPixels(region, snapshot.ClientArea.Width, snapshot.ClientArea.Height)
	if err != nil {
		return protocol.Frame{}, fmt.Errorf("%s: не удалось вычислить пиксельную область: %w", methodCtx, err)
	}
	pixels.Left += snapshot.ClientArea.Left
	pixels.Top += snapshot.ClientArea.Top
	data, err := captureGDI(0, pixels)
	if err != nil {
		return protocol.Frame{}, fmt.Errorf("%s: захват GDI рабочего стола завершился ошибкой: %w", methodCtx, err)
	}
	after, err := capture.window.Snapshot(ctx)
	if err != nil {
		return protocol.Frame{}, fmt.Errorf(
			"%s: не удалось повторно проверить окно после захвата рабочего стола: %w",
			methodCtx,
			err,
		)
	}
	if err := validateDesktopCaptureSnapshot(snapshot, after); err != nil {
		return protocol.Frame{}, fmt.Errorf(
			"%s: состояние окна изменилось во время захвата рабочего стола: %w",
			methodCtx,
			err,
		)
	}
	return protocol.Frame{
		CapturedAt: time.Now().UTC(), Region: region, Encoding: "png", Data: data,
	}, nil
}

package winplatform

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"math"
	"sync/atomic"
	"time"

	"github.com/arena-trading-agent/arena-trading-agent/internal/domain"
	"github.com/arena-trading-agent/arena-trading-agent/internal/protocol"
)

type captureDriver interface {
	Capture(context.Context) (protocol.Frame, error)
	CaptureRegion(context.Context, domain.Rectangle) (protocol.Frame, error)
}

// AdaptiveCapture validates frames from a primary capture driver and can use
// an optional fallback driver.
type AdaptiveCapture struct {
	primary  captureDriver
	fallback captureDriver
	frameID  atomic.Uint64
}

func newAdaptiveCapture(primary, fallback captureDriver) *AdaptiveCapture {
	capture := &AdaptiveCapture{primary: primary, fallback: fallback}
	// Frame IDs survive process restarts as a monotonically increasing
	// timestamp-based namespace. This avoids SQLite collisions with frames
	// recorded by an earlier Windows Agent process.
	capture.frameID.Store(uint64(time.Now().UTC().UnixNano()))
	return capture
}

// Capture captures the full client area.
func (capture *AdaptiveCapture) Capture(ctx context.Context) (protocol.Frame, error) {
	const methodCtx = "winplatform.AdaptiveCapture.Capture"

	frame, err := capture.capture(ctx, nil)
	if err != nil {
		return protocol.Frame{}, fmt.Errorf("%s: захват кадра завершился ошибкой: %w", methodCtx, err)
	}
	return frame, nil
}

// CaptureRegion captures a normalized client region.
func (capture *AdaptiveCapture) CaptureRegion(ctx context.Context, region domain.Rectangle) (protocol.Frame, error) {
	const methodCtx = "winplatform.AdaptiveCapture.CaptureRegion"

	frame, err := capture.capture(ctx, &region)
	if err != nil {
		return protocol.Frame{}, fmt.Errorf("%s: захват области завершился ошибкой: %w", methodCtx, err)
	}
	return frame, nil
}

func (capture *AdaptiveCapture) capture(ctx context.Context, region *domain.Rectangle) (protocol.Frame, error) {
	const methodCtx = "winplatform.AdaptiveCapture.capture"

	first, firstErr := callCapture(ctx, capture.primary, region)
	if firstErr == nil && !visuallyBlank(first.Data) {
		first.ID = capture.frameID.Add(1)
		return first, nil
	}
	if capture.fallback == nil {
		if firstErr != nil {
			return protocol.Frame{}, fmt.Errorf(
				"%s: основной драйвер захвата завершился ошибкой: %w",
				methodCtx,
				firstErr,
			)
		}
		return protocol.Frame{}, fmt.Errorf(
			"%s: основной драйвер захвата вернул визуально пустой кадр",
			methodCtx,
		)
	}
	second, secondErr := callCapture(ctx, capture.fallback, region)
	if secondErr != nil {
		if firstErr != nil {
			return protocol.Frame{}, fmt.Errorf(
				"%s: основной захват: %v; резервный захват: %w",
				methodCtx,
				firstErr,
				secondErr,
			)
		}
		return protocol.Frame{}, fmt.Errorf(
			"%s: основной захват вернул визуально пустой кадр; резервный захват: %w",
			methodCtx,
			secondErr,
		)
	}
	if visuallyBlank(second.Data) {
		return protocol.Frame{}, fmt.Errorf(
			"%s: основной и резервный драйверы вернули визуально пустые кадры",
			methodCtx,
		)
	}
	second.ID = capture.frameID.Add(1)
	return second, nil
}

func callCapture(ctx context.Context, driver captureDriver, region *domain.Rectangle) (protocol.Frame, error) {
	const methodCtx = "winplatform.callCapture"

	if driver == nil {
		return protocol.Frame{}, fmt.Errorf("%s: драйвер захвата не настроен", methodCtx)
	}
	if region == nil {
		frame, err := driver.Capture(ctx)
		if err != nil {
			return protocol.Frame{}, fmt.Errorf("%s: полный захват завершился ошибкой: %w", methodCtx, err)
		}
		return frame, nil
	}
	frame, err := driver.CaptureRegion(ctx, *region)
	if err != nil {
		return protocol.Frame{}, fmt.Errorf("%s: захват области завершился ошибкой: %w", methodCtx, err)
	}
	return frame, nil
}

func validateDesktopCaptureSnapshot(before, after WindowSnapshot) error {
	const methodCtx = "winplatform.validateDesktopCaptureSnapshot"

	if after.Minimized {
		return fmt.Errorf("%s: окно игры было свёрнуто во время захвата", methodCtx)
	}
	if !after.Active {
		return fmt.Errorf("%s: окно игры потеряло активность во время захвата", methodCtx)
	}
	if after.Handle != before.Handle {
		return fmt.Errorf("%s: выбранное окно игры изменилось во время захвата", methodCtx)
	}
	if after.ClientArea != before.ClientArea {
		return fmt.Errorf("%s: клиентская область окна игры изменилась во время захвата", methodCtx)
	}
	return nil
}

// visuallyBlank rejects black/protected-surface frames while tolerating dark
// game screens. A single border or cursor cannot make a frame valid.
func visuallyBlank(data []byte) bool {
	if len(data) == 0 || len(data) > maxEncodedFrameBytes {
		return true
	}
	config, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || validateCaptureDimensions(PixelRectangle{
		Width: config.Width, Height: config.Height,
	}) != nil {
		return true
	}
	source, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return true
	}
	bounds := source.Bounds()
	if bounds.Dx() == 0 || bounds.Dy() == 0 {
		return true
	}
	stepX := max(1, bounds.Dx()/160)
	stepY := max(1, bounds.Dy()/120)
	var (
		count     int
		sum       float64
		sumSquare float64
		histogram [16]int
	)
	for y := bounds.Min.Y; y < bounds.Max.Y; y += stepY {
		for x := bounds.Min.X; x < bounds.Max.X; x += stepX {
			red, green, blue, _ := source.At(x, y).RGBA()
			luma := .299*float64(red>>8) + .587*float64(green>>8) + .114*float64(blue>>8)
			sum += luma
			sumSquare += luma * luma
			histogram[min(15, int(luma)/16)]++
			count++
		}
	}
	if count == 0 {
		return true
	}
	mean := sum / float64(count)
	variance := math.Max(0, sumSquare/float64(count)-mean*mean)
	dominant := 0
	for _, value := range histogram {
		dominant = max(dominant, value)
	}
	return math.Sqrt(variance) < 4 || float64(dominant)/float64(count) > .992
}

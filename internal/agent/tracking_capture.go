package agent

import (
	"context"
	"fmt"
	"sync/atomic"

	"github.com/arena-trading-agent/arena-trading-agent/internal/domain"
	"github.com/arena-trading-agent/arena-trading-agent/internal/protocol"
)

// TrackingCapture records the ID of every successfully captured frame. It is
// the single source used for BasedOnFrame stale-command validation.
type TrackingCapture struct {
	driver CaptureDriver
	latest atomic.Uint64
}

// NewTrackingCapture wraps a platform capture driver.
func NewTrackingCapture(driver CaptureDriver) *TrackingCapture {
	return &TrackingCapture{driver: driver}
}

// Capture obtains and records a full frame.
func (capture *TrackingCapture) Capture(ctx context.Context) (protocol.Frame, error) {
	const methodCtx = "agent.TrackingCapture.Capture"

	if capture == nil || capture.driver == nil {
		return protocol.Frame{}, fmt.Errorf("%s: драйвер захвата не настроен", methodCtx)
	}
	frame, err := capture.driver.Capture(ctx)
	if err != nil {
		return protocol.Frame{}, fmt.Errorf("%s: захват кадра завершился ошибкой: %w", methodCtx, err)
	}
	capture.latest.Store(frame.ID)
	return frame, nil
}

// CaptureRegion obtains and records a region frame.
func (capture *TrackingCapture) CaptureRegion(ctx context.Context, region domain.Rectangle) (protocol.Frame, error) {
	const methodCtx = "agent.TrackingCapture.CaptureRegion"

	if capture == nil || capture.driver == nil {
		return protocol.Frame{}, fmt.Errorf("%s: драйвер захвата не настроен", methodCtx)
	}
	frame, err := capture.driver.CaptureRegion(ctx, region)
	if err != nil {
		return protocol.Frame{}, fmt.Errorf("%s: захват области кадра завершился ошибкой: %w", methodCtx, err)
	}
	capture.latest.Store(frame.ID)
	return frame, nil
}

// LatestFrame returns the newest successfully captured frame ID.
func (capture *TrackingCapture) LatestFrame() uint64 {
	if capture == nil {
		return 0
	}
	return capture.latest.Load()
}

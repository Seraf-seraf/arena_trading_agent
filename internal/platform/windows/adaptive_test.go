package winplatform

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/png"
	"strings"
	"testing"
	"time"

	"github.com/arena-trading-agent/arena-trading-agent/internal/domain"
	"github.com/arena-trading-agent/arena-trading-agent/internal/protocol"
)

type captureDriverMock struct {
	frame       protocol.Frame
	err         error
	captureCall int
	regionCall  int
}

func (driver *captureDriverMock) Capture(context.Context) (protocol.Frame, error) {
	driver.captureCall++
	return driver.frame, driver.err
}

func (driver *captureDriverMock) CaptureRegion(
	context.Context,
	domain.Rectangle,
) (protocol.Frame, error) {
	driver.regionCall++
	return driver.frame, driver.err
}

func TestAdaptiveCaptureWithoutFallbackRejectsPrimaryError(t *testing.T) {
	t.Parallel()

	primary := &captureDriverMock{err: errors.New("источник недоступен")}
	capture := newAdaptiveCapture(primary, nil)
	initialID := capture.frameID.Load()

	frame, err := capture.Capture(context.Background())
	if err == nil {
		t.Fatal("ошибка единственного источника захвата принята")
	}
	if !strings.Contains(err.Error(), "winplatform.AdaptiveCapture.capture") ||
		!strings.Contains(err.Error(), "основной драйвер захвата завершился ошибкой") {
		t.Fatalf("неожиданная диагностика: %v", err)
	}
	if frame.ID != 0 || capture.frameID.Load() != initialID {
		t.Fatalf("ошибочный захват получил ID: frame=%d sequence=%d", frame.ID, capture.frameID.Load())
	}
	if primary.captureCall != 1 || primary.regionCall != 0 {
		t.Fatalf("неожиданные вызовы драйвера: full=%d region=%d", primary.captureCall, primary.regionCall)
	}
}

func TestAdaptiveCaptureWithoutFallbackRejectsBlankPrimary(t *testing.T) {
	t.Parallel()

	blank := image.NewNRGBA(image.Rect(0, 0, 320, 200))
	primary := &captureDriverMock{frame: protocol.Frame{Data: encodePNG(t, blank)}}
	capture := newAdaptiveCapture(primary, nil)
	initialID := capture.frameID.Load()

	frame, err := capture.Capture(context.Background())
	if err == nil {
		t.Fatal("визуально пустой кадр единственного источника принят")
	}
	if !strings.Contains(err.Error(), "winplatform.AdaptiveCapture.capture") ||
		!strings.Contains(err.Error(), "основной драйвер захвата вернул визуально пустой кадр") {
		t.Fatalf("неожиданная диагностика: %v", err)
	}
	if frame.ID != 0 || capture.frameID.Load() != initialID {
		t.Fatalf("пустой кадр получил ID: frame=%d sequence=%d", frame.ID, capture.frameID.Load())
	}
}

func TestAdaptiveCaptureWithoutFallbackPreservesValidPrimary(t *testing.T) {
	t.Parallel()

	data := detailedDarkPNG(t)
	capturedAt := time.Date(2026, time.July, 31, 1, 2, 3, 0, time.UTC)
	region := domain.Rectangle{X: .1, Y: .2, Width: .3, Height: .4}
	primary := &captureDriverMock{frame: protocol.Frame{
		CapturedAt: capturedAt,
		Region:     region,
		Encoding:   "png",
		Data:       data,
	}}
	capture := newAdaptiveCapture(primary, nil)

	frame, err := capture.Capture(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if frame.ID == 0 {
		t.Fatal("валидному кадру не назначен ID")
	}
	if !frame.CapturedAt.Equal(capturedAt) || frame.Region != region ||
		frame.Encoding != "png" || !bytes.Equal(frame.Data, data) {
		t.Fatalf("поля исходного кадра изменены: %+v", frame)
	}
}

func TestValidateDesktopCaptureSnapshot(t *testing.T) {
	t.Parallel()

	before := WindowSnapshot{
		Handle:     42,
		Active:     true,
		ClientArea: PixelRectangle{Left: 10, Top: 20, Width: 1920, Height: 1080},
	}
	tests := []struct {
		name       string
		after      WindowSnapshot
		wantReason string
	}{
		{
			name:  "unchanged active snapshot",
			after: before,
		},
		{
			name: "minimized after capture",
			after: WindowSnapshot{
				Handle:     before.Handle,
				Active:     true,
				Minimized:  true,
				ClientArea: before.ClientArea,
			},
			wantReason: "было свёрнуто",
		},
		{
			name: "inactive after capture",
			after: WindowSnapshot{
				Handle:     before.Handle,
				ClientArea: before.ClientArea,
			},
			wantReason: "потеряло активность",
		},
		{
			name: "changed handle",
			after: WindowSnapshot{
				Handle:     before.Handle + 1,
				Active:     true,
				ClientArea: before.ClientArea,
			},
			wantReason: "выбранное окно игры изменилось",
		},
		{
			name: "changed client area",
			after: WindowSnapshot{
				Handle: before.Handle,
				Active: true,
				ClientArea: PixelRectangle{
					Left:   before.ClientArea.Left,
					Top:    before.ClientArea.Top,
					Width:  before.ClientArea.Width - 1,
					Height: before.ClientArea.Height,
				},
			},
			wantReason: "клиентская область окна игры изменилась",
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			err := validateDesktopCaptureSnapshot(before, test.after)
			if test.wantReason == "" {
				if err != nil {
					t.Fatalf("неизменившийся снимок отклонён: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("изменившийся снимок окна принят")
			}
			if !strings.Contains(err.Error(), "winplatform.validateDesktopCaptureSnapshot") ||
				!strings.Contains(err.Error(), test.wantReason) {
				t.Fatalf("неожиданная диагностика: %v", err)
			}
		})
	}
}

func TestVisuallyBlankRejectsProtectedSurface(t *testing.T) {
	frame := image.NewNRGBA(image.Rect(0, 0, 320, 200))
	for x := 10; x < 310; x++ {
		frame.Set(x, 190, color.RGBA{R: 150, G: 220, B: 255, A: 255})
	}
	if !visuallyBlank(encodePNG(t, frame)) {
		t.Fatal("mostly black protected surface should be rejected")
	}
}

func TestVisuallyBlankAcceptsDetailedDarkFrame(t *testing.T) {
	if visuallyBlank(detailedDarkPNG(t)) {
		t.Fatal("detailed dark frame should be accepted")
	}
}

func detailedDarkPNG(t *testing.T) []byte {
	t.Helper()
	frame := image.NewGray(image.Rect(0, 0, 320, 200))
	for y := 0; y < 200; y++ {
		for x := 0; x < 320; x++ {
			frame.SetGray(x, y, color.Gray{Y: uint8((x*7 + y*3) % 80)})
		}
	}
	return encodePNG(t, frame)
}

func encodePNG(t *testing.T, source image.Image) []byte {
	t.Helper()
	var output bytes.Buffer
	if err := png.Encode(&output, source); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

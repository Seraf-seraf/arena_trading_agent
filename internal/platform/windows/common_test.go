package winplatform

import (
	"math"
	"reflect"
	"testing"

	"github.com/arena-trading-agent/arena-trading-agent/internal/domain"
)

func TestRegionPixelsCoversNormalizedRegion(t *testing.T) {
	got, err := regionPixels(domain.Rectangle{X: 0.1, Y: 0.2, Width: 0.3, Height: 0.4}, 101, 51)
	if err != nil {
		t.Fatal(err)
	}
	want := PixelRectangle{Left: 10, Top: 10, Width: 31, Height: 21}
	if got != want {
		t.Fatalf("regionPixels() = %+v, want %+v", got, want)
	}
}

func TestRegionPixelsRejectsUnsafeValues(t *testing.T) {
	tests := []domain.Rectangle{
		{X: -0.1, Width: 0.5, Height: 0.5},
		{X: 0.8, Width: 0.3, Height: 0.5},
		{Width: 0, Height: 0.5},
		{Width: math.NaN(), Height: 0.5},
	}
	for _, region := range tests {
		if _, err := regionPixels(region, 1920, 1080); err == nil {
			t.Fatalf("regionPixels(%+v) unexpectedly succeeded", region)
		}
	}
}

func TestValidateCaptureDimensionsBoundsMemory(t *testing.T) {
	t.Parallel()
	for _, pixels := range []PixelRectangle{
		{Width: 0, Height: 1080},
		{Width: 9000, Height: 1},
		{Width: 4096, Height: 2161},
	} {
		if err := validateCaptureDimensions(pixels); err == nil {
			t.Fatalf("размер %+v должен быть отклонён", pixels)
		}
	}
	if err := validateCaptureDimensions(PixelRectangle{Width: 4096, Height: 2160}); err != nil {
		t.Fatalf("допустимый кадр 4K отклонён: %v", err)
	}
}

func TestClientPointIncludesBottomRightPixel(t *testing.T) {
	x, y, err := clientPoint(domain.Point{X: 1, Y: 1}, 1920, 1080)
	if err != nil {
		t.Fatal(err)
	}
	if x != 1919 || y != 1079 {
		t.Fatalf("clientPoint() = (%d, %d), want (1919, 1079)", x, y)
	}
}

func TestParseKeyChord(t *testing.T) {
	got, err := parseKeyChord("ctrl + shift + F12")
	if err != nil {
		t.Fatal(err)
	}
	want := []uint16{0x11, 0x10, 0x7b}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseKeyChord() = %#v, want %#v", got, want)
	}
}

func TestMatchesWindowUsesBasenameAndCaseInsensitiveTitle(t *testing.T) {
	criteria := WindowCriteria{ProcessName: "Arena.exe", TitleContains: "arena breakout"}
	if !matchesWindow(criteria, "Arena Breakout: Infinite", `C:\Games\ARENA.exe`) {
		t.Fatal("expected the window to match")
	}
	if matchesWindow(criteria, "Arena Breakout: Infinite", `C:\Games\launcher.exe`) {
		t.Fatal("unexpected process match")
	}
}

func TestTickDistanceHandlesGetTickCountWraparound(t *testing.T) {
	if got := tickDistance(math.MaxUint32-4, 3); got != 8 {
		t.Fatalf("tickDistance() = %d, want 8", got)
	}
}

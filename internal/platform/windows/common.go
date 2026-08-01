package winplatform

import (
	"fmt"
	"math"
	"path"
	"strings"

	"github.com/arena-trading-agent/arena-trading-agent/internal/domain"
)

const (
	maxCaptureDimension  = 8192
	maxCapturePixels     = 4096 * 2160
	maxEncodedFrameBytes = 24 << 20
)

var virtualKeys = map[string]uint16{
	"BACKSPACE":  0x08,
	"TAB":        0x09,
	"ENTER":      0x0d,
	"RETURN":     0x0d,
	"SHIFT":      0x10,
	"CTRL":       0x11,
	"CONTROL":    0x11,
	"ALT":        0x12,
	"MENU":       0x12,
	"PAUSE":      0x13,
	"CAPSLOCK":   0x14,
	"ESC":        0x1b,
	"ESCAPE":     0x1b,
	"SPACE":      0x20,
	"PAGEUP":     0x21,
	"PAGEDOWN":   0x22,
	"END":        0x23,
	"HOME":       0x24,
	"LEFT":       0x25,
	"UP":         0x26,
	"RIGHT":      0x27,
	"DOWN":       0x28,
	"INSERT":     0x2d,
	"DELETE":     0x2e,
	"LWIN":       0x5b,
	"RWIN":       0x5c,
	"MULTIPLY":   0x6a,
	"ADD":        0x6b,
	"SUBTRACT":   0x6d,
	"DECIMAL":    0x6e,
	"DIVIDE":     0x6f,
	"NUMLOCK":    0x90,
	"SCROLLLOCK": 0x91,
	"OEM_PLUS":   0xbb,
	"OEM_MINUS":  0xbd,
}

func init() {
	for key := uint16('0'); key <= uint16('9'); key++ {
		virtualKeys[string(rune(key))] = key
	}
	for key := uint16('A'); key <= uint16('Z'); key++ {
		virtualKeys[string(rune(key))] = key
	}
	for number := 0; number <= 9; number++ {
		virtualKeys[fmt.Sprintf("NUMPAD%d", number)] = uint16(0x60 + number)
	}
	for number := 1; number <= 24; number++ {
		virtualKeys[fmt.Sprintf("F%d", number)] = uint16(0x6f + number)
	}
}

func regionPixels(region domain.Rectangle, width, height int) (PixelRectangle, error) {
	const methodCtx = "winplatform.regionPixels"

	if width <= 0 || height <= 0 {
		return PixelRectangle{}, fmt.Errorf("%s: некорректная клиентская область %dx%d", methodCtx, width, height)
	}
	values := []float64{region.X, region.Y, region.Width, region.Height}
	for _, value := range values {
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return PixelRectangle{}, fmt.Errorf("%s: область захвата содержит неконечную координату", methodCtx)
		}
	}
	if region.X < 0 || region.Y < 0 || region.Width <= 0 || region.Height <= 0 ||
		region.X+region.Width > 1 || region.Y+region.Height > 1 {
		return PixelRectangle{}, fmt.Errorf("%s: область захвата должна находиться внутри нормализованной клиентской области", methodCtx)
	}

	left := int(math.Floor(region.X * float64(width)))
	top := int(math.Floor(region.Y * float64(height)))
	right := int(math.Ceil((region.X + region.Width) * float64(width)))
	bottom := int(math.Ceil((region.Y + region.Height) * float64(height)))
	left = clamp(left, 0, width-1)
	top = clamp(top, 0, height-1)
	right = clamp(right, left+1, width)
	bottom = clamp(bottom, top+1, height)
	return PixelRectangle{Left: left, Top: top, Width: right - left, Height: bottom - top}, nil
}

func validateCaptureDimensions(pixels PixelRectangle) error {
	const methodCtx = "winplatform.validateCaptureDimensions"

	if pixels.Width <= 0 || pixels.Height <= 0 {
		return fmt.Errorf(
			"%s: размеры захвата должны быть положительными, получено %dx%d",
			methodCtx,
			pixels.Width,
			pixels.Height,
		)
	}
	if pixels.Width > maxCaptureDimension || pixels.Height > maxCaptureDimension {
		return fmt.Errorf(
			"%s: размер захвата %dx%d превышает предел измерения %d",
			methodCtx,
			pixels.Width,
			pixels.Height,
			maxCaptureDimension,
		)
	}
	if pixels.Width > maxCapturePixels/pixels.Height {
		return fmt.Errorf(
			"%s: кадр %dx%d превышает предел %d пикселей",
			methodCtx,
			pixels.Width,
			pixels.Height,
			maxCapturePixels,
		)
	}
	return nil
}

func clientPoint(point domain.Point, width, height int) (int, int, error) {
	const methodCtx = "winplatform.clientPoint"

	if width <= 0 || height <= 0 {
		return 0, 0, fmt.Errorf("%s: некорректная клиентская область %dx%d", methodCtx, width, height)
	}
	if math.IsNaN(point.X) || math.IsNaN(point.Y) ||
		math.IsInf(point.X, 0) || math.IsInf(point.Y, 0) ||
		point.X < 0 || point.X > 1 || point.Y < 0 || point.Y > 1 {
		return 0, 0, fmt.Errorf("%s: точка должна находиться внутри нормализованной клиентской области", methodCtx)
	}
	x := int(math.Round(point.X * float64(width-1)))
	y := int(math.Round(point.Y * float64(height-1)))
	return clamp(x, 0, width-1), clamp(y, 0, height-1), nil
}

func parseKeyChord(value string) ([]uint16, error) {
	const methodCtx = "winplatform.parseKeyChord"

	parts := strings.Split(value, "+")
	if len(parts) == 0 {
		return nil, fmt.Errorf("%s: клавиша не задана", methodCtx)
	}
	keys := make([]uint16, 0, len(parts))
	for _, part := range parts {
		name := strings.ToUpper(strings.TrimSpace(part))
		if name == "" {
			return nil, fmt.Errorf("%s: сочетание клавиш %q содержит пустую клавишу", methodCtx, value)
		}
		key, ok := virtualKeys[name]
		if !ok {
			return nil, fmt.Errorf("%s: неподдерживаемая клавиша %q", methodCtx, part)
		}
		keys = append(keys, key)
	}
	return keys, nil
}

func matchesWindow(criteria WindowCriteria, title, processName string) bool {
	if strings.TrimSpace(criteria.ProcessName) == "" && strings.TrimSpace(criteria.TitleContains) == "" {
		return false
	}
	if expected := normalizeExecutable(criteria.ProcessName); expected != "" {
		if normalizeExecutable(processName) != expected {
			return false
		}
	}
	if expected := strings.TrimSpace(criteria.TitleContains); expected != "" {
		if !strings.Contains(strings.ToLower(title), strings.ToLower(expected)) {
			return false
		}
	}
	return true
}

func normalizeExecutable(value string) string {
	value = strings.TrimSpace(strings.ReplaceAll(value, `\`, "/"))
	if value == "" {
		return ""
	}
	return strings.ToLower(path.Base(value))
}

func tickDistance(left, right uint32) uint32 {
	forward := left - right
	backward := right - left
	if forward < backward {
		return forward
	}
	return backward
}

func clamp(value, minimum, maximum int) int {
	if value < minimum {
		return minimum
	}
	if value > maximum {
		return maximum
	}
	return value
}

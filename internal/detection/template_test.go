package detection_test

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"testing"

	"github.com/arena-trading-agent/arena-trading-agent/internal/detection"
	"github.com/arena-trading-agent/arena-trading-agent/internal/domain"
	"github.com/arena-trading-agent/arena-trading-agent/internal/protocol"
)

func TestMatcherRecognizesCalibratedImage(t *testing.T) {
	source := gradientImage()
	hash := detection.HashRegion(source, domain.Rectangle{Width: 1, Height: 1})
	matcher, err := detection.New(detection.Config{Screens: []detection.Screen{{
		State: domain.StateMainMenu,
		Anchors: []detection.Anchor{{
			Region: domain.Rectangle{Width: 1, Height: 1}, Hash: hash, MaxDistance: 0,
		}},
		Regions: map[string]domain.Rectangle{
			"balance": {X: .8, Y: 0, Width: .2, Height: .1},
		},
	}}}, ".")
	if err != nil {
		t.Fatal(err)
	}
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, source); err != nil {
		t.Fatal(err)
	}
	state, confidence, regions, err := matcher.Match(context.Background(), protocol.Frame{
		ID: 1, Encoding: "png", Data: encoded.Bytes(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if state != domain.StateMainMenu || confidence != 1 || regions["balance"].Width != .2 {
		t.Fatalf(
			"неожиданный результат сопоставления: состояние=%s уверенность=%f области=%+v",
			state,
			confidence,
			regions,
		)
	}
}

func gradientImage() image.Image {
	result := image.NewGray(image.Rect(0, 0, 90, 80))
	for y := 0; y < 80; y++ {
		for x := 0; x < 90; x++ {
			result.SetGray(x, y, color.Gray{Y: uint8((x*3 + y) % 255)})
		}
	}
	return result
}

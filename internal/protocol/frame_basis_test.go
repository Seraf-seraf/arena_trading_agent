package protocol_test

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"strings"
	"testing"
	"time"

	"github.com/arena-trading-agent/arena-trading-agent/internal/domain"
	"github.com/arena-trading-agent/arena-trading-agent/internal/protocol"
)

func TestFrameRegionBasisIgnoresPixelsOutsideROI(t *testing.T) {
	t.Parallel()

	region := domain.Rectangle{X: 0, Y: 0, Width: .5, Height: 1}
	source := basisFrame(t, 1, func(frame *image.NRGBA) {
		fillBasisImage(frame, color.NRGBA{R: 10, G: 20, B: 30, A: 255})
	})
	basis, err := protocol.BuildFrameRegionBasis(source, []domain.Rectangle{region})
	if err != nil {
		t.Fatalf("BuildFrameRegionBasis() error = %v", err)
	}
	changedOutside := basisFrame(t, 2, func(frame *image.NRGBA) {
		fillBasisImage(frame, color.NRGBA{R: 10, G: 20, B: 30, A: 255})
		frame.SetNRGBA(3, 1, color.NRGBA{R: 250, A: 255})
	})
	if err := protocol.VerifyFrameRegionBasis(changedOutside, basis); err != nil {
		t.Fatalf("пиксель вне ROI ошибочно изменил основание: %v", err)
	}
}

func TestFrameRegionBasisRejectsPixelInsideROI(t *testing.T) {
	t.Parallel()

	region := domain.Rectangle{X: 0, Y: 0, Width: .5, Height: 1}
	source := basisFrame(t, 1, func(frame *image.NRGBA) {
		fillBasisImage(frame, color.NRGBA{R: 10, G: 20, B: 30, A: 255})
	})
	basis, err := protocol.BuildFrameRegionBasis(source, []domain.Rectangle{region})
	if err != nil {
		t.Fatalf("BuildFrameRegionBasis() error = %v", err)
	}
	changedInside := basisFrame(t, 2, func(frame *image.NRGBA) {
		fillBasisImage(frame, color.NRGBA{R: 10, G: 20, B: 30, A: 255})
		frame.SetNRGBA(1, 1, color.NRGBA{G: 250, A: 255})
	})
	err = protocol.VerifyFrameRegionBasis(changedInside, basis)
	if err == nil || !strings.Contains(err.Error(), "пиксели области 1 изменились") {
		t.Fatalf("изменение внутри ROI не обнаружено: %v", err)
	}
}

func TestFrameRegionBasisRejectsMalformedAndExcessiveRegions(t *testing.T) {
	t.Parallel()

	validDigest := protocol.ComputeFrameDigest([]byte("roi"))
	tests := []struct {
		name  string
		basis []protocol.FrameRegionDigest
	}{
		{
			name: "область выходит за кадр",
			basis: []protocol.FrameRegionDigest{{
				Region: domain.Rectangle{X: .8, Y: 0, Width: .3, Height: .5},
				Digest: validDigest,
			}},
		},
		{
			name: "невалидный digest",
			basis: []protocol.FrameRegionDigest{{
				Region: domain.Rectangle{Width: .5, Height: .5},
				Digest: "не-sha256",
			}},
		},
		{
			name: "повтор области",
			basis: []protocol.FrameRegionDigest{
				{
					Region: domain.Rectangle{Width: .5, Height: .5},
					Digest: validDigest,
				},
				{
					Region: domain.Rectangle{Width: .5, Height: .5},
					Digest: validDigest,
				},
			},
		},
		{
			name: "чрезмерная суммарная площадь",
			basis: []protocol.FrameRegionDigest{
				{
					Region: domain.Rectangle{Width: .6, Height: 1},
					Digest: validDigest,
				},
				{
					Region: domain.Rectangle{Width: 1, Height: 1},
					Digest: validDigest,
				},
			},
		},
		{
			name:  "слишком много областей",
			basis: make([]protocol.FrameRegionDigest, protocol.MaxFrameBasisRegions+1),
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if err := protocol.ValidateFrameRegionBasis(test.basis); err == nil {
				t.Fatal("некорректное ROI-основание принято")
			}
		})
	}
}

func basisFrame(
	t *testing.T,
	id uint64,
	change func(*image.NRGBA),
) protocol.Frame {
	t.Helper()

	frame := image.NewNRGBA(image.Rect(0, 0, 4, 3))
	change(frame)
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, frame); err != nil {
		t.Fatalf("png.Encode() error = %v", err)
	}
	data := encoded.Bytes()
	return protocol.Frame{
		ID:            id,
		CapturedAt:    time.Now().UTC(),
		ContentDigest: protocol.ComputeFrameDigest(data),
		Region:        domain.Rectangle{Width: 1, Height: 1},
		Encoding:      "png",
		Data:          data,
	}
}

func fillBasisImage(frame *image.NRGBA, value color.NRGBA) {
	for y := frame.Bounds().Min.Y; y < frame.Bounds().Max.Y; y++ {
		for x := frame.Bounds().Min.X; x < frame.Bounds().Max.X; x++ {
			frame.SetNRGBA(x, y, value)
		}
	}
}

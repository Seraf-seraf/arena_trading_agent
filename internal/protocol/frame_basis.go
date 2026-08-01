package protocol

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"hash"
	"image"
	"image/color"
	"image/png"
	"math"
	"sort"
	"strings"

	"github.com/arena-trading-agent/arena-trading-agent/internal/domain"
)

const (
	maxFrameBasisDecodedPixels = 16 << 20
	maxFrameBasisDimension     = 16_384
	maxFrameBasisTotalArea     = 1.5
	frameRegionDigestVersion   = "arena-frame-roi-v1\x00"
)

// FrameRegionDigest связывает нормализованную ROI исходного полного кадра с
// каноническим SHA-256 digest её декодированных пикселей.
type FrameRegionDigest struct {
	Region domain.Rectangle `json:"region"`
	Digest string           `json:"digest"`
}

// BuildFrameRegionBasis декодирует PNG ровно один раз и строит
// детерминированный, отсортированный список pixel-digest для уникальных ROI.
func BuildFrameRegionBasis(
	frame Frame,
	regions []domain.Rectangle,
) ([]FrameRegionDigest, error) {
	const methodCtx = "protocol.BuildFrameRegionBasis"

	if len(regions) == 0 {
		return nil, nil
	}
	if len(regions) > MaxFrameBasisRegions {
		return nil, fmt.Errorf(
			"%s: число областей %d превышает лимит %d",
			methodCtx,
			len(regions),
			MaxFrameBasisRegions,
		)
	}
	regions = append([]domain.Rectangle(nil), regions...)
	for index := range regions {
		regions[index] = canonicalRectangle(regions[index])
		if err := validateBasisRectangle(regions[index]); err != nil {
			return nil, fmt.Errorf("%s: область %d некорректна: %w", methodCtx, index+1, err)
		}
	}
	sort.Slice(regions, func(first, second int) bool {
		return compareRectangle(regions[first], regions[second]) < 0
	})
	unique := regions[:0]
	for _, region := range regions {
		if len(unique) > 0 && compareRectangle(unique[len(unique)-1], region) == 0 {
			continue
		}
		unique = append(unique, region)
	}
	if len(unique) > MaxFrameBasisRegions {
		return nil, fmt.Errorf(
			"%s: число уникальных областей %d превышает лимит %d",
			methodCtx,
			len(unique),
			MaxFrameBasisRegions,
		)
	}
	if err := validateTotalBasisArea(unique); err != nil {
		return nil, fmt.Errorf("%s: суммарная область основания некорректна: %w", methodCtx, err)
	}
	decoded, err := decodeBasisFrame(frame)
	if err != nil {
		return nil, fmt.Errorf("%s: не удалось декодировать исходный кадр: %w", methodCtx, err)
	}
	basis := make([]FrameRegionDigest, 0, len(unique))
	for index, region := range unique {
		digest, digestErr := digestImageRegion(decoded, region)
		if digestErr != nil {
			return nil, fmt.Errorf(
				"%s: не удалось вычислить digest области %d: %w",
				methodCtx,
				index+1,
				digestErr,
			)
		}
		basis = append(basis, FrameRegionDigest{Region: region, Digest: digest})
	}
	return basis, nil
}

// ValidateFrameRegionBasis отклоняет неканонический, чрезмерный или
// неоднозначный список ROI до декодирования изображения.
func ValidateFrameRegionBasis(basis []FrameRegionDigest) error {
	const methodCtx = "protocol.ValidateFrameRegionBasis"

	if len(basis) > MaxFrameBasisRegions {
		return fmt.Errorf(
			"%s: число областей %d превышает лимит %d",
			methodCtx,
			len(basis),
			MaxFrameBasisRegions,
		)
	}
	regions := make([]domain.Rectangle, 0, len(basis))
	for index, item := range basis {
		if err := validateBasisRectangle(item.Region); err != nil {
			return fmt.Errorf("%s: область %d некорректна: %w", methodCtx, index+1, err)
		}
		if canonicalRectangle(item.Region) != item.Region {
			return fmt.Errorf("%s: область %d записана неканонически", methodCtx, index+1)
		}
		if !ValidFrameDigest(item.Digest) {
			return fmt.Errorf("%s: digest области %d имеет некорректный формат", methodCtx, index+1)
		}
		if index > 0 && compareRectangle(basis[index-1].Region, item.Region) >= 0 {
			return fmt.Errorf(
				"%s: области должны быть строго отсортированы и не содержать повторов",
				methodCtx,
			)
		}
		regions = append(regions, item.Region)
	}
	if err := validateTotalBasisArea(regions); err != nil {
		return fmt.Errorf("%s: суммарная область основания некорректна: %w", methodCtx, err)
	}
	return nil
}

// VerifyFrameRegionBasis повторно вычисляет digest всех ROI свежего полного
// кадра. Изменения вне этих областей намеренно не влияют на результат.
func VerifyFrameRegionBasis(frame Frame, basis []FrameRegionDigest) error {
	const methodCtx = "protocol.VerifyFrameRegionBasis"

	if len(basis) == 0 {
		return fmt.Errorf("%s: список областей основания пуст", methodCtx)
	}
	if err := ValidateFrameRegionBasis(basis); err != nil {
		return fmt.Errorf("%s: основание кадра не прошло проверку: %w", methodCtx, err)
	}
	regions := make([]domain.Rectangle, len(basis))
	for index := range basis {
		regions[index] = basis[index].Region
	}
	actual, err := BuildFrameRegionBasis(frame, regions)
	if err != nil {
		return fmt.Errorf("%s: не удалось пересчитать основание свежего кадра: %w", methodCtx, err)
	}
	for index := range basis {
		if actual[index].Digest != basis[index].Digest {
			return fmt.Errorf(
				"%s: пиксели области %d изменились после исходного наблюдения",
				methodCtx,
				index+1,
			)
		}
	}
	return nil
}

func decodeBasisFrame(frame Frame) (image.Image, error) {
	const methodCtx = "protocol.decodeBasisFrame"

	frame, err := NormalizeFrameDigest(frame)
	if err != nil {
		return nil, fmt.Errorf("%s: кадр не прошёл проверку целостности: %w", methodCtx, err)
	}
	if !fullFrameRegion(frame.Region) {
		return nil, fmt.Errorf("%s: ROI-основание требует полный кадр клиентской области", methodCtx)
	}
	switch strings.ToLower(strings.TrimSpace(frame.Encoding)) {
	case "png", "image/png":
	default:
		return nil, fmt.Errorf("%s: ROI-основание поддерживает только PNG, получено %q", methodCtx, frame.Encoding)
	}
	config, err := png.DecodeConfig(bytes.NewReader(frame.Data))
	if err != nil {
		return nil, fmt.Errorf("%s: не удалось прочитать заголовок PNG: %w", methodCtx, err)
	}
	if config.Width <= 0 || config.Height <= 0 ||
		config.Width > maxFrameBasisDimension ||
		config.Height > maxFrameBasisDimension ||
		config.Width > maxFrameBasisDecodedPixels/config.Height {
		return nil, fmt.Errorf(
			"%s: декодированный кадр %dx%d превышает лимит %d пикселей",
			methodCtx,
			config.Width,
			config.Height,
			maxFrameBasisDecodedPixels,
		)
	}
	decoded, err := png.Decode(bytes.NewReader(frame.Data))
	if err != nil {
		return nil, fmt.Errorf("%s: не удалось декодировать PNG: %w", methodCtx, err)
	}
	if decoded.Bounds().Dx() != config.Width || decoded.Bounds().Dy() != config.Height {
		return nil, fmt.Errorf("%s: размеры декодированного PNG изменились", methodCtx)
	}
	return decoded, nil
}

func digestImageRegion(frame image.Image, region domain.Rectangle) (string, error) {
	const methodCtx = "protocol.digestImageRegion"

	bounds := frame.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	left := int(math.Floor(region.X * float64(width)))
	top := int(math.Floor(region.Y * float64(height)))
	right := int(math.Ceil((region.X + region.Width) * float64(width)))
	bottom := int(math.Ceil((region.Y + region.Height) * float64(height)))
	left, top = max(0, left), max(0, top)
	right, bottom = min(width, right), min(height, bottom)
	if left >= right || top >= bottom {
		return "", fmt.Errorf("%s: область не содержит ни одного пикселя", methodCtx)
	}
	digest := sha256.New()
	_, _ = digest.Write([]byte(frameRegionDigestVersion))
	writeDigestUint32(digest, uint32(width))
	writeDigestUint32(digest, uint32(height))
	writeDigestUint32(digest, uint32(left))
	writeDigestUint32(digest, uint32(top))
	writeDigestUint32(digest, uint32(right))
	writeDigestUint32(digest, uint32(bottom))
	row := make([]byte, (right-left)*4)
	for y := top; y < bottom; y++ {
		offset := 0
		for x := left; x < right; x++ {
			pixel := color.NRGBAModel.Convert(frame.At(bounds.Min.X+x, bounds.Min.Y+y)).(color.NRGBA)
			row[offset], row[offset+1], row[offset+2], row[offset+3] =
				pixel.R, pixel.G, pixel.B, pixel.A
			offset += 4
		}
		_, _ = digest.Write(row)
	}
	return base64.RawURLEncoding.EncodeToString(digest.Sum(nil)), nil
}

func writeDigestUint32(destination hash.Hash, value uint32) {
	var data [4]byte
	binary.BigEndian.PutUint32(data[:], value)
	_, _ = destination.Write(data[:])
}

func validateBasisRectangle(region domain.Rectangle) error {
	const methodCtx = "protocol.validateBasisRectangle"

	values := []float64{region.X, region.Y, region.Width, region.Height}
	for _, value := range values {
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return fmt.Errorf("%s: координаты области должны быть конечными", methodCtx)
		}
	}
	if math.Signbit(region.X) && region.X == 0 ||
		math.Signbit(region.Y) && region.Y == 0 ||
		math.Signbit(region.Width) && region.Width == 0 ||
		math.Signbit(region.Height) && region.Height == 0 {
		return fmt.Errorf("%s: отрицательный ноль не является канонической координатой", methodCtx)
	}
	if region.X < 0 || region.Y < 0 || region.Width <= 0 || region.Height <= 0 ||
		region.X+region.Width > 1 || region.Y+region.Height > 1 {
		return fmt.Errorf("%s: область выходит за нормализованную клиентскую область", methodCtx)
	}
	return nil
}

func validateTotalBasisArea(regions []domain.Rectangle) error {
	const methodCtx = "protocol.validateTotalBasisArea"

	total := 0.0
	for _, region := range regions {
		total += region.Width * region.Height
	}
	if math.IsNaN(total) || math.IsInf(total, 0) || total > maxFrameBasisTotalArea {
		return fmt.Errorf(
			"%s: суммарная нормализованная площадь %.3f превышает лимит %.3f",
			methodCtx,
			total,
			maxFrameBasisTotalArea,
		)
	}
	return nil
}

func canonicalRectangle(region domain.Rectangle) domain.Rectangle {
	if region.X == 0 {
		region.X = 0
	}
	if region.Y == 0 {
		region.Y = 0
	}
	if region.Width == 0 {
		region.Width = 0
	}
	if region.Height == 0 {
		region.Height = 0
	}
	return region
}

func compareRectangle(first, second domain.Rectangle) int {
	firstValues := [...]float64{first.X, first.Y, first.Width, first.Height}
	secondValues := [...]float64{second.X, second.Y, second.Width, second.Height}
	for index := range firstValues {
		switch {
		case firstValues[index] < secondValues[index]:
			return -1
		case firstValues[index] > secondValues[index]:
			return 1
		}
	}
	return 0
}

func fullFrameRegion(region domain.Rectangle) bool {
	return region.X == 0 && region.Y == 0 && region.Width == 1 && region.Height == 1
}

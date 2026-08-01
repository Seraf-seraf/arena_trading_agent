// Package detection implements deterministic recognition of calibrated game
// screens. It deliberately contains no model calls.
package detection

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"math/bits"
	"os"
	"path/filepath"

	"github.com/arena-trading-agent/arena-trading-agent/internal/domain"
	"github.com/arena-trading-agent/arena-trading-agent/internal/protocol"
)

// Config is a serializable set of calibrated screen fingerprints and OCR ROIs.
type Config struct {
	Screens []Screen `json:"screens"`
}

// Screen describes one known UI state.
type Screen struct {
	State         domain.ScreenState          `json:"state"`
	MinConfidence float64                     `json:"min_confidence"`
	Anchors       []Anchor                    `json:"anchors"`
	Regions       map[string]domain.Rectangle `json:"regions,omitempty"`
}

// Anchor is a perceptual dHash of a stable normalized screen region.
type Anchor struct {
	Region      domain.Rectangle `json:"region"`
	Hash        string           `json:"hash,omitempty"`
	Template    string           `json:"template,omitempty"`
	MaxDistance int              `json:"max_distance"`
}

// Matcher recognizes known screens using calibrated anchors.
type Matcher struct {
	screens []compiledScreen
}

type compiledScreen struct {
	state         domain.ScreenState
	minConfidence float64
	anchors       []compiledAnchor
	regions       map[string]domain.Rectangle
}

type compiledAnchor struct {
	region      domain.Rectangle
	hash        uint64
	maxDistance int
}

// Load reads a detector config and resolves relative template image paths
// against the config directory.
func Load(path string) (*Matcher, error) {
	const methodCtx = "detection.Load"

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf(
			"%s: не удалось прочитать конфигурацию детектора %q: %w",
			methodCtx,
			path,
			err,
		)
	}
	var config Config
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf(
			"%s: не удалось разобрать конфигурацию детектора %q: %w",
			methodCtx,
			path,
			err,
		)
	}
	matcher, err := New(config, filepath.Dir(path))
	if err != nil {
		return nil, fmt.Errorf("%s: некорректная конфигурация детектора %q: %w", methodCtx, path, err)
	}
	return matcher, nil
}

// New compiles detector configuration. baseDir is used for template files.
func New(config Config, baseDir string) (*Matcher, error) {
	const methodCtx = "detection.New"

	matcher := &Matcher{screens: make([]compiledScreen, 0, len(config.Screens))}
	seen := make(map[domain.ScreenState]bool)
	for _, screen := range config.Screens {
		if screen.State == "" || screen.State == domain.StateUnknown || !isKnownState(screen.State) {
			return nil, fmt.Errorf("%s: некорректное состояние детектора %q", methodCtx, screen.State)
		}
		if seen[screen.State] {
			return nil, fmt.Errorf("%s: состояние %s задано несколько раз", methodCtx, screen.State)
		}
		seen[screen.State] = true
		if len(screen.Anchors) == 0 {
			return nil, fmt.Errorf(
				"%s: состояние %s не содержит опорных областей в поле anchors",
				methodCtx,
				screen.State,
			)
		}
		minConfidence := screen.MinConfidence
		if minConfidence == 0 {
			minConfidence = .85
		}
		if minConfidence < 0 || minConfidence > 1 {
			return nil, fmt.Errorf(
				"%s: состояние %s содержит некорректное значение поля min_confidence",
				methodCtx,
				screen.State,
			)
		}
		compiled := compiledScreen{
			state: screen.State, minConfidence: minConfidence,
			regions: cloneRegions(screen.Regions),
		}
		for _, anchor := range screen.Anchors {
			if !validRegion(anchor.Region) {
				return nil, fmt.Errorf(
					"%s: состояние %s содержит некорректную опорную область",
					methodCtx,
					screen.State,
				)
			}
			if anchor.MaxDistance < 0 || anchor.MaxDistance > 64 {
				return nil, fmt.Errorf(
					"%s: состояние %s содержит некорректное значение поля max_distance",
					methodCtx,
					screen.State,
				)
			}
			hash, err := resolveHash(anchor, baseDir)
			if err != nil {
				return nil, fmt.Errorf(
					"%s: не удалось подготовить опорную область состояния %s: %w",
					methodCtx,
					screen.State,
					err,
				)
			}
			compiled.anchors = append(compiled.anchors, compiledAnchor{
				region: anchor.Region, hash: hash, maxDistance: anchor.MaxDistance,
			})
		}
		for name, region := range compiled.regions {
			if name == "" || !validRegion(region) {
				return nil, fmt.Errorf(
					"%s: состояние %s содержит некорректную область распознавания текста %q",
					methodCtx,
					screen.State,
					name,
				)
			}
		}
		matcher.screens = append(matcher.screens, compiled)
	}
	return matcher, nil
}

// Match decodes a frame and returns the best calibrated screen.
func (m *Matcher) Match(_ context.Context, frame protocol.Frame) (domain.ScreenState, float64, map[string]domain.Rectangle, error) {
	const methodCtx = "detection.Matcher.Match"

	if len(frame.Data) == 0 {
		return domain.StateUnknown, 0, nil, fmt.Errorf("%s: кадр %d пуст", methodCtx, frame.ID)
	}
	source, _, err := image.Decode(bytes.NewReader(frame.Data))
	if err != nil {
		return domain.StateUnknown, 0, nil, fmt.Errorf(
			"%s: не удалось декодировать кадр %d: %w",
			methodCtx,
			frame.ID,
			err,
		)
	}
	bestState := domain.StateUnknown
	bestConfidence := 0.0
	var bestRegions map[string]domain.Rectangle
	for _, screen := range m.screens {
		var confidenceSum float64
		matched := true
		for _, anchor := range screen.anchors {
			actual := differenceHash(source, anchor.region)
			distance := bits.OnesCount64(actual ^ anchor.hash)
			if distance > anchor.maxDistance {
				matched = false
				break
			}
			confidenceSum += 1 - float64(distance)/64
		}
		if !matched {
			continue
		}
		confidence := confidenceSum / float64(len(screen.anchors))
		if confidence >= screen.minConfidence && confidence > bestConfidence {
			bestState, bestConfidence = screen.state, confidence
			bestRegions = cloneRegions(screen.regions)
		}
	}
	return bestState, bestConfidence, bestRegions, nil
}

// LocalDetector adapts Matcher to observation.LocalDetector.
type LocalDetector struct{ Matcher *Matcher }

// Detect returns the state and configured OCR ROIs.
func (d LocalDetector) Detect(ctx context.Context, frame protocol.Frame) (domain.ScreenState, float64, map[string]domain.Rectangle, error) {
	const methodCtx = "detection.LocalDetector.Detect"

	if d.Matcher == nil {
		return domain.StateUnknown, 0, nil, nil
	}
	state, confidence, regions, err := d.Matcher.Match(ctx, frame)
	if err != nil {
		return domain.StateUnknown, 0, nil, fmt.Errorf(
			"%s: не удалось сопоставить кадр с известным экраном: %w",
			methodCtx,
			err,
		)
	}
	return state, confidence, regions, nil
}

// AgentStateDetector adapts Matcher to agent.StateDetector.
type AgentStateDetector struct{ Matcher *Matcher }

// Detect returns only the state information needed for action verification.
func (d AgentStateDetector) Detect(ctx context.Context, frame protocol.Frame) (domain.ScreenState, float64, error) {
	const methodCtx = "detection.AgentStateDetector.Detect"

	if d.Matcher == nil {
		return domain.StateUnknown, 0, nil
	}
	state, confidence, _, err := d.Matcher.Match(ctx, frame)
	if err != nil {
		return domain.StateUnknown, 0, fmt.Errorf(
			"%s: не удалось сопоставить кадр с известным экраном: %w",
			methodCtx,
			err,
		)
	}
	return state, confidence, nil
}

// HashRegion calculates a stable hex fingerprint used by calibration tooling.
func HashRegion(source image.Image, region domain.Rectangle) string {
	value := differenceHash(source, region)
	buffer := make([]byte, 8)
	for index := range buffer {
		buffer[index] = byte(value >> (56 - 8*index))
	}
	return hex.EncodeToString(buffer)
}

func resolveHash(anchor Anchor, baseDir string) (uint64, error) {
	const methodCtx = "detection.resolveHash"

	if anchor.Hash != "" && anchor.Template != "" {
		return 0, fmt.Errorf(
			"%s: укажите только хеш в поле hash или шаблон в поле template",
			methodCtx,
		)
	}
	if anchor.Hash != "" {
		value, err := hex.DecodeString(anchor.Hash)
		if err != nil || len(value) != 8 {
			if err != nil {
				return 0, fmt.Errorf(
					"%s: поле hash должно содержать 16 шестнадцатеричных символов: %w",
					methodCtx,
					err,
				)
			}
			return 0, fmt.Errorf(
				"%s: поле hash должно содержать 16 шестнадцатеричных символов",
				methodCtx,
			)
		}
		var result uint64
		for _, part := range value {
			result = result<<8 | uint64(part)
		}
		return result, nil
	}
	if anchor.Template == "" {
		return 0, fmt.Errorf(
			"%s: отсутствует хеш в поле hash или шаблон в поле template",
			methodCtx,
		)
	}
	path := anchor.Template
	if !filepath.IsAbs(path) {
		path = filepath.Join(baseDir, path)
	}
	file, err := os.Open(path)
	if err != nil {
		return 0, fmt.Errorf("%s: не удалось открыть файл шаблона %q: %w", methodCtx, path, err)
	}
	defer file.Close()
	source, _, err := image.Decode(file)
	if err != nil {
		return 0, fmt.Errorf(
			"%s: не удалось декодировать файл шаблона %q: %w",
			methodCtx,
			path,
			err,
		)
	}
	return differenceHash(source, anchor.Region), nil
}

func differenceHash(source image.Image, region domain.Rectangle) uint64 {
	bounds := source.Bounds()
	left := bounds.Min.X + int(region.X*float64(bounds.Dx()))
	top := bounds.Min.Y + int(region.Y*float64(bounds.Dy()))
	width := max(1, int(region.Width*float64(bounds.Dx())))
	height := max(1, int(region.Height*float64(bounds.Dy())))
	var result uint64
	for y := 0; y < 8; y++ {
		sampleY := top + min(height-1, (2*y+1)*height/16)
		for x := 0; x < 8; x++ {
			firstX := left + min(width-1, (2*x+1)*width/18)
			secondX := left + min(width-1, (2*x+3)*width/18)
			result <<= 1
			if luminance(source.At(firstX, sampleY)) > luminance(source.At(secondX, sampleY)) {
				result |= 1
			}
		}
	}
	return result
}

func luminance(colorValue interface{ RGBA() (r, g, b, a uint32) }) uint64 {
	red, green, blue, _ := colorValue.RGBA()
	return 299*uint64(red) + 587*uint64(green) + 114*uint64(blue)
}

func validRegion(region domain.Rectangle) bool {
	return region.X >= 0 && region.Y >= 0 && region.Width > 0 && region.Height > 0 &&
		region.X+region.Width <= 1 && region.Y+region.Height <= 1
}

func cloneRegions(input map[string]domain.Rectangle) map[string]domain.Rectangle {
	if input == nil {
		return nil
	}
	result := make(map[string]domain.Rectangle, len(input))
	for key, value := range input {
		result[key] = value
	}
	return result
}

func isKnownState(state domain.ScreenState) bool {
	switch state {
	case domain.StateMainMenu, domain.StateMarketHome, domain.StateMarketSearch,
		domain.StateMarketResults, domain.StateItemCard, domain.StatePurchaseDialog,
		domain.StateContacts, domain.StateContactPage, domain.StateContactBarter,
		domain.StateBarterCard, domain.StateInventory, domain.StateSaleDialog,
		domain.StateConfirmation, domain.StateErrorDialog:
		return true
	default:
		return false
	}
}

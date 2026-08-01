package observation_test

import (
	"context"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/arena-trading-agent/arena-trading-agent/internal/domain"
	"github.com/arena-trading-agent/arena-trading-agent/internal/observation"
	"github.com/arena-trading-agent/arena-trading-agent/internal/protocol"
)

type localDetectorFunc func(
	context.Context,
	protocol.Frame,
) (domain.ScreenState, float64, map[string]domain.Rectangle, error)

func (function localDetectorFunc) Detect(
	ctx context.Context,
	frame protocol.Frame,
) (domain.ScreenState, float64, map[string]domain.Rectangle, error) {
	return function(ctx, frame)
}

type ocrServiceFunc func(
	context.Context,
	protocol.Frame,
	map[string]domain.Rectangle,
) (map[string]domain.Value, error)

func (function ocrServiceFunc) Read(
	ctx context.Context,
	frame protocol.Frame,
	regions map[string]domain.Rectangle,
) (map[string]domain.Value, error) {
	return function(ctx, frame, regions)
}

type vlmServiceFunc func(context.Context, protocol.Frame) (domain.Observation, error)

func (function vlmServiceFunc) Ground(
	ctx context.Context,
	frame protocol.Frame,
) (domain.Observation, error) {
	return function(ctx, frame)
}

func TestObserverAcceptsValidatedKnownScreen(t *testing.T) {
	region := domain.Rectangle{X: .1, Y: .2, Width: .3, Height: .1}
	pipeline := observation.New(
		localDetectorFunc(func(
			context.Context,
			protocol.Frame,
		) (domain.ScreenState, float64, map[string]domain.Rectangle, error) {
			return domain.StateMarketResults, .95, map[string]domain.Rectangle{"price": region}, nil
		}),
		ocrServiceFunc(func(
			_ context.Context,
			_ protocol.Frame,
			regions map[string]domain.Rectangle,
		) (map[string]domain.Value, error) {
			if regions["price"] != region {
				t.Fatalf("неожиданные области OCR: %+v", regions)
			}
			return map[string]domain.Value{
				"price": {
					Raw: "18 430", Normalized: "18430", Source: "OCR",
					Confidence: .98, Region: region,
				},
			}, nil
		}),
		nil,
	)

	result, err := pipeline.Observe(context.Background(), protocol.Frame{
		ID: 17, CapturedAt: time.Now().UTC(), Encoding: "png", Data: []byte{1},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.FrameID != 17 || result.State != domain.StateMarketResults ||
		result.Values["price"].Normalized != "18430" || result.CreatedAt.IsZero() {
		t.Fatalf("неожиданное наблюдение: %+v", result)
	}
}

func TestObserverRejectsMalformedDetectorRegionBeforeOCR(t *testing.T) {
	ocrCalled := false
	pipeline := observation.New(
		localDetectorFunc(func(
			context.Context,
			protocol.Frame,
		) (domain.ScreenState, float64, map[string]domain.Rectangle, error) {
			return domain.StateMarketResults, .95, map[string]domain.Rectangle{
				"price": {X: math.NaN(), Y: .2, Width: .3, Height: .1},
			}, nil
		}),
		ocrServiceFunc(func(
			context.Context,
			protocol.Frame,
			map[string]domain.Rectangle,
		) (map[string]domain.Value, error) {
			ocrCalled = true
			return nil, nil
		}),
		nil,
	)

	_, err := pipeline.Observe(context.Background(), protocol.Frame{ID: 1, Data: []byte{1}})
	if err == nil || !strings.Contains(err.Error(), "observation.Observer.Observe") {
		t.Fatalf("ожидалась контекстная ошибка области OCR, получено: %v", err)
	}
	if ocrCalled {
		t.Fatal("OCR не должен вызываться для некорректной области")
	}
}

func TestObserverRejectsInvalidVLMObservation(t *testing.T) {
	pipeline := observation.New(
		localDetectorFunc(func(
			context.Context,
			protocol.Frame,
		) (domain.ScreenState, float64, map[string]domain.Rectangle, error) {
			return domain.StateUnknown, 0, nil, nil
		}),
		nil,
		vlmServiceFunc(func(context.Context, protocol.Frame) (domain.Observation, error) {
			return domain.Observation{
				State:      domain.StateMainMenu,
				Confidence: math.Inf(1),
			}, nil
		}),
	)

	_, err := pipeline.Observe(context.Background(), protocol.Frame{ID: 2, Data: []byte{1}})
	if err == nil || !strings.Contains(err.Error(), "observation.validate") {
		t.Fatalf("ожидалась ошибка проверки VLM-наблюдения, получено: %v", err)
	}
}

func TestObserverCapsVLMObservationBelowInputThreshold(t *testing.T) {
	region := domain.Rectangle{X: .1, Y: .1, Width: .2, Height: .1}
	pipeline := observation.New(
		localDetectorFunc(func(
			context.Context,
			protocol.Frame,
		) (domain.ScreenState, float64, map[string]domain.Rectangle, error) {
			return domain.StateUnknown, 0, nil, nil
		}),
		nil,
		vlmServiceFunc(func(context.Context, protocol.Frame) (domain.Observation, error) {
			return domain.Observation{
				State:      domain.StatePurchaseDialog,
				Confidence: .99,
				Values: map[string]domain.Value{
					"purchase_price": {
						Raw: "10", Normalized: "10", Source: "OCR",
						Confidence: .99, Region: region,
					},
				},
			}, nil
		}),
	)

	result, err := pipeline.Observe(
		context.Background(),
		protocol.Frame{ID: 3, Data: []byte{1}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Confidence > .5 ||
		result.Values["purchase_price"].Source != "VLM" {
		t.Fatalf("VLM-наблюдение может авторизовать ввод: %+v", result)
	}
}

func TestObserverRejectsMissingDependenciesWithoutPanic(t *testing.T) {
	var pipeline *observation.Observer
	_, err := pipeline.Observe(context.Background(), protocol.Frame{ID: 1, Data: []byte{1}})
	if err == nil || !strings.Contains(err.Error(), "локальный детектор") {
		t.Fatalf("ожидалась ошибка зависимости, получено: %v", err)
	}

	pipeline = observation.New(
		localDetectorFunc(func(
			context.Context,
			protocol.Frame,
		) (domain.ScreenState, float64, map[string]domain.Rectangle, error) {
			return domain.StateUnknown, 0, nil, nil
		}),
		nil,
		nil,
	)
	_, err = pipeline.Observe(context.Background(), protocol.Frame{ID: 2, Data: []byte{1}})
	if err == nil || !strings.Contains(err.Error(), "VLM") {
		t.Fatalf("ожидалась ошибка зависимости VLM, получено: %v", err)
	}
}

func TestObserverNormalizesUnanimousCriticalOCRConsensus(t *testing.T) {
	region := domain.Rectangle{X: .1, Y: .2, Width: .3, Height: .1}
	samples := []domain.Value{
		{Raw: "18 430", Normalized: "18 430", Source: "OCR", Confidence: .96, Region: region},
		{Raw: "18430", Normalized: "18430", Source: "OCR", Confidence: .91, Region: region},
		{Raw: "18.430", Source: "OCR", Confidence: .99, Region: region},
	}
	calls := 0
	pipeline := observation.New(
		fixedDetector(domain.StateMarketResults, map[string]domain.Rectangle{"purchase_price": region}),
		ocrServiceFunc(func(
			context.Context,
			protocol.Frame,
			map[string]domain.Rectangle,
		) (map[string]domain.Value, error) {
			value := samples[calls]
			calls++
			return map[string]domain.Value{"purchase_price": value}, nil
		}),
		nil,
	)

	result, err := pipeline.Observe(
		context.Background(),
		protocol.Frame{ID: 8, Data: []byte{1}},
	)
	if err != nil {
		t.Fatal(err)
	}
	value := result.Values["purchase_price"]
	if calls != 3 {
		t.Fatalf("OCR вызван %d раз вместо 3", calls)
	}
	if value.Normalized != "18430" {
		t.Fatalf("нормализованное значение равно %q вместо 18430", value.Normalized)
	}
	if math.Abs(value.Confidence-.91) > 1e-9 {
		t.Fatalf("уверенность consensus равна %.6f вместо 0.91", value.Confidence)
	}
}

func TestObserverRequiresConsensusForMarketOrderIdentity(t *testing.T) {
	t.Parallel()

	region := domain.Rectangle{X: .1, Y: .2, Width: .3, Height: .1}
	raw := []string{"ORDER-42", " order-42 ", "ORDER-43"}
	calls := 0
	pipeline := observation.New(
		fixedDetector(
			domain.StateMarketHome,
			map[string]domain.Rectangle{"market_order_id": region},
		),
		ocrServiceFunc(func(
			context.Context,
			protocol.Frame,
			map[string]domain.Rectangle,
		) (map[string]domain.Value, error) {
			value := domain.Value{
				Raw: raw[calls], Source: "OCR", Confidence: .99, Region: region,
			}
			calls++
			return map[string]domain.Value{"market_order_id": value}, nil
		}),
		nil,
	)

	result, err := pipeline.Observe(
		context.Background(),
		protocol.Frame{ID: 81, Data: []byte{1}},
	)
	if err != nil {
		t.Fatal(err)
	}
	value := result.Values["market_order_id"]
	if calls != 3 || value.Normalized != "ORDER-42" ||
		value.Source != "OCR_CONSENSUS" {
		t.Fatalf("идентификатор ордера не прошёл consensus: calls=%d value=%+v", calls, value)
	}
}

func TestObserverRequiresConsensusForVisibleTradeIdentities(t *testing.T) {
	t.Parallel()

	region := domain.Rectangle{X: .1, Y: .2, Width: .3, Height: .1}
	regions := map[string]domain.Rectangle{
		"item_name":         region,
		"contact_name":      region,
		"result_item_name":  region,
		"ingredient.a.name": region,
	}
	calls := 0
	pipeline := observation.New(
		fixedDetector(domain.StateBarterCard, regions),
		ocrServiceFunc(func(
			context.Context,
			protocol.Frame,
			map[string]domain.Rectangle,
		) (map[string]domain.Value, error) {
			calls++
			values := make(map[string]domain.Value, len(regions))
			for name := range regions {
				values[name] = domain.Value{
					Raw: name, Source: "OCR", Confidence: .99, Region: region,
				}
			}
			return values, nil
		}),
		nil,
	)

	result, err := pipeline.Observe(
		context.Background(),
		protocol.Frame{ID: 82, Data: []byte{1}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 3 {
		t.Fatalf("видимые идентичности прочитаны %d раз вместо 3", calls)
	}
	for name := range regions {
		if result.Values[name].Source != "OCR_CONSENSUS" {
			t.Fatalf("идентичность %q не прошла consensus: %+v", name, result.Values[name])
		}
	}
}

func TestObserverPenalizesCriticalOCRMajority(t *testing.T) {
	region := domain.Rectangle{X: .1, Y: .2, Width: .3, Height: .1}
	samples := []domain.Value{
		{Raw: "18430", Source: "OCR", Confidence: .99, Region: region},
		{Raw: "18 430", Source: "OCR", Confidence: .90, Region: region},
		{Raw: "19430", Source: "OCR", Confidence: .98, Region: region},
	}
	calls := 0
	pipeline := observation.New(
		fixedDetector(domain.StatePurchaseDialog, map[string]domain.Rectangle{"price": region}),
		ocrServiceFunc(func(
			context.Context,
			protocol.Frame,
			map[string]domain.Rectangle,
		) (map[string]domain.Value, error) {
			value := samples[calls]
			calls++
			return map[string]domain.Value{"price": value}, nil
		}),
		nil,
	)

	result, err := pipeline.Observe(
		context.Background(),
		protocol.Frame{ID: 9, Data: []byte{1}},
	)
	if err != nil {
		t.Fatal(err)
	}
	value := result.Values["price"]
	if value.Normalized != "18430" {
		t.Fatalf("большинство выбрало неожиданное значение: %+v", value)
	}
	if math.Abs(value.Confidence-.60) > 1e-9 {
		t.Fatalf("расхождение не снизило уверенность до 0.60: %.6f", value.Confidence)
	}
}

func TestObserverFailsClosedWhenCriticalOCRHasNoMajority(t *testing.T) {
	region := domain.Rectangle{X: .1, Y: .2, Width: .3, Height: .1}
	raw := []string{"18430", "19430", "20430"}
	calls := 0
	pipeline := observation.New(
		fixedDetector(domain.StatePurchaseDialog, map[string]domain.Rectangle{"balance": region}),
		ocrServiceFunc(func(
			context.Context,
			protocol.Frame,
			map[string]domain.Rectangle,
		) (map[string]domain.Value, error) {
			value := domain.Value{
				Raw: raw[calls], Source: "OCR", Confidence: .99, Region: region,
			}
			calls++
			return map[string]domain.Value{"balance": value}, nil
		}),
		nil,
	)

	_, err := pipeline.Observe(
		context.Background(),
		protocol.Frame{ID: 10, Data: []byte{1}},
	)
	if err == nil || !strings.Contains(err.Error(), `критичное значение "balance"`) {
		t.Fatalf("ожидалась fail-closed ошибка consensus, получено: %v", err)
	}
	if calls != 3 {
		t.Fatalf("OCR вызван %d раз вместо жёстко ограниченных 3", calls)
	}
}

func TestObserverStrictConsensusRejectsAnyDisagreement(t *testing.T) {
	region := domain.Rectangle{X: .1, Y: .2, Width: .3, Height: .1}
	raw := []string{"18430", "18430", "19430"}
	calls := 0
	pipeline, err := observation.NewWithOCRConsensusPolicy(
		fixedDetector(domain.StatePurchaseDialog, map[string]domain.Rectangle{"price": region}),
		ocrServiceFunc(func(
			context.Context,
			protocol.Frame,
			map[string]domain.Rectangle,
		) (map[string]domain.Value, error) {
			value := domain.Value{
				Raw: raw[calls], Source: "OCR", Confidence: .99, Region: region,
			}
			calls++
			return map[string]domain.Value{"price": value}, nil
		}),
		nil,
		observation.OCRConsensusPolicy{
			Attempts: 3, MinimumAgreement: 2, FailOnDisagreement: true,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	_, err = pipeline.Observe(
		context.Background(),
		protocol.Frame{ID: 11, Data: []byte{1}},
	)
	if err == nil || !strings.Contains(err.Error(), "строгая политика") {
		t.Fatalf("ожидался отказ строгой политики, получено: %v", err)
	}
}

func TestObserverReadsNonCriticalOCRValueOnce(t *testing.T) {
	region := domain.Rectangle{X: .1, Y: .2, Width: .3, Height: .1}
	calls := 0
	pipeline := observation.New(
		fixedDetector(domain.StateItemCard, map[string]domain.Rectangle{"item_description": region}),
		ocrServiceFunc(func(
			context.Context,
			protocol.Frame,
			map[string]domain.Rectangle,
		) (map[string]domain.Value, error) {
			calls++
			return map[string]domain.Value{
				"item_description": {
					Raw: "Фильтр", Source: "OCR", Confidence: .95, Region: region,
				},
			}, nil
		}),
		nil,
	)

	if _, err := pipeline.Observe(
		context.Background(),
		protocol.Frame{ID: 12, Data: []byte{1}},
	); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("некритичное значение прочитано %d раз вместо одного", calls)
	}
}

func TestOCRConsensusPolicyRejectsMoreThanThreeAttempts(t *testing.T) {
	err := (observation.OCRConsensusPolicy{
		Attempts: 4, MinimumAgreement: 2,
	}).Validate()
	if err == nil || !strings.Contains(err.Error(), "1..3") {
		t.Fatalf("ожидалась ошибка жёсткого лимита попыток, получено: %v", err)
	}
}

func fixedDetector(
	state domain.ScreenState,
	regions map[string]domain.Rectangle,
) localDetectorFunc {
	return func(
		context.Context,
		protocol.Frame,
	) (domain.ScreenState, float64, map[string]domain.Rectangle, error) {
		return state, .99, regions, nil
	}
}

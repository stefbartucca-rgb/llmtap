package telemetry_test

import (
	"context"
	"sort"
	"testing"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/stefbartucca-rgb/llmtap/internal/telemetry"
	"github.com/stefbartucca-rgb/llmtap/internal/wire"
)

// TestHistogramBucketsResolveRealisticValues guards against the SDK default
// buckets (0, 5, 10, 25, ... 10000), which are tuned for milliseconds. With
// durations in seconds every ordinary call fell into the first bucket, and the
// dashboard quantiles became fixed fractions of 5s. No unit test noticed; only
// running the stack did.
//
// The assertion is deliberately about behaviour rather than the exact list:
// values that a dashboard needs to tell apart must land in different buckets.
func TestHistogramBucketsResolveRealisticValues(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	tp := sdktrace.NewTracerProvider()

	rec, err := telemetry.NewRecorder(tp, mp, nil)
	if err != nil {
		t.Fatalf("NewRecorder: %v", err)
	}

	_, call := rec.StartCall(context.Background(), telemetry.CallOptions{
		Provider:     wire.ProviderOpenAI,
		Operation:    wire.OperationChat,
		RequestModel: "gpt-5",
	})
	call.FirstChunk()
	call.End(wire.CallInfo{UsageKnown: true, InputTokens: 1200, OutputTokens: 300}, 200, nil)

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}

	cases := []struct {
		metric      string
		distinguish [][2]float64 // pairs that must fall into different buckets
		atLeast     float64      // the largest finite bound must reach this
	}{
		{
			metric:      "gen_ai.client.operation.duration",
			distinguish: [][2]float64{{0.05, 0.3}, {0.3, 1.5}, {1.5, 8}, {8, 30}},
			atLeast:     60,
		},
		{
			metric:      "gen_ai.client.operation.time_to_first_chunk",
			distinguish: [][2]float64{{0.05, 0.3}, {0.3, 1.5}, {1.5, 8}},
			atLeast:     30,
		},
		{
			metric:      "gen_ai.client.token.usage",
			distinguish: [][2]float64{{50, 800}, {800, 8000}, {8000, 100000}},
			atLeast:     1_000_000,
		},
	}

	for _, tc := range cases {
		t.Run(tc.metric, func(t *testing.T) {
			bounds := histogramBounds(t, rm, tc.metric)
			if len(bounds) == 0 {
				t.Fatal("histogram has no finite bucket bounds")
			}
			if top := bounds[len(bounds)-1]; top < tc.atLeast {
				t.Errorf("largest bound is %v, want at least %v", top, tc.atLeast)
			}
			for _, pair := range tc.distinguish {
				if bucketOf(bounds, pair[0]) == bucketOf(bounds, pair[1]) {
					t.Errorf("%v and %v land in the same bucket; bounds %v", pair[0], pair[1], bounds)
				}
			}
		})
	}
}

func histogramBounds(t *testing.T, rm metricdata.ResourceMetrics, name string) []float64 {
	t.Helper()
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			switch data := m.Data.(type) {
			case metricdata.Histogram[float64]:
				if len(data.DataPoints) > 0 {
					return data.DataPoints[0].Bounds
				}
			case metricdata.Histogram[int64]:
				if len(data.DataPoints) > 0 {
					return data.DataPoints[0].Bounds
				}
			}
			t.Fatalf("%s is not a histogram with data points (%T)", name, m.Data)
		}
	}
	t.Fatalf("metric %s was not recorded", name)
	return nil
}

// bucketOf returns the index of the bucket a value falls into, using the
// Prometheus convention that a bucket includes its upper bound.
func bucketOf(bounds []float64, v float64) int {
	return sort.SearchFloat64s(bounds, v)
}

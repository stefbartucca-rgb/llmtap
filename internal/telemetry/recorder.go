package telemetry

import (
	"context"
	"strconv"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/stefbartucca-rgb/llmtap/internal/wire"
)

// Pricer converts token counts into a dollar amount. The pricing package
// implements it; declaring the interface here keeps telemetry free of a
// dependency on a price table it does not otherwise care about.
type Pricer interface {
	// Cost reports the price of a call, and false if the model is unknown.
	Cost(provider, model string, inputTokens, cachedInputTokens, outputTokens int64) (float64, bool)
}

// Recorder turns observed calls into spans and metrics.
type Recorder struct {
	tracer trace.Tracer
	pricer Pricer

	tokenUsage metric.Int64Histogram
	duration   metric.Float64Histogram
	ttfc       metric.Float64Histogram
	cost       metric.Float64Counter
}

const instrumentationName = "github.com/stefbartucca-rgb/llmtap"

// NewRecorder builds a Recorder. The providers are passed in rather than read
// from the global ones so that a test can hand it an in-memory exporter and
// assert on exactly what was emitted. A nil pricer means no cost metric.
func NewRecorder(tp trace.TracerProvider, mp metric.MeterProvider, pricer Pricer) (*Recorder, error) {
	meter := mp.Meter(instrumentationName)

	tokenUsage, err := meter.Int64Histogram(metricTokenUsage,
		metric.WithUnit("{token}"),
		metric.WithDescription("Number of input and output tokens used."))
	if err != nil {
		return nil, err
	}
	duration, err := meter.Float64Histogram(metricOpDuration,
		metric.WithUnit("s"),
		metric.WithDescription("Duration of a GenAI client operation."))
	if err != nil {
		return nil, err
	}
	ttfc, err := meter.Float64Histogram(metricTimeToFirst,
		metric.WithUnit("s"),
		metric.WithDescription("Time to receive the first chunk of a streamed response."))
	if err != nil {
		return nil, err
	}
	cost, err := meter.Float64Counter(metricCostUSD,
		metric.WithUnit("{USD}"),
		metric.WithDescription("Estimated spend, derived from token counts and a local price table."))
	if err != nil {
		return nil, err
	}

	return &Recorder{
		tracer:     tp.Tracer(instrumentationName),
		pricer:     pricer,
		tokenUsage: tokenUsage,
		duration:   duration,
		ttfc:       ttfc,
		cost:       cost,
	}, nil
}

// CallOptions is what is known when a call starts.
type CallOptions struct {
	Provider      wire.Provider
	Operation     wire.Operation
	RequestModel  string
	ServerAddress string
	ServerPort    int
}

// Call is one in-flight model call.
type Call struct {
	rec  *Recorder
	span trace.Span
	ctx  context.Context

	opts  CallOptions
	start time.Time

	firstChunkOnce sync.Once
	endOnce        sync.Once
}

// StartCall opens the span. The returned context carries it, so the metrics
// recorded at the end land as exemplars pointing back at this trace — that is
// what makes a Grafana panel clickable through to the actual call.
func (r *Recorder) StartCall(ctx context.Context, opts CallOptions) (context.Context, *Call) {
	// The conventions define the span name as "{operation} {request model}".
	name := string(opts.Operation)
	if opts.RequestModel != "" {
		name += " " + opts.RequestModel
	}

	attrs := []attribute.KeyValue{
		attribute.String(attrOperationName, string(opts.Operation)),
		attribute.String(attrProviderName, string(opts.Provider)),
	}
	if opts.RequestModel != "" {
		attrs = append(attrs, attribute.String(attrRequestModel, opts.RequestModel))
	}
	if opts.ServerAddress != "" {
		attrs = append(attrs, attribute.String(attrServerAddress, opts.ServerAddress))
		if opts.ServerPort != 0 {
			attrs = append(attrs, attribute.Int(attrServerPort, opts.ServerPort))
		}
	}

	// SpanKindClient: as far as the model API is concerned, llmtap is the
	// client making the call.
	ctx, span := r.tracer.Start(ctx, name,
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(attrs...))

	return ctx, &Call{rec: r, span: span, ctx: ctx, opts: opts, start: time.Now()}
}

// FirstChunk marks the arrival of the first streamed byte. Repeat calls are
// ignored, so the proxy can call it on every read without checking.
func (c *Call) FirstChunk() {
	c.firstChunkOnce.Do(func() {
		c.rec.ttfc.Record(c.ctx, time.Since(c.start).Seconds(),
			metric.WithAttributes(c.metricAttrs("")...))
	})
}

// End closes the span and emits the metrics. statusCode is the upstream HTTP
// status, or 0 if the request never got that far; err is a transport-level
// failure.
func (c *Call) End(info wire.CallInfo, statusCode int, err error) {
	c.endOnce.Do(func() {
		defer c.span.End()

		errorType := classifyError(statusCode, err)
		c.setSpanAttributes(info, errorType, err)

		attrs := c.metricAttrs(errorType)
		c.rec.duration.Record(c.ctx, time.Since(c.start).Seconds(),
			metric.WithAttributes(attrs...))

		// No usage means no usage. The conventions are explicit that
		// instrumentation must not report token counts it could not obtain,
		// and a fabricated zero in a cost dashboard is worse than a gap.
		if !info.UsageKnown {
			return
		}

		c.rec.tokenUsage.Record(c.ctx, info.InputTokens,
			metric.WithAttributes(append(attrs, attribute.String(attrTokenType, tokenTypeInput))...))
		c.rec.tokenUsage.Record(c.ctx, info.OutputTokens,
			metric.WithAttributes(append(attrs, attribute.String(attrTokenType, tokenTypeOutput))...))

		c.recordCost(info, attrs)
	})
}

func (c *Call) recordCost(info wire.CallInfo, attrs []attribute.KeyValue) {
	if c.rec.pricer == nil {
		return
	}
	model := info.ResponseModel
	if model == "" {
		model = c.opts.RequestModel
	}

	usd, ok := c.rec.pricer.Cost(string(c.opts.Provider), model,
		info.InputTokens, info.CachedInputTokens, info.OutputTokens)
	if !ok {
		return // unpriced model; the proxy logs this once per model
	}
	c.rec.cost.Add(c.ctx, usd, metric.WithAttributes(attrs...))
}

func (c *Call) setSpanAttributes(info wire.CallInfo, errorType string, err error) {
	attrs := make([]attribute.KeyValue, 0, 6)
	if info.ResponseModel != "" {
		attrs = append(attrs, attribute.String(attrResponseModel, info.ResponseModel))
	}
	if info.ResponseID != "" {
		attrs = append(attrs, attribute.String(attrResponseID, info.ResponseID))
	}
	if len(info.FinishReasons) > 0 {
		attrs = append(attrs, attribute.StringSlice(attrFinishReasons, info.FinishReasons))
	}
	if info.UsageKnown {
		attrs = append(attrs,
			attribute.Int64(attrInputTokens, info.InputTokens),
			attribute.Int64(attrOutputTokens, info.OutputTokens))
	}
	if errorType != "" {
		attrs = append(attrs, attribute.String(attrErrorType, errorType))
	}
	c.span.SetAttributes(attrs...)

	if errorType == "" {
		return
	}
	// The span status carries no message when the failure is only an HTTP
	// status: the code is already an attribute, and upstream error bodies can
	// echo back prompt text that llmtap deliberately does not record.
	if err != nil {
		c.span.SetStatus(codes.Error, err.Error())
		return
	}
	c.span.SetStatus(codes.Error, "")
}

// metricAttrs is the attribute set shared by every instrument.
//
// Server address and port are intentionally left out: they are constant per
// deployment and would only add series without adding information.
func (c *Call) metricAttrs(errorType string) []attribute.KeyValue {
	attrs := make([]attribute.KeyValue, 0, 4)
	attrs = append(attrs,
		attribute.String(attrOperationName, string(c.opts.Operation)),
		attribute.String(attrProviderName, string(c.opts.Provider)))
	if c.opts.RequestModel != "" {
		attrs = append(attrs, attribute.String(attrRequestModel, c.opts.RequestModel))
	}
	if errorType != "" {
		attrs = append(attrs, attribute.String(attrErrorType, errorType))
	}
	return attrs
}

// classifyError maps an outcome onto the error.type convention: the HTTP
// status code for a rejected call, a catch-all for anything else.
func classifyError(statusCode int, err error) string {
	switch {
	case err != nil:
		return errorTypeOther
	case statusCode >= 400:
		return strconv.Itoa(statusCode)
	default:
		return ""
	}
}

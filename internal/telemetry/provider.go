// Package telemetry owns everything OpenTelemetry: SDK wiring, and turning a
// normalized wire.CallInfo into spans and metrics.
package telemetry

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// Config describes where telemetry goes. A zero Endpoint falls back to the
// standard OTEL_EXPORTER_OTLP_* environment variables, so llmtap behaves like
// any other OTLP producer when dropped into an existing setup.
type Config struct {
	ServiceName    string
	ServiceVersion string

	// Endpoint is an OTLP/HTTP host:port, for example "localhost:4318".
	//
	// HTTP rather than gRPC is a deliberate choice: it keeps grpc-go out of
	// the dependency tree, which is most of the difference between a binary
	// that fits in a small scratch image and one that does not. Every
	// collector accepts both.
	Endpoint string
	Insecure bool

	// MetricInterval controls how often metrics are pushed. Defaults to 10s.
	MetricInterval time.Duration
}

// Provider holds the configured SDK and knows how to shut it down.
type Provider struct {
	Tracer   *sdktrace.TracerProvider
	Meter    *sdkmetric.MeterProvider
	shutdown []func(context.Context) error
}

// Setup configures the global tracer and meter providers and returns a handle
// for shutting them down.
func Setup(ctx context.Context, cfg Config) (*Provider, error) {
	if cfg.MetricInterval <= 0 {
		cfg.MetricInterval = 10 * time.Second
	}

	res, err := resource.New(ctx,
		resource.WithFromEnv(),
		resource.WithTelemetrySDK(),
		resource.WithHost(),
		resource.WithAttributes(
			attribute.String("service.name", cfg.ServiceName),
			attribute.String("service.version", cfg.ServiceVersion),
		),
	)
	// A schema URL conflict between detectors is not fatal: the resource is
	// still usable, and refusing to start over it would be absurd.
	if err != nil && !errors.Is(err, resource.ErrSchemaURLConflict) {
		return nil, fmt.Errorf("build resource: %w", err)
	}

	traceOpts := []otlptracehttp.Option{}
	metricOpts := []otlpmetrichttp.Option{}
	if cfg.Endpoint != "" {
		traceOpts = append(traceOpts, otlptracehttp.WithEndpoint(cfg.Endpoint))
		metricOpts = append(metricOpts, otlpmetrichttp.WithEndpoint(cfg.Endpoint))
	}
	if cfg.Insecure {
		traceOpts = append(traceOpts, otlptracehttp.WithInsecure())
		metricOpts = append(metricOpts, otlpmetrichttp.WithInsecure())
	}

	traceExp, err := otlptracehttp.New(ctx, traceOpts...)
	if err != nil {
		return nil, fmt.Errorf("otlp trace exporter: %w", err)
	}
	metricExp, err := otlpmetrichttp.New(ctx, metricOpts...)
	if err != nil {
		return nil, fmt.Errorf("otlp metric exporter: %w", err)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(traceExp),
		sdktrace.WithResource(res),
	)
	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(metricExp,
			sdkmetric.WithInterval(cfg.MetricInterval))),
	)

	otel.SetTracerProvider(tp)
	otel.SetMeterProvider(mp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{},
	))

	return &Provider{
		Tracer:   tp,
		Meter:    mp,
		shutdown: []func(context.Context) error{tp.Shutdown, mp.Shutdown},
	}, nil
}

// Shutdown flushes pending telemetry. Call it with a bounded context: a
// collector that has gone away must not hold up the process exit.
func (p *Provider) Shutdown(ctx context.Context) error {
	if p == nil {
		return nil
	}
	var errs []error
	for _, fn := range p.shutdown {
		if err := fn(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

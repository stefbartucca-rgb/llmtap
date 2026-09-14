// Command llmtap is an observability proxy for model API traffic.
//
// Point a client at it instead of the provider — Codex CLI via openai_base_url,
// Claude Code via ANTHROPIC_BASE_URL, any SDK via its base URL setting — and
// every call it makes turns into an OpenTelemetry span with token, cost and
// latency metrics, without touching the client.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/stefbartucca-rgb/llmtap/internal/config"
	"github.com/stefbartucca-rgb/llmtap/internal/pricing"
	"github.com/stefbartucca-rgb/llmtap/internal/proxy"
	"github.com/stefbartucca-rgb/llmtap/internal/telemetry"
)

// version is stamped in at build time via -ldflags.
var version = "dev"

func main() {
	if err := run(); err != nil {
		slog.Error("llmtap exited", slog.Any("error", err))
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load(version)
	if err != nil {
		return err
	}

	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel}))
	slog.SetDefault(log)

	// Signals cancel this context, which unwinds the shutdown sequence below.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	tel, err := telemetry.Setup(ctx, telemetry.Config{
		ServiceName:    cfg.ServiceName,
		ServiceVersion: cfg.ServiceVersion,
		Endpoint:       cfg.OTLPEndpoint,
		Insecure:       cfg.OTLPInsecure,
	})
	if err != nil {
		return err
	}
	defer shutdownTelemetry(tel, log)

	prices := loadPrices(cfg.PricingPath, log)

	// A nil *pricing.Table would still be a non-nil Pricer interface, and the
	// recorder would call straight into a nil receiver.
	var pricer telemetry.Pricer
	if prices != nil {
		pricer = prices
	}

	recorder, err := telemetry.NewRecorder(tel.Tracer, tel.Meter, pricer)
	if err != nil {
		return err
	}

	srv, err := proxy.New(proxy.Options{
		Config:   cfg,
		Recorder: recorder,
		Logger:   log,
		Prices:   prices,
	})
	if err != nil {
		return err
	}

	httpSrv := &http.Server{
		Addr:    cfg.Addr,
		Handler: srv.Handler(),

		// No WriteTimeout on purpose: a streamed answer legitimately runs for
		// minutes, and a deadline here would sever it mid-response.
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	log.Info("llmtap listening",
		slog.String("addr", cfg.Addr),
		slog.String("version", version),
		slog.String("openai_upstream", cfg.OpenAIBaseURL.String()),
		slog.String("anthropic_upstream", cfg.AnthropicBaseURL.String()),
		slog.Bool("inject_stream_usage", cfg.InjectStreamUsage))

	errCh := make(chan error, 1)
	go func() {
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		log.Info("shutting down")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	return httpSrv.Shutdown(shutdownCtx)
}

// loadPrices reads the price table. A missing or broken table disables the
// cost metric but must not stop the proxy: forwarding traffic is the job,
// pricing it is a bonus.
func loadPrices(path string, log *slog.Logger) *pricing.Table {
	if path == "" {
		log.Info("no price table configured; cost will not be reported")
		return nil
	}

	table, err := pricing.Load(path)
	if err != nil {
		log.Warn("could not load the price table; cost will not be reported",
			slog.String("path", path), slog.Any("error", err))
		return nil
	}
	log.Info("price table loaded", slog.String("path", path))
	return table
}

// shutdownTelemetry flushes buffered spans and metrics on a fresh context: by
// the time this runs the signal context is already cancelled, and an expired
// context would discard exactly the data from the final requests.
func shutdownTelemetry(tel *telemetry.Provider, log *slog.Logger) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := tel.Shutdown(ctx); err != nil {
		log.Warn("telemetry shutdown was incomplete", slog.Any("error", err))
	}
}

// Package config loads llmtap settings from the environment.
//
// Environment variables rather than a config file: llmtap runs in a container
// next to a collector, and every value here is something an operator sets once
// per deployment.
package config

import (
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Defaults chosen to make "docker compose up" work with no configuration.
const (
	DefaultAddr             = ":8080"
	DefaultOpenAIBaseURL    = "https://api.openai.com"
	DefaultAnthropicBaseURL = "https://api.anthropic.com"
	DefaultPricingPath      = "configs/pricing.yaml"
	DefaultBodyLimit        = 64 << 20 // 64 MiB
	DefaultShutdownTimeout  = 15 * time.Second
)

// Config is the fully resolved runtime configuration.
type Config struct {
	Addr            string
	ShutdownTimeout time.Duration
	LogLevel        slog.Level

	OpenAIBaseURL    *url.URL
	AnthropicBaseURL *url.URL

	// RequestBodyLimit caps how much of a request llmtap will buffer in order
	// to read the model name out of it.
	RequestBodyLimit int64

	// InjectStreamUsage adds stream_options.include_usage to streaming Chat
	// Completions requests. Without it those calls report no token counts at
	// all; with it, the client sees one extra final chunk.
	InjectStreamUsage bool

	// PricingPath may be empty, which disables the cost metric.
	PricingPath string

	ServiceName    string
	ServiceVersion string
	OTLPEndpoint   string
	OTLPInsecure   bool
}

// Load reads the configuration, applying defaults for anything unset.
func Load(version string) (Config, error) {
	cfg := Config{
		Addr:              env("LLMTAP_ADDR", DefaultAddr),
		RequestBodyLimit:  DefaultBodyLimit,
		ShutdownTimeout:   DefaultShutdownTimeout,
		InjectStreamUsage: true,
		PricingPath:       env("LLMTAP_PRICING", DefaultPricingPath),
		ServiceName:       env("LLMTAP_SERVICE_NAME", "llmtap"),
		ServiceVersion:    version,
		OTLPEndpoint:      os.Getenv("LLMTAP_OTLP_ENDPOINT"),
	}

	var err error
	if cfg.OpenAIBaseURL, err = parseBaseURL("LLMTAP_OPENAI_BASE_URL", DefaultOpenAIBaseURL); err != nil {
		return Config{}, err
	}
	if cfg.AnthropicBaseURL, err = parseBaseURL("LLMTAP_ANTHROPIC_BASE_URL", DefaultAnthropicBaseURL); err != nil {
		return Config{}, err
	}
	if cfg.LogLevel, err = parseLogLevel(env("LLMTAP_LOG_LEVEL", "info")); err != nil {
		return Config{}, err
	}
	if cfg.InjectStreamUsage, err = envBool("LLMTAP_INJECT_STREAM_USAGE", true); err != nil {
		return Config{}, err
	}
	if cfg.OTLPInsecure, err = envBool("LLMTAP_OTLP_INSECURE", true); err != nil {
		return Config{}, err
	}
	if cfg.RequestBodyLimit, err = envBytes("LLMTAP_BODY_LIMIT", DefaultBodyLimit); err != nil {
		return Config{}, err
	}
	if cfg.ShutdownTimeout, err = envDuration("LLMTAP_SHUTDOWN_TIMEOUT", DefaultShutdownTimeout); err != nil {
		return Config{}, err
	}

	return cfg, nil
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func parseBaseURL(key, fallback string) (*url.URL, error) {
	raw := env(key, fallback)

	parsed, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", key, err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("%s: need an http or https URL, got %q", key, raw)
	}
	if parsed.Host == "" {
		return nil, fmt.Errorf("%s: missing host in %q", key, raw)
	}

	// A trailing slash would double up when joined with the request path.
	parsed.Path = strings.TrimSuffix(parsed.Path, "/")
	return parsed, nil
}

func parseLogLevel(raw string) (slog.Level, error) {
	switch strings.ToLower(raw) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("LLMTAP_LOG_LEVEL: unknown level %q", raw)
	}
}

func envBool(key string, fallback bool) (bool, error) {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback, nil
	}
	v, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("%s: %w", key, err)
	}
	return v, nil
}

func envBytes(key string, fallback int64) (int64, error) {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback, nil
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	if v <= 0 {
		return 0, fmt.Errorf("%s: must be positive, got %d", key, v)
	}
	return v, nil
}

func envDuration(key string, fallback time.Duration) (time.Duration, error) {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback, nil
	}
	v, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return v, nil
}

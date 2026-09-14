package proxy_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/stefbartucca-rgb/llmtap/internal/config"
	"github.com/stefbartucca-rgb/llmtap/internal/proxy"
	"github.com/stefbartucca-rgb/llmtap/internal/telemetry"
)

const responsesStream = "event: response.created\n" +
	`data: {"type":"response.created","response":{"id":"resp_1","model":"gpt-5-codex","status":"in_progress"}}` + "\n\n" +
	"event: response.completed\n" +
	`data: {"type":"response.completed","response":{"id":"resp_1","model":"gpt-5-codex","status":"completed","usage":{"input_tokens":1000,"input_tokens_details":{"cached_tokens":400},"output_tokens":200}}}` + "\n\n"

// fixedPricer prices everything the same, so a cost assertion is about the
// plumbing rather than about the numbers.
type fixedPricer struct{ usd float64 }

func (p fixedPricer) Cost(_, _ string, _, _, _ int64) (float64, bool) { return p.usd, true }

type harness struct {
	proxyURL string
	spans    *tracetest.SpanRecorder
	reader   *sdkmetric.ManualReader

	mu           sync.Mutex
	upstreamReqs []*http.Request
	upstreamBody []string
}

// lastUpstream returns what the fake provider actually received.
func (h *harness) lastUpstream(t *testing.T) (*http.Request, string) {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()

	if len(h.upstreamReqs) == 0 {
		t.Fatal("upstream received no requests")
	}
	return h.upstreamReqs[len(h.upstreamReqs)-1], h.upstreamBody[len(h.upstreamBody)-1]
}

func newHarness(t *testing.T, upstreamHandler http.HandlerFunc) *harness {
	t.Helper()
	h := &harness{}

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)

		h.mu.Lock()
		h.upstreamReqs = append(h.upstreamReqs, r.Clone(context.Background()))
		h.upstreamBody = append(h.upstreamBody, string(body))
		h.mu.Unlock()

		upstreamHandler(w, r)
	}))
	t.Cleanup(upstream.Close)

	h.spans = tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(h.spans))
	h.reader = sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(h.reader))

	recorder, err := telemetry.NewRecorder(tp, mp, fixedPricer{usd: 0.25})
	if err != nil {
		t.Fatalf("NewRecorder: %v", err)
	}

	target, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatalf("parse upstream URL: %v", err)
	}

	srv, err := proxy.New(proxy.Options{
		Config: config.Config{
			OpenAIBaseURL:     target,
			AnthropicBaseURL:  target,
			RequestBodyLimit:  1 << 20,
			InjectStreamUsage: true,
		},
		Recorder: recorder,
		// Discard keeps warning lines out of the test output; the assertions
		// are about telemetry, not logs.
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

	front := httptest.NewServer(srv.Handler())
	t.Cleanup(front.Close)
	h.proxyURL = front.URL

	return h
}

// post sends a request through the proxy and returns the completed response.
func (h *harness) post(t *testing.T, path, body string, headers map[string]string) *http.Response {
	t.Helper()

	req, err := http.NewRequest(http.MethodPost, h.proxyURL+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request through proxy: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })

	// Drain so the tap sees EOF and the span is ended before assertions run.
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		t.Fatalf("drain response: %v", err)
	}
	return resp
}

func sseHandler(payload string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, payload)
	}
}

func TestStreamingCallProducesSpan(t *testing.T) {
	h := newHarness(t, sseHandler(responsesStream))

	h.post(t, "/v1/responses", `{"model":"gpt-5-codex","stream":true}`, nil)

	span := onlySpan(t, h)
	if span.Name() != "chat gpt-5-codex" {
		t.Errorf("span name = %q, want %q", span.Name(), "chat gpt-5-codex")
	}

	assertAttr(t, span, "gen_ai.operation.name", "chat")
	assertAttr(t, span, "gen_ai.provider.name", "openai")
	assertAttr(t, span, "gen_ai.request.model", "gpt-5-codex")
	assertAttr(t, span, "gen_ai.response.model", "gpt-5-codex")
	assertAttr(t, span, "gen_ai.response.id", "resp_1")
	assertAttr(t, span, "gen_ai.usage.input_tokens", int64(1000))
	assertAttr(t, span, "gen_ai.usage.output_tokens", int64(200))

	if span.Status().Code == codes.Error {
		t.Errorf("span marked as error: %+v", span.Status())
	}
}

// TestCredentialsNeverReachTelemetry is the test that has to keep passing. An
// observability proxy sees every API key that passes through it; leaking one
// into a trace would hand it to whoever can read the dashboards.
func TestCredentialsNeverReachTelemetry(t *testing.T) {
	const secret = "sk-proj-SUPERSECRET-do-not-log-0123456789"

	h := newHarness(t, sseHandler(responsesStream))
	h.post(t, "/v1/responses", `{"model":"gpt-5-codex","stream":true,"input":"hello"}`,
		map[string]string{
			"Authorization": "Bearer " + secret,
			"x-api-key":     secret,
		})

	// The credential must still reach the provider, or the proxy is useless.
	upstreamReq, _ := h.lastUpstream(t)
	if got := upstreamReq.Header.Get("Authorization"); got != "Bearer "+secret {
		t.Errorf("upstream Authorization = %q, want the header forwarded intact", got)
	}

	for _, span := range waitForSpans(t, h, 1) {
		for _, attr := range span.Attributes() {
			if strings.Contains(attr.Value.String(), secret) {
				t.Errorf("attribute %s leaked the API key", attr.Key)
			}
		}
		for _, event := range span.Events() {
			for _, attr := range event.Attributes {
				if strings.Contains(attr.Value.String(), secret) {
					t.Errorf("event attribute %s leaked the API key", attr.Key)
				}
			}
		}
		if strings.Contains(span.Status().Description, secret) {
			t.Error("span status description leaked the API key")
		}
	}
}

// TestStreamingIsNotBuffered guards the whole point of the tap: the proxy must
// hand each chunk to the client as it arrives. If it ever accumulates the
// response to parse it, the first read below blocks until the upstream is
// released and this test times out.
func TestStreamingIsNotBuffered(t *testing.T) {
	release := make(chan struct{})

	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)

		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("upstream ResponseWriter cannot flush")
			return
		}
		_, _ = io.WriteString(w, "event: response.created\ndata: {\"type\":\"response.created\"}\n\n")
		flusher.Flush()

		<-release // hold the rest back until the client proves it got the first part
		_, _ = io.WriteString(w, responsesStream)
		flusher.Flush()
	})

	req, err := http.NewRequest(http.MethodPost, h.proxyURL+"/v1/responses",
		strings.NewReader(`{"model":"gpt-5-codex","stream":true}`))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request through proxy: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	firstRead := make(chan string, 1)
	go func() {
		buf := make([]byte, 256)
		n, _ := resp.Body.Read(buf)
		firstRead <- string(buf[:n])
	}()

	select {
	case got := <-firstRead:
		if !strings.Contains(got, "response.created") {
			t.Errorf("first read = %q, want the first event", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the first chunk never arrived: the proxy is buffering the stream")
	}

	close(release)
	_, _ = io.Copy(io.Discard, resp.Body)
}

// Without stream_options.include_usage a streamed Chat Completions call
// reports no tokens at all, so the proxy adds it.
func TestInjectStreamUsage(t *testing.T) {
	h := newHarness(t, sseHandler(
		`data: {"id":"c1","model":"gpt-4.1","choices":[{"finish_reason":"stop"}],`+
			`"usage":{"prompt_tokens":10,"completion_tokens":5}}`+"\n\ndata: [DONE]\n\n"))

	h.post(t, "/v1/chat/completions", `{"model":"gpt-4.1","stream":true}`, nil)

	_, body := h.lastUpstream(t)
	if !strings.Contains(body, `"include_usage":true`) {
		t.Errorf("upstream body = %s, want stream_options.include_usage added", body)
	}

	span := onlySpan(t, h)
	assertAttr(t, span, "gen_ai.usage.input_tokens", int64(10))
	assertAttr(t, span, "gen_ai.usage.output_tokens", int64(5))
}

// Codex talks to endpoints llmtap has no opinion about. Those must pass
// through untouched and produce no telemetry.
func TestUninstrumentedPathIsForwardedWithoutSpans(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[{"id":"gpt-5-codex"}]}`)
	})

	resp, err := http.Get(h.proxyURL + "/v1/models")
	if err != nil {
		t.Fatalf("GET /v1/models: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "gpt-5-codex") {
		t.Errorf("body = %s, want the upstream response forwarded", body)
	}
	if got := len(h.spans.Ended()); got != 0 {
		t.Errorf("recorded %d spans for an uninstrumented path, want 0", got)
	}
}

func TestUpstreamErrorStatusIsRecorded(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"message":"rate limit"}}`)
	})

	resp := h.post(t, "/v1/responses", `{"model":"gpt-5-codex"}`, nil)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("status = %d, want the upstream status passed through", resp.StatusCode)
	}

	span := onlySpan(t, h)
	assertAttr(t, span, "error.type", "429")
	if span.Status().Code != codes.Error {
		t.Errorf("span status = %v, want Error", span.Status().Code)
	}
	// A rejected call has no usage, so no token attributes should exist.
	if _, ok := findAttr(span, "gen_ai.usage.input_tokens"); ok {
		t.Error("token attributes were set for a call that reported no usage")
	}
}

func TestMetricsAreRecorded(t *testing.T) {
	h := newHarness(t, sseHandler(responsesStream))
	h.post(t, "/v1/responses", `{"model":"gpt-5-codex","stream":true}`, nil)
	waitForSpans(t, h, 1) // metrics are emitted alongside the span, in End

	var rm metricdata.ResourceMetrics
	if err := h.reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect metrics: %v", err)
	}

	recorded := map[string]bool{}
	for _, scope := range rm.ScopeMetrics {
		for _, m := range scope.Metrics {
			recorded[m.Name] = true
		}
	}

	for _, name := range []string{
		"gen_ai.client.token.usage",
		"gen_ai.client.operation.duration",
		"gen_ai.client.operation.time_to_first_chunk",
		"llmtap.cost.usd",
	} {
		if !recorded[name] {
			t.Errorf("metric %s was not recorded; got %v", name, recorded)
		}
	}
}

// waitForSpans blocks until n spans have ended.
//
// The client sees EOF as soon as the last byte is written, but the server
// goroutine still has to return from ServeHTTP and call End. Asserting
// immediately after the response would race against that.
func waitForSpans(t *testing.T, h *harness, n int) []sdktrace.ReadOnlySpan {
	t.Helper()

	deadline := time.Now().Add(3 * time.Second)
	for {
		ended := h.spans.Ended()
		if len(ended) >= n {
			return ended
		}
		if time.Now().After(deadline) {
			t.Fatalf("got %d spans after 3s, want %d", len(ended), n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func onlySpan(t *testing.T, h *harness) sdktrace.ReadOnlySpan {
	t.Helper()

	ended := waitForSpans(t, h, 1)
	if len(ended) != 1 {
		t.Fatalf("got %d spans, want exactly 1", len(ended))
	}
	return ended[0]
}

func findAttr(span sdktrace.ReadOnlySpan, key string) (attribute.Value, bool) {
	for _, attr := range span.Attributes() {
		if string(attr.Key) == key {
			return attr.Value, true
		}
	}
	return attribute.Value{}, false
}

func assertAttr(t *testing.T, span sdktrace.ReadOnlySpan, key string, want any) {
	t.Helper()

	value, ok := findAttr(span, key)
	if !ok {
		t.Errorf("attribute %s is missing", key)
		return
	}

	var got any
	switch want.(type) {
	case int64:
		got = value.AsInt64()
	default:
		got = value.AsString()
	}
	if got != want {
		t.Errorf("attribute %s = %v, want %v", key, got, want)
	}
}

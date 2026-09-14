// Package proxy is the HTTP front end: it forwards model API traffic
// unchanged and reports on it as it passes.
package proxy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"

	"github.com/stefbartucca-rgb/llmtap/internal/config"
	"github.com/stefbartucca-rgb/llmtap/internal/pricing"
	"github.com/stefbartucca-rgb/llmtap/internal/telemetry"
	"github.com/stefbartucca-rgb/llmtap/internal/wire"
)

// anthropicVersionHeader is how an Anthropic client identifies itself. It is
// the only reliable way to route a path that both APIs could plausibly use.
const anthropicVersionHeader = "anthropic-version"

// stateKey carries per-call state from the handler through the reverse proxy
// to ModifyResponse, which only receives the outbound request.
type stateKey struct{}

// callState is the mutable record of one call in flight.
//
// It is written by the handler goroutine and by the tap, which runs on that
// same goroutine while the body is copied. Nothing else touches it, so no
// locking is needed — and the race detector in CI proves it.
type callState struct {
	call   *telemetry.Call
	parser wire.Parser
	info   wire.CallInfo
	status int
	err    error
}

func stateFrom(ctx context.Context) *callState {
	st, _ := ctx.Value(stateKey{}).(*callState)
	return st
}

// Options are the dependencies of a Server.
type Options struct {
	Config   config.Config
	Recorder *telemetry.Recorder
	Logger   *slog.Logger

	// Prices is optional and used only to warn once about models that carry
	// no price. Cost itself is computed inside the recorder.
	Prices *pricing.Table
}

// Server routes and instruments model API traffic.
type Server struct {
	cfg    config.Config
	rec    *telemetry.Recorder
	log    *slog.Logger
	prices *pricing.Table

	openai    *httputil.ReverseProxy
	anthropic *httputil.ReverseProxy
}

// New builds a Server.
func New(opts Options) (*Server, error) {
	if opts.Recorder == nil {
		return nil, errors.New("proxy: a Recorder is required")
	}
	if opts.Logger == nil {
		return nil, errors.New("proxy: a Logger is required")
	}

	s := &Server{
		cfg:    opts.Config,
		rec:    opts.Recorder,
		log:    opts.Logger,
		prices: opts.Prices,
	}
	s.openai = s.newReverseProxy(opts.Config.OpenAIBaseURL)
	s.anthropic = s.newReverseProxy(opts.Config.AnthropicBaseURL)
	return s, nil
}

// Handler returns the routing table.
//
// Instrumented routes cover the three endpoints that carry model calls.
// Everything else is forwarded untouched, because a client such as Codex also
// talks to endpoints llmtap has no opinion about and must not break.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("POST /v1/responses",
		s.instrumented(wire.OpenAIResponses{}, s.openai, s.cfg.OpenAIBaseURL, false))
	mux.HandleFunc("POST /v1/chat/completions",
		s.instrumented(wire.OpenAIChat{}, s.openai, s.cfg.OpenAIBaseURL, s.cfg.InjectStreamUsage))
	mux.HandleFunc("POST /v1/messages",
		s.instrumented(wire.Anthropic{}, s.anthropic, s.cfg.AnthropicBaseURL, false))

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, "ok\n")
	})

	mux.HandleFunc("/", s.passthrough)
	return mux
}

// newReverseProxy builds the forwarder for one upstream.
func (s *Server) newReverseProxy(target *url.URL) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.Host = target.Host

			// Let Go's transport negotiate compression instead of the client.
			// It only decompresses transparently when it set Accept-Encoding
			// itself; if the client's header is forwarded, the tap would be
			// handed gzip bytes and parse nothing. The hop to the client is
			// then uncompressed, which for a local sidecar costs nothing.
			pr.Out.Header.Del("Accept-Encoding")
		},

		// -1 flushes every write straight through, which is what keeps a
		// streamed response streaming rather than arriving in one lump.
		FlushInterval: -1,

		ModifyResponse: s.tapResponse,
		ErrorHandler:   s.handleUpstreamError,
	}
}

// instrumented wraps one endpoint with a span, request parsing and the tap.
func (s *Server) instrumented(
	parser wire.Parser,
	rp *httputil.ReverseProxy,
	upstream *url.URL,
	injectUsage bool,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := readBody(r, s.cfg.RequestBodyLimit)
		if err != nil {
			s.log.Warn("rejected an oversized or unreadable request body",
				slog.String("path", r.URL.Path), slog.Any("error", err))
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}

		// A body llmtap cannot parse is still a body upstream may accept, so
		// the error only costs us the model name.
		reqInfo, parseErr := parser.ParseRequest(body)
		if parseErr != nil {
			s.log.Debug("could not read the model from the request body",
				slog.String("path", r.URL.Path), slog.Any("error", parseErr))
		}

		if injectUsage && reqInfo.Stream {
			if rewritten, changed := wire.InjectStreamUsage(body); changed {
				body = rewritten
			}
		}
		restoreBody(r, body)

		ctx, call := s.rec.StartCall(r.Context(), telemetry.CallOptions{
			Provider:      parser.Provider(),
			Operation:     parser.Operation(),
			RequestModel:  reqInfo.Model,
			ServerAddress: upstream.Hostname(),
			ServerPort:    upstreamPort(upstream),
		})

		st := &callState{
			call:   call,
			parser: parser,
			info: wire.CallInfo{
				Provider:     parser.Provider(),
				Operation:    parser.Operation(),
				RequestModel: reqInfo.Model,
				Streaming:    reqInfo.Stream,
			},
		}

		rp.ServeHTTP(w, r.WithContext(context.WithValue(ctx, stateKey{}, st)))

		// ServeHTTP has returned, so the body copy — and with it every write
		// the tap made to st — has completed.
		call.End(st.info, st.status, st.err)
		s.warnIfUnpriced(st)
	}
}

// passthrough forwards anything llmtap does not instrument.
func (s *Server) passthrough(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get(anthropicVersionHeader) != "" {
		s.anthropic.ServeHTTP(w, r)
		return
	}
	s.openai.ServeHTTP(w, r)
}

// tapResponse installs the tap. It runs for every proxied response, including
// the uninstrumented ones, which carry no state and are left alone.
func (s *Server) tapResponse(resp *http.Response) error {
	st := stateFrom(resp.Request.Context())
	if st == nil {
		return nil
	}
	st.status = resp.StatusCode

	// The response decides whether this is a stream. A request can ask for one
	// and be answered with a plain error document instead.
	streaming := strings.Contains(
		strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream")

	resp.Body = newResponseTap(
		resp.Body, st.parser, &st.info, st.call, s.log, streaming, s.cfg.RequestBodyLimit)
	return nil
}

// handleUpstreamError reports a call that never produced a response.
func (s *Server) handleUpstreamError(w http.ResponseWriter, r *http.Request, err error) {
	if st := stateFrom(r.Context()); st != nil {
		st.err = err
	}

	// A client that gave up is routine, not a fault worth an error log.
	level := slog.LevelWarn
	if errors.Is(err, context.Canceled) {
		level = slog.LevelDebug
	}
	s.log.Log(r.Context(), level, "upstream request failed",
		slog.String("path", r.URL.Path), slog.Any("error", err))

	w.WriteHeader(http.StatusBadGateway)
}

// warnIfUnpriced points out a model missing from the price table, once.
func (s *Server) warnIfUnpriced(st *callState) {
	if s.prices == nil || !st.info.UsageKnown {
		return
	}
	model := st.info.ResponseModel
	if model == "" {
		model = st.info.RequestModel
	}
	provider := string(st.info.Provider)

	if _, ok := s.prices.Cost(provider, model, 0, 0, 0); ok {
		return
	}
	if s.prices.ShouldWarn(provider, model) {
		s.log.Warn("no price for this model; cost will not be reported for it",
			slog.String("provider", provider),
			slog.String("model", model))
	}
}

// readBody buffers the request so the model name can be read out of it, and
// refuses anything past the limit rather than growing without bound.
func readBody(r *http.Request, limit int64) ([]byte, error) {
	if r.Body == nil {
		return nil, nil
	}
	defer func() { _ = r.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("read request body: %w", err)
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("request body exceeds the %d byte limit", limit)
	}
	return body, nil
}

// restoreBody puts a buffered body back on the request. GetBody matters
// because the transport replays it when it has to retry.
func restoreBody(r *http.Request, body []byte) {
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	r.Header.Set("Content-Length", strconv.Itoa(len(body)))
	r.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(body)), nil
	}
}

// upstreamPort resolves the effective port, which the conventions want
// alongside server.address.
func upstreamPort(u *url.URL) int {
	if p := u.Port(); p != "" {
		port, err := strconv.Atoi(p)
		if err == nil {
			return port
		}
	}
	if u.Scheme == "https" {
		return 443
	}
	return 80
}

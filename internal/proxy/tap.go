package proxy

import (
	"bytes"
	"io"
	"log/slog"
	"sync"

	"github.com/stefbartucca-rgb/llmtap/internal/sse"
	"github.com/stefbartucca-rgb/llmtap/internal/telemetry"
	"github.com/stefbartucca-rgb/llmtap/internal/wire"
)

// responseTap wraps the upstream response body and reads along as the bytes
// travel to the client.
//
// It runs on the goroutine doing the copy, so there is no buffering of the
// whole response and no added latency beyond parsing a few small control
// events. Nothing it does may disturb the call: every observation is wrapped
// so that a parser bug degrades telemetry instead of the response.
type responseTap struct {
	inner  io.ReadCloser
	parser wire.Parser
	info   *wire.CallInfo
	call   *telemetry.Call
	logger *slog.Logger

	streaming bool
	decoder   *sse.Decoder

	// buf accumulates a non-streaming body until it can be parsed at EOF.
	buf       bytes.Buffer
	limit     int64
	truncated bool

	finishOnce sync.Once
}

func newResponseTap(
	inner io.ReadCloser,
	parser wire.Parser,
	info *wire.CallInfo,
	call *telemetry.Call,
	logger *slog.Logger,
	streaming bool,
	limit int64,
) *responseTap {
	tap := &responseTap{
		inner:     inner,
		parser:    parser,
		info:      info,
		call:      call,
		logger:    logger,
		streaming: streaming,
		limit:     limit,
	}
	if streaming {
		tap.decoder = sse.NewDecoder(0)
	}
	return tap
}

func (t *responseTap) Read(p []byte) (int, error) {
	n, err := t.inner.Read(p)

	if n > 0 {
		t.call.FirstChunk()
		t.observe(p[:n])
	}
	if err != nil {
		// EOF or a broken connection: nothing further is coming.
		t.finish()
	}
	return n, err
}

func (t *responseTap) Close() error {
	// A client that hangs up mid-stream never produces an EOF on Read, so this
	// is the only chance to record what was seen.
	t.finish()
	return t.inner.Close()
}

// observe consumes one chunk exactly as it was handed to the client.
func (t *responseTap) observe(chunk []byte) {
	defer t.recoverParse("observe")

	if t.streaming {
		for _, ev := range t.decoder.Feed(chunk) {
			t.parser.ParseStreamEvent(ev, t.info)
		}
		return
	}

	remaining := t.limit - int64(t.buf.Len())
	if remaining <= 0 {
		t.truncated = true
		return
	}
	if int64(len(chunk)) > remaining {
		chunk = chunk[:remaining]
		t.truncated = true
	}
	t.buf.Write(chunk)
}

// finish parses whatever a non-streaming response accumulated. Streaming
// responses were parsed event by event and have nothing left to do.
func (t *responseTap) finish() {
	t.finishOnce.Do(func() {
		defer t.recoverParse("finish")

		if t.streaming {
			if dropped := t.decoder.Truncated(); dropped > 0 {
				t.logger.Warn("oversized SSE events were dropped; usage may be incomplete",
					slog.Int("dropped", dropped))
			}
			return
		}
		if t.truncated {
			// Half a JSON document parses to nothing useful, and reporting
			// partial token counts would be worse than reporting none.
			t.logger.Warn("response exceeded the parse limit; usage not recorded",
				slog.Int64("limit_bytes", t.limit))
			return
		}
		if t.buf.Len() == 0 {
			return
		}
		t.parser.ParseResponse(t.buf.Bytes(), t.info)
	})
}

// recoverParse keeps a malformed payload from taking down the request. llmtap
// observing a call badly is a bug; llmtap breaking a call is an outage.
func (t *responseTap) recoverParse(stage string) {
	if r := recover(); r != nil {
		t.logger.Error("response parser panicked; telemetry for this call is incomplete",
			slog.String("stage", stage),
			slog.Any("panic", r))
	}
}

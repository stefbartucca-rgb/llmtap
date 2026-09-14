package sse

import (
	"strings"
	"testing"
)

func TestDecoderEvents(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  []Event
	}{
		{
			name:  "typed event",
			input: "event: message_start\ndata: {\"a\":1}\n\n",
			want:  []Event{{Type: "message_start", Data: []byte(`{"a":1}`)}},
		},
		{
			name:  "untyped event, as Chat Completions sends them",
			input: "data: {\"a\":1}\n\ndata: [DONE]\n\n",
			want: []Event{
				{Data: []byte(`{"a":1}`)},
				{Data: []byte(`[DONE]`)},
			},
		},
		{
			name:  "multi-line data is joined with newlines",
			input: "data: line1\ndata: line2\n\n",
			want:  []Event{{Data: []byte("line1\nline2")}},
		},
		{
			name:  "CRLF line endings",
			input: "event: ping\r\ndata: {}\r\n\r\n",
			want:  []Event{{Type: "ping", Data: []byte("{}")}},
		},
		{
			name:  "comments are keep-alives, not events",
			input: ": keep-alive\n\ndata: real\n\n",
			want:  []Event{{Data: []byte("real")}},
		},
		{
			name:  "value keeps inner spaces, drops only the one after the colon",
			input: "data:  {\"k\": \"v\"}\n\n",
			want:  []Event{{Data: []byte(` {"k": "v"}`)}},
		},
		{
			name:  "incomplete trailing event is withheld",
			input: "data: complete\n\ndata: incompl",
			want:  []Event{{Data: []byte("complete")}},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := NewDecoder(0).Feed([]byte(tc.input))
			assertEvents(t, got, tc.want)
		})
	}
}

// TestDecoderChunkBoundaries is the case that matters in production: the proxy
// feeds whatever a single Read returned, which splits events at arbitrary
// offsets. Decoding must not depend on where those splits land.
func TestDecoderChunkBoundaries(t *testing.T) {
	const stream = "event: message_start\ndata: {\"type\":\"message_start\"}\n\n" +
		"event: message_delta\ndata: {\"usage\":{\"output_tokens\":42}}\n\n"

	want := []Event{
		{Type: "message_start", Data: []byte(`{"type":"message_start"}`)},
		{Type: "message_delta", Data: []byte(`{"usage":{"output_tokens":42}}`)},
	}

	for split := 0; split <= len(stream); split++ {
		d := NewDecoder(0)
		var got []Event
		got = append(got, d.Feed([]byte(stream[:split]))...)
		got = append(got, d.Feed([]byte(stream[split:]))...)

		if len(got) != len(want) {
			t.Fatalf("split at %d: got %d events, want %d", split, len(got), len(want))
		}
		assertEvents(t, got, want)
	}
}

// TestDecoderBytewise is the pathological version of the above: one byte per
// Feed, which is what a heavily fragmented TLS stream can look like.
func TestDecoderBytewise(t *testing.T) {
	const stream = "event: a\ndata: 1\n\nevent: b\ndata: 2\n\n"

	d := NewDecoder(0)
	var got []Event
	for i := 0; i < len(stream); i++ {
		got = append(got, d.Feed([]byte{stream[i]})...)
	}

	assertEvents(t, got, []Event{
		{Type: "a", Data: []byte("1")},
		{Type: "b", Data: []byte("2")},
	})
}

func TestDecoderDropsOversizedEvent(t *testing.T) {
	d := NewDecoder(64)

	if got := d.Feed([]byte("data: " + strings.Repeat("x", 200))); got != nil {
		t.Fatalf("got %d events from an oversized block, want none", len(got))
	}
	if d.Truncated() != 1 {
		t.Fatalf("Truncated() = %d, want 1", d.Truncated())
	}

	// The decoder must keep working afterwards rather than stay poisoned.
	assertEvents(t, d.Feed([]byte("data: ok\n\n")), []Event{{Data: []byte("ok")}})
}

func assertEvents(t *testing.T, got, want []Event) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d events, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i].Type != want[i].Type {
			t.Errorf("event %d: Type = %q, want %q", i, got[i].Type, want[i].Type)
		}
		if string(got[i].Data) != string(want[i].Data) {
			t.Errorf("event %d: Data = %q, want %q", i, got[i].Data, want[i].Data)
		}
	}
}

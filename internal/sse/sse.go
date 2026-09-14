// Package sse decodes Server-Sent Events from a byte stream that arrives in
// arbitrary chunks. It is deliberately incremental: the proxy hands it whatever
// a single Read produced, and gets back only the events that are complete.
package sse

import (
	"bytes"
	"io"
)

// DefaultMaxEventBytes caps the pending buffer. Model APIs send small control
// events, so anything larger means the stream is malformed or not SSE at all.
// Without a cap, a stream that never emits a blank line would grow the buffer
// until the process died.
const DefaultMaxEventBytes = 4 << 20 // 4 MiB

// Event is one dispatched SSE event.
type Event struct {
	// Type is the "event:" field. OpenAI's Responses API and Anthropic both
	// set it; Chat Completions leaves it empty and puts the type in the JSON.
	Type string
	// Data is the concatenation of the event's "data:" lines, joined by "\n".
	Data []byte
}

// Decoder accumulates bytes and emits events as they complete.
//
// The zero value is ready to use and applies DefaultMaxEventBytes.
type Decoder struct {
	pending   bytes.Buffer
	maxEvent  int
	truncated int
}

// NewDecoder returns a Decoder with an explicit size cap. A maxEvent of zero
// or less selects DefaultMaxEventBytes.
func NewDecoder(maxEvent int) *Decoder {
	if maxEvent <= 0 {
		maxEvent = DefaultMaxEventBytes
	}
	return &Decoder{maxEvent: maxEvent}
}

// Truncated reports how many times the pending buffer was discarded for
// exceeding the size cap. A non-zero value means events were lost.
func (d *Decoder) Truncated() int { return d.truncated }

// Feed appends a chunk and returns every event that became complete. Each
// Event owns its data, so the results stay valid across later calls.
func (d *Decoder) Feed(chunk []byte) []Event {
	if d.maxEvent <= 0 {
		d.maxEvent = DefaultMaxEventBytes
	}
	d.pending.Write(chunk)

	if d.pending.Len() > d.maxEvent {
		// Drop what we have rather than grow without bound. Resyncing on the
		// next blank line would be nicer, but a stream this malformed is not
		// worth the complexity — we are only observing, never serving, it.
		d.pending.Reset()
		d.truncated++
		return nil
	}

	var events []Event
	for {
		block, ok := d.nextBlock()
		if !ok {
			return events
		}
		if ev, ok := parseBlock(block); ok {
			events = append(events, ev)
		}
	}
}

// nextBlock pulls one complete event block (everything up to a blank line) off
// the front of the pending buffer.
func (d *Decoder) nextBlock() ([]byte, bool) {
	buf := d.pending.Bytes()

	// A blank line is "\n\n" or "\r\n\r\n"; normalizing first would mean
	// copying the whole buffer on every Feed, so both are matched directly.
	idx, sepLen := -1, 0
	if i := bytes.Index(buf, []byte("\r\n\r\n")); i >= 0 {
		idx, sepLen = i, 4
	}
	if i := bytes.Index(buf, []byte("\n\n")); i >= 0 && (idx < 0 || i < idx) {
		idx, sepLen = i, 2
	}
	if idx < 0 {
		return nil, false
	}

	block := make([]byte, idx)
	copy(block, buf[:idx])
	d.pending.Next(idx + sepLen)
	return block, true
}

// parseBlock turns one event block into an Event. It reports false for blocks
// that carry no data, which the SSE specification says must not be dispatched.
func parseBlock(block []byte) (Event, bool) {
	var (
		ev   Event
		data bytes.Buffer
	)

	for _, raw := range bytes.Split(block, []byte("\n")) {
		line := bytes.TrimSuffix(raw, []byte("\r"))
		if len(line) == 0 || line[0] == ':' {
			continue // blank line or comment (used as a keep-alive)
		}

		field, value, found := bytes.Cut(line, []byte(":"))
		if !found {
			// A line without a colon is a field with an empty value; none of
			// the fields we care about are meaningful that way.
			continue
		}
		value = bytes.TrimPrefix(value, []byte(" "))

		switch string(field) {
		case "event":
			ev.Type = string(value)
		case "data":
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.Write(value)
		}
		// "id" and "retry" carry no information llmtap needs.
	}

	if data.Len() == 0 {
		return Event{}, false
	}
	ev.Data = data.Bytes()
	return ev, true
}

// DecodeAll reads r to completion and returns every event in it. It exists for
// tests over recorded streams; the proxy uses Feed.
func DecodeAll(r io.Reader) ([]Event, error) {
	body, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	return NewDecoder(0).Feed(body), nil
}

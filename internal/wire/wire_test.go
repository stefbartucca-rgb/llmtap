package wire

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/stefbartucca-rgb/llmtap/internal/sse"
)

// replayFixture feeds a recorded stream through a parser the way the proxy
// would, and returns what it learned.
func replayFixture(t *testing.T, p Parser, name string) CallInfo {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	events, err := sse.DecodeAll(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	if len(events) == 0 {
		t.Fatalf("fixture %s decoded to no events", name)
	}

	info := CallInfo{Provider: p.Provider(), Operation: p.Operation(), Streaming: true}
	for _, ev := range events {
		p.ParseStreamEvent(ev, &info)
	}
	return info
}

func TestOpenAIResponsesStream(t *testing.T) {
	got := replayFixture(t, OpenAIResponses{}, "openai_responses_stream.sse")

	assertField(t, "ResponseID", got.ResponseID, "resp_abc123")
	assertField(t, "ResponseModel", got.ResponseModel, "gpt-5-codex")
	assertField(t, "InputTokens", got.InputTokens, int64(1024))
	assertField(t, "OutputTokens", got.OutputTokens, int64(256))
	assertField(t, "CachedInputTokens", got.CachedInputTokens, int64(768))
	assertField(t, "UsageKnown", got.UsageKnown, true)
	assertField(t, "FinishReasons", got.FinishReasons, []string{"completed"})
}

// The in_progress status on response.created is a lifecycle marker, not an
// outcome, and must not end up in the finish reasons.
func TestOpenAIResponsesIncompleteReportsReasonNotStatus(t *testing.T) {
	got := replayFixture(t, OpenAIResponses{}, "openai_responses_incomplete.sse")

	assertField(t, "FinishReasons", got.FinishReasons, []string{"max_output_tokens"})
	assertField(t, "OutputTokens", got.OutputTokens, int64(512))
	assertField(t, "UsageKnown", got.UsageKnown, true)
}

func TestOpenAIChatStream(t *testing.T) {
	got := replayFixture(t, OpenAIChat{}, "openai_chat_stream.sse")

	assertField(t, "ResponseModel", got.ResponseModel, "gpt-4.1")
	assertField(t, "InputTokens", got.InputTokens, int64(19))
	assertField(t, "OutputTokens", got.OutputTokens, int64(7))
	assertField(t, "CachedInputTokens", got.CachedInputTokens, int64(16))
	assertField(t, "UsageKnown", got.UsageKnown, true)
	assertField(t, "FinishReasons", got.FinishReasons, []string{"stop"})
}

// Without stream_options.include_usage the API reports nothing, and the
// conventions forbid inventing a number. UsageKnown has to stay false so the
// token and cost metrics are skipped rather than recorded as zero.
func TestOpenAIChatStreamWithoutUsageLeavesUsageUnknown(t *testing.T) {
	got := replayFixture(t, OpenAIChat{}, "openai_chat_stream_no_usage.sse")

	assertField(t, "UsageKnown", got.UsageKnown, false)
	assertField(t, "InputTokens", got.InputTokens, int64(0))
	assertField(t, "FinishReasons", got.FinishReasons, []string{"stop"})
}

// Anthropic announces input tokens once in message_start and then revises a
// cumulative output total in message_delta. Two mistakes are easy here:
// letting the delta's absent input count zero out the real one, and adding the
// cumulative totals instead of replacing them.
func TestAnthropicStreamMergesSplitUsage(t *testing.T) {
	got := replayFixture(t, Anthropic{}, "anthropic_stream.sse")

	assertField(t, "ResponseID", got.ResponseID, "msg_01XYZ")
	assertField(t, "ResponseModel", got.ResponseModel, "claude-sonnet-4-5")
	assertField(t, "InputTokens", got.InputTokens, int64(2048))
	assertField(t, "OutputTokens", got.OutputTokens, int64(128))
	assertField(t, "CachedInputTokens", got.CachedInputTokens, int64(1024))
	assertField(t, "UsageKnown", got.UsageKnown, true)
	assertField(t, "FinishReasons", got.FinishReasons, []string{"end_turn"})
}

func TestParseResponseNonStreaming(t *testing.T) {
	tests := []struct {
		name   string
		parser Parser
		body   string
		want   CallInfo
	}{
		{
			name:   "openai responses",
			parser: OpenAIResponses{},
			body:   `{"id":"resp_1","model":"gpt-5-codex","status":"completed","usage":{"input_tokens":5,"output_tokens":9,"input_tokens_details":{"cached_tokens":2}}}`,
			want: CallInfo{
				ResponseID: "resp_1", ResponseModel: "gpt-5-codex",
				InputTokens: 5, OutputTokens: 9, CachedInputTokens: 2,
				UsageKnown: true, FinishReasons: []string{"completed"},
			},
		},
		{
			name:   "openai chat completions",
			parser: OpenAIChat{},
			body:   `{"id":"chatcmpl-3","model":"gpt-4.1","choices":[{"finish_reason":"length"}],"usage":{"prompt_tokens":11,"completion_tokens":3}}`,
			want: CallInfo{
				ResponseID: "chatcmpl-3", ResponseModel: "gpt-4.1",
				InputTokens: 11, OutputTokens: 3,
				UsageKnown: true, FinishReasons: []string{"length"},
			},
		},
		{
			name:   "anthropic messages",
			parser: Anthropic{},
			body:   `{"id":"msg_2","type":"message","model":"claude-sonnet-4-5","stop_reason":"max_tokens","usage":{"input_tokens":31,"output_tokens":17,"cache_read_input_tokens":8}}`,
			want: CallInfo{
				ResponseID: "msg_2", ResponseModel: "claude-sonnet-4-5",
				InputTokens: 31, OutputTokens: 17, CachedInputTokens: 8,
				UsageKnown: true, FinishReasons: []string{"max_tokens"},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var got CallInfo
			tc.parser.ParseResponse([]byte(tc.body), &got)

			assertField(t, "ResponseID", got.ResponseID, tc.want.ResponseID)
			assertField(t, "ResponseModel", got.ResponseModel, tc.want.ResponseModel)
			assertField(t, "InputTokens", got.InputTokens, tc.want.InputTokens)
			assertField(t, "OutputTokens", got.OutputTokens, tc.want.OutputTokens)
			assertField(t, "CachedInputTokens", got.CachedInputTokens, tc.want.CachedInputTokens)
			assertField(t, "UsageKnown", got.UsageKnown, tc.want.UsageKnown)
			assertField(t, "FinishReasons", got.FinishReasons, tc.want.FinishReasons)
		})
	}
}

// Garbage must be shrugged off: an unparseable body is an observability gap,
// never a reason to disturb the call.
func TestParsersTolerateMalformedPayloads(t *testing.T) {
	parsers := []Parser{OpenAIResponses{}, OpenAIChat{}, Anthropic{}}
	payloads := [][]byte{nil, {}, []byte("not json"), []byte(`{"usage":`), []byte(`[]`)}

	for _, p := range parsers {
		for _, payload := range payloads {
			var info CallInfo
			p.ParseResponse(payload, &info)
			p.ParseStreamEvent(sse.Event{Data: payload}, &info)

			if info.UsageKnown {
				t.Errorf("%T: UsageKnown set from payload %q", p, payload)
			}
		}
	}
}

func TestParseRequest(t *testing.T) {
	got, err := OpenAIResponses{}.ParseRequest([]byte(`{"model":"gpt-5-codex","stream":true,"input":"hi"}`))
	if err != nil {
		t.Fatalf("ParseRequest: %v", err)
	}
	assertField(t, "Model", got.Model, "gpt-5-codex")
	assertField(t, "Stream", got.Stream, true)

	if _, err := (OpenAIChat{}).ParseRequest([]byte("{oops")); err == nil {
		t.Error("ParseRequest accepted malformed JSON, want an error")
	}
}

func assertField(t *testing.T, name string, got, want any) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s = %v, want %v", name, got, want)
	}
}

func TestInjectStreamUsage(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		want     string
		wantEdit bool
	}{
		{
			name:     "streaming request gets the flag",
			body:     `{"model":"gpt-4.1","stream":true}`,
			want:     `{"model":"gpt-4.1","stream":true,"stream_options":{"include_usage":true}}`,
			wantEdit: true,
		},
		{
			name:     "existing stream_options are extended, not replaced",
			body:     `{"model":"gpt-4.1","stream":true,"stream_options":{"other":1}}`,
			want:     `{"model":"gpt-4.1","stream":true,"stream_options":{"include_usage":true,"other":1}}`,
			wantEdit: true,
		},
		{
			name: "an explicit false is the caller's decision and stands",
			body: `{"model":"gpt-4.1","stream":true,"stream_options":{"include_usage":false}}`,
			want: `{"model":"gpt-4.1","stream":true,"stream_options":{"include_usage":false}}`,
		},
		{
			name: "non-streaming requests report usage anyway",
			body: `{"model":"gpt-4.1","stream":false}`,
			want: `{"model":"gpt-4.1","stream":false}`,
		},
		{
			name: "absent stream field is left alone",
			body: `{"model":"gpt-4.1"}`,
			want: `{"model":"gpt-4.1"}`,
		},
		{
			name: "malformed bodies pass through untouched",
			body: `{oops`,
			want: `{oops`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, edited := InjectStreamUsage([]byte(tc.body))

			if edited != tc.wantEdit {
				t.Errorf("edited = %v, want %v", edited, tc.wantEdit)
			}
			if !tc.wantEdit {
				// Untouched bodies must be forwarded byte for byte.
				if string(got) != tc.body {
					t.Errorf("body was rewritten: got %s, want %s", got, tc.body)
				}
				return
			}
			assertJSONEqual(t, got, tc.want)
		})
	}
}

// assertJSONEqual compares by value, since Go marshals map keys in sorted
// order and the field order is not part of the contract.
func assertJSONEqual(t *testing.T, got []byte, want string) {
	t.Helper()

	var gotVal, wantVal any
	if err := json.Unmarshal(got, &gotVal); err != nil {
		t.Fatalf("result is not valid JSON: %v (%s)", err, got)
	}
	if err := json.Unmarshal([]byte(want), &wantVal); err != nil {
		t.Fatalf("want is not valid JSON: %v", err)
	}
	if !reflect.DeepEqual(gotVal, wantVal) {
		t.Errorf("got %s, want %s", got, want)
	}
}

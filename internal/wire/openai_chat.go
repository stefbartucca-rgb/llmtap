package wire

import (
	"bytes"
	"encoding/json"

	"github.com/stefbartucca-rgb/llmtap/internal/sse"
)

// doneSentinel terminates a Chat Completions stream and is not JSON.
var doneSentinel = []byte("[DONE]")

// OpenAIChat parses the OpenAI Chat Completions API (/v1/chat/completions),
// and by extension the many providers that copy its shape.
//
// It has one wrinkle the other formats do not: a streaming call reports no
// token counts at all unless the request opted in via stream_options. See
// InjectStreamUsage.
type OpenAIChat struct{}

func (OpenAIChat) Provider() Provider   { return ProviderOpenAI }
func (OpenAIChat) Operation() Operation { return OperationChat }

func (OpenAIChat) ParseRequest(body []byte) (RequestInfo, error) {
	return parseCommonRequest(body)
}

type chatUsage struct {
	PromptTokens        int64 `json:"prompt_tokens"`
	CompletionTokens    int64 `json:"completion_tokens"`
	PromptTokensDetails *struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
}

// chatCompletion matches both a complete response and a single stream chunk:
// the fields llmtap reads sit in the same place in either.
type chatCompletion struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Choices []struct {
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *chatUsage `json:"usage"`
}

func (OpenAIChat) ParseResponse(body []byte, info *CallInfo) {
	applyChatCompletion(body, info)
}

func (OpenAIChat) ParseStreamEvent(ev sse.Event, info *CallInfo) {
	if bytes.Equal(bytes.TrimSpace(ev.Data), doneSentinel) {
		return
	}
	applyChatCompletion(ev.Data, info)
}

func applyChatCompletion(data []byte, info *CallInfo) {
	var obj chatCompletion
	if err := json.Unmarshal(data, &obj); err != nil {
		return
	}

	if obj.ID != "" {
		info.ResponseID = obj.ID
	}
	if obj.Model != "" {
		info.ResponseModel = obj.Model
	}
	for _, choice := range obj.Choices {
		addFinishReason(info, choice.FinishReason)
	}

	// Streamed chunks carry "usage": null until the final one, so a nil here
	// is the normal case rather than a problem.
	if obj.Usage == nil {
		return
	}
	info.InputTokens = obj.Usage.PromptTokens
	info.OutputTokens = obj.Usage.CompletionTokens
	if d := obj.Usage.PromptTokensDetails; d != nil {
		info.CachedInputTokens = d.CachedTokens
	}
	info.UsageKnown = true
}

// InjectStreamUsage sets stream_options.include_usage on a streaming Chat
// Completions request, which is the only way to make the API report token
// counts for a stream at all. Without it there is nothing to measure.
//
// It returns the body unchanged when the request is not streaming, when the
// caller already made a choice, or when anything about the body is
// unexpected — a request that reaches upstream intact matters more than a
// metric.
//
// The visible effect on the client is one extra final chunk carrying the usage
// object, which is why the proxy exposes this as a config switch.
func InjectStreamUsage(body []byte) ([]byte, bool) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return body, false
	}

	var streaming bool
	if err := json.Unmarshal(raw["stream"], &streaming); err != nil || !streaming {
		return body, false
	}
	if existing, ok := raw["stream_options"]; ok && !bytes.Equal(bytes.TrimSpace(existing), []byte("null")) {
		var opts map[string]json.RawMessage
		if err := json.Unmarshal(existing, &opts); err != nil {
			return body, false
		}
		if _, set := opts["include_usage"]; set {
			return body, false // the caller decided; do not override it
		}
		opts["include_usage"] = json.RawMessage("true")
		return remarshal(raw, "stream_options", opts, body)
	}

	return remarshal(raw, "stream_options", map[string]json.RawMessage{
		"include_usage": json.RawMessage("true"),
	}, body)
}

func remarshal(raw map[string]json.RawMessage, key string, value any, original []byte) ([]byte, bool) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return original, false
	}
	raw[key] = encoded

	out, err := json.Marshal(raw)
	if err != nil {
		return original, false
	}
	return out, true
}

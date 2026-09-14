package wire

import (
	"encoding/json"

	"github.com/stefbartucca-rgb/llmtap/internal/sse"
)

// Anthropic parses the Anthropic Messages API (/v1/messages), which is what
// Claude Code speaks.
//
// Its streaming format splits usage across two events: message_start knows the
// input tokens immediately, message_delta revises the output count as the
// response grows. The output figure is cumulative, so each update replaces the
// previous one rather than adding to it.
type Anthropic struct{}

func (Anthropic) Provider() Provider   { return ProviderAnthropic }
func (Anthropic) Operation() Operation { return OperationChat }

func (Anthropic) ParseRequest(body []byte) (RequestInfo, error) {
	return parseCommonRequest(body)
}

type anthropicUsage struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
}

type anthropicMessage struct {
	ID         string          `json:"id"`
	Model      string          `json:"model"`
	StopReason string          `json:"stop_reason"`
	Usage      *anthropicUsage `json:"usage"`
}

type anthropicEvent struct {
	Type string `json:"type"`

	// message_start nests the message; message_delta puts the stop reason in
	// delta and a partial usage object at the top level.
	Message *anthropicMessage `json:"message"`
	Delta   *struct {
		StopReason string `json:"stop_reason"`
	} `json:"delta"`
	Usage *anthropicUsage `json:"usage"`

	Error *struct {
		Type string `json:"type"`
	} `json:"error"`
}

func (Anthropic) ParseResponse(body []byte, info *CallInfo) {
	var msg anthropicMessage
	if err := json.Unmarshal(body, &msg); err != nil {
		return
	}
	applyAnthropicMessage(&msg, info)
}

func (Anthropic) ParseStreamEvent(ev sse.Event, info *CallInfo) {
	var parsed anthropicEvent
	if err := json.Unmarshal(ev.Data, &parsed); err != nil {
		return
	}

	if parsed.Message != nil {
		applyAnthropicMessage(parsed.Message, info)
	}
	if parsed.Delta != nil {
		addFinishReason(info, parsed.Delta.StopReason)
	}
	if parsed.Error != nil {
		addFinishReason(info, parsed.Error.Type)
	}

	// message_delta reports the running output total. Input tokens are only
	// ever announced in message_start, so a zero here must not clobber them.
	if u := parsed.Usage; u != nil {
		info.OutputTokens = u.OutputTokens
		if u.InputTokens > 0 {
			info.InputTokens = u.InputTokens
		}
		info.UsageKnown = true
	}
}

func applyAnthropicMessage(msg *anthropicMessage, info *CallInfo) {
	if msg.ID != "" {
		info.ResponseID = msg.ID
	}
	if msg.Model != "" {
		info.ResponseModel = msg.Model
	}
	addFinishReason(info, msg.StopReason)

	if msg.Usage == nil {
		return
	}
	info.InputTokens = msg.Usage.InputTokens
	info.OutputTokens = msg.Usage.OutputTokens
	// Anthropic reports cache reads separately from input_tokens; both were
	// billed, at different rates, so pricing needs them kept apart.
	info.CachedInputTokens = msg.Usage.CacheReadInputTokens
	info.UsageKnown = true
}

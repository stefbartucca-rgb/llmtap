package wire

import (
	"encoding/json"

	"github.com/stefbartucca-rgb/llmtap/internal/sse"
)

// OpenAIResponses parses the OpenAI Responses API (/v1/responses).
//
// This is the format that matters most for llmtap: Codex CLI defaults to
// wire_api = "responses", so anything it does arrives here.
type OpenAIResponses struct{}

func (OpenAIResponses) Provider() Provider   { return ProviderOpenAI }
func (OpenAIResponses) Operation() Operation { return OperationChat }

func (OpenAIResponses) ParseRequest(body []byte) (RequestInfo, error) {
	return parseCommonRequest(body)
}

// responsesUsage is reported once, when the response reaches a terminal state.
type responsesUsage struct {
	InputTokens        int64 `json:"input_tokens"`
	OutputTokens       int64 `json:"output_tokens"`
	InputTokensDetails *struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"input_tokens_details"`
}

type responsesObject struct {
	ID                string `json:"id"`
	Model             string `json:"model"`
	Status            string `json:"status"`
	IncompleteDetails *struct {
		Reason string `json:"reason"`
	} `json:"incomplete_details"`
	Usage *responsesUsage `json:"usage"`
}

// responsesEvent is the streaming envelope. Terminal events nest the same
// response object that a non-streaming call returns as its whole body.
type responsesEvent struct {
	Type     string           `json:"type"`
	Response *responsesObject `json:"response"`
}

func (OpenAIResponses) ParseResponse(body []byte, info *CallInfo) {
	var obj responsesObject
	if err := json.Unmarshal(body, &obj); err != nil {
		return
	}
	applyResponsesObject(&obj, info)
}

func (OpenAIResponses) ParseStreamEvent(ev sse.Event, info *CallInfo) {
	var parsed responsesEvent
	if err := json.Unmarshal(ev.Data, &parsed); err != nil {
		return
	}
	if parsed.Response != nil {
		applyResponsesObject(parsed.Response, info)
	}
}

// applyResponsesObject is shared by both paths because the Responses API sends
// the identical object either way — once as the body, or wrapped in the
// response.created and response.completed events.
func applyResponsesObject(obj *responsesObject, info *CallInfo) {
	if obj.ID != "" {
		info.ResponseID = obj.ID
	}
	if obj.Model != "" {
		info.ResponseModel = obj.Model
	}

	// An incomplete response explains itself in incomplete_details; for every
	// other outcome the status is the most specific thing available.
	switch {
	case obj.IncompleteDetails != nil && obj.IncompleteDetails.Reason != "":
		addFinishReason(info, obj.IncompleteDetails.Reason)
	case obj.Status != "" && obj.Status != "in_progress":
		addFinishReason(info, obj.Status)
	}

	if obj.Usage == nil {
		return
	}
	info.InputTokens = obj.Usage.InputTokens
	info.OutputTokens = obj.Usage.OutputTokens
	if d := obj.Usage.InputTokensDetails; d != nil {
		info.CachedInputTokens = d.CachedTokens
	}
	info.UsageKnown = true
}

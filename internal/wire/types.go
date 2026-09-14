// Package wire normalizes the request and response formats of the different
// model APIs into a single shape. Everything provider-specific lives here, so
// that the telemetry package never has to know whether a call went to OpenAI
// or Anthropic.
package wire

import "github.com/stefbartucca-rgb/llmtap/internal/sse"

// Provider is reported as the gen_ai.provider.name attribute.
type Provider string

const (
	ProviderOpenAI    Provider = "openai"
	ProviderAnthropic Provider = "anthropic"
)

// Operation is reported as the gen_ai.operation.name attribute. The
// conventions define more values (embeddings, execute_tool, ...); llmtap only
// proxies chat traffic today.
type Operation string

const OperationChat Operation = "chat"

// RequestInfo is what can be known before the call leaves for upstream. The
// tags are shared by all three wire formats, which is why one type covers
// them.
type RequestInfo struct {
	Model  string `json:"model"`
	Stream bool   `json:"stream"`
}

// CallInfo accumulates everything observed about a single model call. A parser
// fills it in progressively: streaming responses reveal the model early and
// the token counts only at the very end.
type CallInfo struct {
	Provider  Provider
	Operation Operation

	RequestModel  string
	ResponseModel string
	ResponseID    string

	InputTokens       int64
	OutputTokens      int64
	CachedInputTokens int64

	// UsageKnown stays false when the upstream never reported token counts.
	// The conventions are explicit that instrumentation MUST NOT report usage
	// it cannot obtain reliably, so this gates the token and cost metrics
	// rather than defaulting them to zero.
	UsageKnown bool

	FinishReasons []string
	Streaming     bool
}

// Parser understands one wire format. Adding a provider means adding one
// implementation, not touching the proxy or the telemetry layer.
type Parser interface {
	// Provider and Operation describe every call this parser handles.
	Provider() Provider
	Operation() Operation

	// ParseRequest inspects the outgoing request body.
	ParseRequest(body []byte) (RequestInfo, error)

	// ParseResponse handles a complete non-streaming response body.
	ParseResponse(body []byte, info *CallInfo)

	// ParseStreamEvent is fed each SSE event in the order it arrives.
	ParseStreamEvent(ev sse.Event, info *CallInfo)
}

// addFinishReason records a finish reason, ignoring blanks and duplicates.
// Chat Completions reports one per choice, so repeats are the normal case.
func addFinishReason(info *CallInfo, reason string) {
	if reason == "" {
		return
	}
	for _, existing := range info.FinishReasons {
		if existing == reason {
			return
		}
	}
	info.FinishReasons = append(info.FinishReasons, reason)
}

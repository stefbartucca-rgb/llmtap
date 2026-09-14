package telemetry

// Attribute and instrument names from the OpenTelemetry GenAI semantic
// conventions.
//
// These are deliberately hand-declared in one place instead of pulled from
// go.opentelemetry.io/otel/semconv. The GenAI conventions moved out of the
// main semantic-conventions repository into semantic-conventions-genai and are
// still at "Development" stability — gen_ai.system was already renamed to
// gen_ai.provider.name. Keeping every name in a single file means the next
// rename is a diff you can read in one screen.
//
// Spec: https://github.com/open-telemetry/semantic-conventions-genai
const (
	// Span and metric attributes.
	attrOperationName = "gen_ai.operation.name"
	attrProviderName  = "gen_ai.provider.name"
	attrRequestModel  = "gen_ai.request.model"
	attrResponseModel = "gen_ai.response.model"
	attrResponseID    = "gen_ai.response.id"
	attrInputTokens   = "gen_ai.usage.input_tokens"
	attrOutputTokens  = "gen_ai.usage.output_tokens"
	attrFinishReasons = "gen_ai.response.finish_reasons"
	attrTokenType     = "gen_ai.token.type"

	// Shared conventions, stable.
	attrServerAddress = "server.address"
	attrServerPort    = "server.port"
	attrErrorType     = "error.type"

	// Instruments.
	metricTokenUsage  = "gen_ai.client.token.usage"
	metricOpDuration  = "gen_ai.client.operation.duration"
	metricTimeToFirst = "gen_ai.client.operation.time_to_first_chunk"

	// llmtap's own instruments live under their own prefix. Cost is not part
	// of the conventions, and squatting the gen_ai.* namespace with invented
	// names while the spec is still moving would guarantee a collision later.
	metricCostUSD = "llmtap.cost.usd"

	// Values for attrTokenType.
	tokenTypeInput  = "input"
	tokenTypeOutput = "output"

	// errorTypeOther is the conventions' catch-all for an error that has no
	// more specific classification.
	errorTypeOther = "_OTHER"
)

package wire

import "encoding/json"

// commonRequest covers the two fields every chat API spells the same way.
type commonRequest struct {
	Model  string `json:"model"`
	Stream bool   `json:"stream"`
}

// parseCommonRequest reads model and stream from a request body. A malformed
// body is not rejected here: the upstream is the authority on what is valid,
// and llmtap must never turn an observability problem into a failed request.
func parseCommonRequest(body []byte) (RequestInfo, error) {
	var req commonRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return RequestInfo{}, err
	}
	return RequestInfo{Model: req.Model, Stream: req.Stream}, nil
}

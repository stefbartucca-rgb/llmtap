package wire

import "encoding/json"

// parseCommonRequest reads the two fields every chat API spells the same way.
// A malformed body is not rejected here: the upstream is the authority on what
// is valid, and llmtap must never turn an observability problem into a failed
// request.
func parseCommonRequest(body []byte) (RequestInfo, error) {
	var req RequestInfo
	if err := json.Unmarshal(body, &req); err != nil {
		return RequestInfo{}, err
	}
	return req, nil
}

package anthropic

import (
	"encoding/json"

	"github.com/zereker/llm-gateway/pkg/protocol"
	"github.com/zereker/llm-gateway/pkg/domain"
)

// Classify implements protocol.Classifier, overriding DefaultClassifier to refine
// classification for the Anthropic protocol family.
//
// **Anthropic error JSON shape** (top-level type=error):
//
//	{ "type": "error", "error": { "type": "...", "message": "..." } }
//
// **error.type enum (per Anthropic API docs)**:
//   - invalid_request_error  → client error (4xx)
//   - authentication_error   → 401 (invalid key)
//   - permission_error       → 403
//   - not_found_error        → 404
//   - rate_limit_error       → 429
//   - api_error              → 5xx upstream internal error
//   - overloaded_error       → 529 / 5xx capacity error (should map to ErrRateLimit/capacity,
//     not a short transient cooldown, otherwise it will hammer the upstream)
//
// **Refinement rules** (judgment beyond HTTP status alone):
//   - error.type=overloaded_error  → ErrRateLimit (capacity class, cooldown should be longer)
//   - error.type=invalid_request_error → ErrInvalid (treated as a client error even if status is 5xx)
//   - otherwise: fall through to DefaultClassifier
//
// **On body parse failure / truncation**: fall back to DefaultClassifier.
func (Factory) Classify(httpStatus int, body []byte) *domain.AdapterError {
	base := protocol.DefaultClassifier{}.Classify(httpStatus, body)

	if len(body) == 0 {
		return base
	}
	var probe struct {
		Type  string `json:"type"`
		Error *struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return base
	}
	if probe.Error == nil {
		return base
	}
	if probe.Error.Message != "" {
		base.UpstreamMessage = probe.Error.Message
	}

	switch probe.Error.Type {
	case "overloaded_error":
		base.Class = domain.ErrRateLimit
	case "invalid_request_error":
		base.Class = domain.ErrInvalid
	case "authentication_error", "permission_error":
		base.Class = domain.ErrPermanent
	}
	return base
}

// Compile-time assertion that Factory implements protocol.Classifier.
var _ protocol.Classifier = Factory{}

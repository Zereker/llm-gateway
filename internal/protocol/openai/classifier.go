package openai

import (
	"encoding/json"

	"github.com/zereker/llm-gateway/internal/domain"
	"github.com/zereker/llm-gateway/internal/protocol"
)

// Classify implements protocol.Classifier, refining DefaultClassifier's
// classification for the OpenAI protocol family.
//
// **OpenAI error JSON shape**:
//
//	{ "error": { "type": "...", "code": "...", "message": "...", "param": null } }
//
// **Refinement rules** (beyond plain HTTP status):
//   - 429 + code="insufficient_quota"  → ErrPermanent (account quota exhausted,
//     long cooldown; should not be retried after a few seconds like a transient
//     rate-limit)
//   - 429 + code="rate_limit_exceeded" → ErrRateLimit (default behavior, but
//     tagged explicitly)
//   - 400 + code="context_length_exceeded" → ErrInvalid (client request too
//     long, switching endpoint won't help)
//   - 401 + type="invalid_api_key"     → ErrPermanent (401 already defaults to
//     Permanent, but fill in UpstreamMessage accurately)
//   - otherwise: falls through to DefaultClassifier's status-only classification
//
// **When body parsing fails** (invalid JSON / truncated): falls back to
// DefaultClassifier without erroring.
func (Factory) Classify(httpStatus int, body []byte) *domain.AdapterError {
	base := protocol.DefaultClassifier{}.Classify(httpStatus, body)

	if len(body) == 0 {
		return base
	}

	var probe struct {
		Error *struct {
			Type    string `json:"type"`
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &probe); err != nil || probe.Error == nil {
		return base
	}
	// Propagate the upstream's raw message into base (overriding
	// DefaultClassifier's full string(body))
	if probe.Error.Message != "" {
		base.UpstreamMessage = probe.Error.Message
	}

	switch httpStatus {
	case 429:
		switch probe.Error.Code {
		case "insufficient_quota":
			base.Class = domain.ErrPermanent
		case "rate_limit_exceeded", "":
			base.Class = domain.ErrRateLimit
		}
	case 400:
		if probe.Error.Code == "context_length_exceeded" {
			base.Class = domain.ErrInvalid
		}
	}

	return base
}

// Compile-time assertion that Factory implements protocol.Classifier.
var _ protocol.Classifier = Factory{}

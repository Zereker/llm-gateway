package middleware

import (
	"errors"
	"io"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"go.opentelemetry.io/otel"

	"github.com/zereker/llm-gateway/pkg/domain"
	"github.com/zereker/llm-gateway/pkg/protocol"
)

// WithSourceProtocol pins the client protocol at route-registration time.
//
// The route itself tells downstream "this path = which protocol", so Envelope
// no longer needs to do path-based heuristic matching, and renaming a path /
// adding a path / reusing the same prefix across multiple protocols can no
// longer break due to Detector misjudgment.
//
// Call order: must come before Envelope. This middleware pre-creates a
// RequestEnvelope shell on the RequestContext (filling only SourceProtocol /
// Modality); the Envelope middleware fills in RawBytes / Parsed / RequestTime
// afterward.
//
// proto == ProtoUnknown is treated as invalid (it should be unambiguous at
// route-registration time); no fallback is applied.
func WithSourceProtocol(proto domain.Protocol, mod domain.Modality) gin.HandlerFunc {
	return func(c *gin.Context) {
		rc := GetRequestContext(c)
		if rc.Envelope == nil {
			rc.Envelope = &domain.RequestEnvelope{}
		}
		rc.Envelope.SourceProtocol = proto
		rc.Envelope.Modality = mod
		c.Next()
	}
}

// Envelope is M3: reads the body → extracts model → writes rc.Envelope.
//
// **Responsibilities are strictly narrowed**:
//   - Reads the body once, storing it in rc.Envelope.RawBytes for downstream to share
//   - Extracts the top-level `model` field from the body
//   - Writes rc.Envelope.{RawBytes, Model}
//
// **Does NOT** do the following (clear responsibility boundary):
//   - Parameter parsing / field translation — delegated to pkg/translator/<src>_<tgt>/
//   - Body validity checking — the translator can just fail inside TranslateRequest
//   - Stream detection — the translator reads the stream field from the body itself
//
// **Does not reset c.Request.Body**: all body consumers (M8 / M6 / M7 / token
// estimator / translator) go through rc.Envelope.RawBytes; the adapter has
// already been slimmed down to only build the HTTP request from the translator
// output, never reading c.Request.Body. With zero real consumers left, resetting
// via NopCloser would just be noise.
//
// Failure behavior (all go through abort → M9 writes out JSON):
//   - Route forgot to attach WithSourceProtocol → 500 / ErrUnknown
//   - Reading the body failed → 400 / ErrInvalid / "envelope: read body: <err>"
//   - Missing model field → 400 / ErrInvalid / "envelope: ..."
func Envelope() gin.HandlerFunc {
	tracer := otel.GetTracerProvider().Tracer(ScopeName)

	return func(c *gin.Context) {
		ctx, span := tracer.Start(c.Request.Context(), "envelope.parse")
		defer span.End()
		c.Request = c.Request.WithContext(ctx)

		rc := GetRequestContext(c)
		if rc.Envelope == nil || rc.Envelope.SourceProtocol == domain.ProtoUnknown {
			abort(c, 500, domain.ErrUnknown, "envelope: WithSourceProtocol middleware missing")
			return
		}

		raw, err := io.ReadAll(c.Request.Body)
		if err != nil {
			abort(c, 400, domain.ErrInvalid, "envelope: read body: "+err.Error())
			return
		}
		_ = c.Request.Body.Close()

		model, err := extractModel(raw)
		if err != nil {
			abort(c, 400, domain.ErrInvalid, "envelope: "+err.Error())
			return
		}

		rc.Envelope.RawBytes = raw
		rc.Envelope.Model = model

		// Request-level lookup default: wraps the global adapter + translator
		// registry, dynamically composing a Handler by (endpoint, srcProto).
		// Later middleware (e.g. multi-tenant / canary policies) can override
		// rc.Handlers with a custom Handler set.
		if rc.Handlers == nil {
			rc.Handlers = protocol.DefaultLookup{}
		}

		c.Next()
	}
}

// extractModel extracts the top-level `model` field from the client body.
//
// All three supported client protocols (OpenAI Chat / Anthropic Messages /
// OpenAI Responses) use `model` as the top-level field name, so no per-protocol
// dispatch is needed.
//
// **Uses gjson** (not encoding/json): schema-less extraction of a single field,
// skipping the unmarshal of the whole messages / tools array — ~5x faster, 1
// alloc (vs 3 stdlib allocs) on a typical 4KB chat body. stdlib `json.Unmarshal`
// is schema-based and fully tokenizes; pure waste for this use case.
func extractModel(raw []byte) (string, error) {
	if len(raw) == 0 {
		return "", errors.New("empty body")
	}
	res := gjson.GetBytes(raw, "model")
	if !res.Exists() {
		return "", errors.New("missing 'model' field")
	}
	model := res.String()
	if model == "" {
		return "", errors.New("'model' field is empty")
	}
	return model, nil
}

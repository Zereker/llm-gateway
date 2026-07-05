// Package protocol defines the end-to-end protocol handler Handler — fusing
// "vendor HTTP layer + protocol body conversion" into a single abstraction.
//
// **Architecture position**:
//
//	┌──────────┐                                           ┌──────────┐
//	│ Client   │ ────────── Handler ──────────────────── → │ Upstream │
//	│ (proto X)│  PrepareCall (pre-call protocol conversion)│ (proto Y)│
//	│          │  ↓ translate body X→Y                    │          │
//	│          │  ↓ build HTTP request (vendor-specific)  │          │
//	│          │                                           │          │
//	│          │ ← NewResponseStream (post-call protocol conversion) │
//	│          │  ↓ chunk-by-chunk Y→X                    │          │
//	└──────────┘                                           └──────────┘
//
// **Difference from v0.5 (split adapter + translator)**:
//
//	v0.5: two independent abstractions (adapter + translator), the consumer side
//	      does two lookups + a match check; protocol.Metadata.NativeProtocol
//	      hard-codes vendor → upstream protocol.
//	v0.6: a single Handler abstraction (facade), dynamically composed by
//	      DefaultLookup from (endpoint, srcProto). The endpoint carries a
//	      Protocol field — the same vendor can have multiple endpoints on
//	      different protocols.
//
// **When composition happens**: per-request, not in init(). DefaultLookup.Get(ep, srcProto) calls:
//  1. protocol.LookupFactory(ep.Vendor) → protocol.Factory (vendor HTTP implementation)
//  2. translator.Find(srcProto, ep.Protocol) → translator.Translator (body conversion)
//  3. Combine(ad, tr) → Handler
//
// If either is missing → return nil → the eligibility filter excludes that endpoint.
package protocol

import (
	"context"
	"net/http"

	"github.com/zereker/llm-gateway/pkg/domain"
)

// Capabilities is Handler's runtime metadata — describes this composition's
// (srcProto, ep.Protocol) + the modalities the adapter supports.
//
// **Note**: Vendor is not here — it's a property of the endpoint, not the
// Handler; Handler is a dynamic (adapter, translator) composition, and its
// one-to-one correspondence with a specific endpoint is only fixed at
// PrepareCall time.
//
// Used for: metric labels / debug logs / eligibility capability filtering.
type Capabilities struct {
	SourceProtocol      domain.Protocol   // the client protocol this Handler accepts
	UpstreamProtocol    domain.Protocol   // the protocol the endpoint's upstream uses (from ep.Protocol)
	SupportedModalities []domain.Modality // modalities the adapter supports
}

// Call is PrepareCall's output — the HTTP request ready to send upstream +
// a copy of the translated body.
//
// **UpstreamBody field**: used by the caller to fan out to audit / observer
// hooks (audit scenarios need to "log before sending" the upstream bytes).
// Request.Body has already consumed these bytes; UpstreamBody is an
// independent copy for observability — so even for large bodies the footprint
// is only ~2x, without re-consuming the Reader.
type Call struct {
	Request      *http.Request
	UpstreamBody []byte
}

// Handler is an end-to-end protocol processor for one (vendor, sourceProtocol) pair.
//
// **Responsibilities**:
//   - PrepareCall: pre-call protocol conversion — translates the client body
//     into the upstream protocol, wraps it into a vendor-specific HTTP request
//   - NewResponseStream: post-call protocol conversion — translates the response
//     back to the client chunk-by-chunk
//
// **Concurrency constraint**: Handler instances MUST be safe for concurrent use
// (multiple gin handlers call PrepareCall / NewResponseStream concurrently).
// The ResponseStream returned by NewResponseStream is a per-request, new,
// single-goroutine handle.
type Handler interface {
	Capabilities() Capabilities

	// PrepareCall converts + wraps the client's raw body into an HTTP request
	// that can be sent upstream.
	//
	// Two internal steps:
	//   1. translator.TranslateRequest(srcBody) → upstreamBody
	//   2. adapter session BuildRequest(upstreamBody) → *http.Request (URL / auth / headers)
	//
	// Failure classification:
	//   - PrepareError{Phase: PhaseTranslate}: srcBody doesn't match the
	//     SourceProtocol schema; the caller should abort (retrying with a
	//     different endpoint would fail the same way)
	//   - PrepareError{Phase: PhaseBuild}: vendor HTTP construction failed
	//     (rare; usually an invalid endpoint config)
	PrepareCall(ctx context.Context, ep *domain.Endpoint, srcBody []byte) (*Call, error)

	// NewResponseStream is one per request; consumes upstream response chunks →
	// emits client chunks.
	//
	// Single goroutine (same goroutine as the gin handler).
	NewResponseStream() ResponseStream
}

// ResponseStream handles one upstream response: fed chunk-by-chunk; Flush
// produces the final output.
//
// **Streaming mode** (identity): Feed returns the chunk directly; usage is
// parsed during the Feed phase; Flush returns nil bytes + the accumulated usage.
//
// **Buffer-then-translate mode** (cross-protocol): Feed accumulates everything,
// returns nil; Flush translates the accumulated body once and returns the full
// client-format body + usage.
type ResponseStream interface {
	Feed(chunk []byte) (clientBytes []byte, err error)
	Flush() (clientBytes []byte, usage *domain.Usage, err error)
}

// Classifier is an optional interface: lets a vendor refine an error response
// body down to a domain.ErrorClass.
//
// The invoker calls it via type-assert on non-2xx HTTP: e.g. OpenAI
// distinguishes insufficient_quota (permanent) from a real rate-limit
// (capacity); Anthropic's 529 overloaded_error → capacity.
//
// **Typical use cases**: when HTTP status alone isn't fine-grained enough
//   - Same 429: OpenAI distinguishes insufficient_quota (permanent) vs a real rate-limit (capacity)
//   - 200 + error body: a few vendors stuff errors into a 200 response, which HTTP-status-only can't detect
//   - 5xx refinement: Anthropic's 529 overloaded_error should be treated as capacity, not transient
//
// **Contract**:
//   - implementations MUST be safe for concurrent use (multiple goroutines classify simultaneously)
//   - body param: implementations must not retain a reference to the slice — if
//     it needs to be stored in the returned AdapterError, string(body) / copy it
//   - body may be partial (M7 limit-reads 1KiB), implementations must tolerate truncated JSON
type Classifier interface {
	Classify(status int, body []byte) *domain.AdapterError
}

// DefaultClassifier classifies by HTTP status only. The fallback used when a
// Factory doesn't implement Classifier.
type DefaultClassifier struct{}

// Classify maps HTTP status to an ErrorClass.
func (DefaultClassifier) Classify(httpStatus int, body []byte) *domain.AdapterError {
	e := &domain.AdapterError{
		HTTPStatus:      httpStatus,
		UpstreamMessage: string(body),
	}
	switch {
	case httpStatus == 429:
		e.Class = domain.ErrRateLimit
	case httpStatus == 401, httpStatus == 403:
		e.Class = domain.ErrPermanent
	case httpStatus >= 400 && httpStatus < 500:
		e.Class = domain.ErrInvalid
	case httpStatus >= 500:
		e.Class = domain.ErrTransient
	default:
		e.Class = domain.ErrUnknown
	}
	return e
}

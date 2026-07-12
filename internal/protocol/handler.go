// Package protocol defines the end-to-end protocol handler abstraction,
// Handler — it fuses "vendor HTTP layer + protocol body conversion" into a
// single abstraction.
//
// **Architectural role**:
//
//	┌──────────┐                                           ┌──────────┐
//	│ Client   │ ────────── Handler ──────────────────── → │ Upstream │
//	│ (proto X)│  PrepareCall (pre-call protocol convert)  │ (proto Y)│
//	│          │  ↓ translate body X→Y                    │          │
//	│          │  ↓ build HTTP request (vendor-specific)  │          │
//	│          │                                           │          │
//	│          │ ← NewResponseStream (post-call protocol   │          │
//	│          │    convert) ─ ↓ chunk-by-chunk Y→X        │          │
//	└──────────┘                                           └──────────┘
//
// **Difference from v0.5 (split adapter + translator)**:
//
//	v0.5: two independent abstractions (adapter + translator); the consumer
//	      side did two lookups + a match check; protocol.Metadata.NativeProtocol
//	      hardcoded vendor → upstream protocol, locking them together.
//	v0.6: a single Handler abstraction (facade), dynamically composed by
//	      DefaultLookup from (endpoint, srcProto) → adapter + translator. The
//	      endpoint carries a Protocol field — the same vendor can back
//	      multiple endpoints on different protocols.
//
// **When composition happens**: at request time, not at startup.
// DefaultLookup.Get(ep, srcProto) calls:
//  1. factories[ep.Vendor] → protocol.Factory (vendor HTTP implementation)
//  2. translators.FindVia(srcProto, ep.Protocol, pivot) → translator.Translator (body conversion)
//  3. Combine(ad, tr) → Handler
//
// If either is missing → return nil → the eligibility filter excludes that endpoint.
package protocol

import (
	"context"
	"io"
	"net/http"

	"github.com/zereker/llm-gateway/internal/domain"
)

// Capabilities is Handler's runtime metadata — it describes the (srcProto,
// ep.Protocol) of this composition + the modalities the adapter supports.
//
// **Note**: Vendor is not here — it's a property of the endpoint, not the
// Handler; Handler is a dynamic (adapter, translator) composition, and its
// one-to-one correspondence with a specific endpoint is only determined at
// PrepareCall time.
//
// Use: metric labels / debug logs / eligibility capability filtering.
type Capabilities struct {
	SourceProtocol      domain.Protocol   // the client protocol this Handler accepts
	UpstreamProtocol    domain.Protocol   // the protocol used by the endpoint's upstream (from ep.Protocol)
	SupportedModalities []domain.Modality // modalities the adapter supports
}

// Call is the output of PrepareCall — the HTTP request ready to send
// upstream + a copy of the translated body.
//
// **UpstreamBody field**: used by callers to fan out to audit / observer
// hooks (audit scenarios need to "log before sending" the upstream bytes).
// Request.Body has already consumed these bytes; UpstreamBody is an
// independent copy for observability — so even for a large body the
// footprint is only ~2x, and the Reader is never consumed twice.
type Call struct {
	Request      *http.Request
	UpstreamBody []byte
}

// Handler is the end-to-end protocol handler for a (vendor, sourceProtocol) pair.
//
// **Responsibilities**:
//   - PrepareCall: pre-call protocol conversion — translates the client body
//     into the upstream protocol, wraps it into a vendor-specific HTTP request
//   - NewResponseStream: post-call protocol conversion — translates the
//     response back to the client, chunk by chunk
//
// **Concurrency constraint**: Handler instances MUST be safe for concurrent
// use (multiple gin handlers call PrepareCall / NewResponseStream
// concurrently). The ResponseStream returned by NewResponseStream is a
// per-request, single-goroutine handle.
type Handler interface {
	Capabilities() Capabilities

	// PrepareCall converts the client's raw body and wraps it into an HTTP
	// request ready to send upstream.
	//
	// Internally, two steps:
	//   1. translator.TranslateRequest(srcBody) → upstreamBody
	//   2. adapter session BuildRequest(upstreamBody) → *http.Request (URL / auth / headers)
	//
	// Failure classification:
	//   - PrepareError{Phase: PhaseTranslate}: srcBody doesn't match
	//     SourceProtocol's schema; the caller should abort (retrying with a
	//     different endpoint on the same request will also fail)
	//   - PrepareError{Phase: PhaseBuild}: vendor HTTP construction failed
	//     (rare; usually an invalid endpoint configuration)
	PrepareCall(ctx context.Context, ep *domain.Endpoint, srcBody []byte) (*Call, error)

	// NewResponseStream creates one instance per request; it consumes
	// upstream response chunks → emits client chunks.
	//
	// Single-goroutine (runs in the same goroutine as the gin handler).
	NewResponseStream() ResponseStream
}

// ResponseStream handles one upstream response: fed chunk-by-chunk; Flush
// produces the final output.
//
// **Streaming mode** (identity): Feed returns the chunk directly; usage is
// parsed during the Feed phase; Flush returns nil bytes + the accumulated usage.
//
// **Buffer-then-translate mode** (cross-protocol): Feed accumulates
// everything and returns nil; Flush translates the accumulated body once and
// returns the full client-format body + usage.
type ResponseStream interface {
	Feed(chunk []byte) (clientBytes []byte, err error)
	Flush() (clientBytes []byte, usage *domain.Usage, err error)
}

// TransportDecoder is an optional interface: the vendor decodes the upstream
// response's **transport framing** into the byte stream the protocol handler
// understands. Used when the wire transport format ≠ the protocol's SSE format.
//
// **Why a separate layer**: so far every provider's streaming is SSE / JSON,
// where the transport format happens to ≈ the protocol format, so a single
// ResponseStream conveniently handles both. AWS Bedrock's event-stream is
// binary framing (`vnd.amazon.eventstream`) that wraps Anthropic events
// inside frames — that's a **transport-layer** concern, distinct from
// protocol shape. TransportDecoder strips the framing **before** the bytes
// enter ResponseStream.Feed, restoring the bytes the protocol handler
// expects (e.g. Anthropic SSE). This lets Bedrock keep protocol=anthropic
// and reuse anthropic's ResponseStream for shape translation, cleanly
// separating transport from protocol.
//
// **Optional**: if the Factory doesn't implement it, upstream bytes go
// straight into the handler (the SSE/JSON case, the vast majority). The
// combined Handler automatically surfaces this Factory capability (same
// promotion pattern as Classifier).
type TransportDecoder interface {
	// DecodeTransport wraps resp.Body into an "already de-framed" reader;
	// returns nil when no decoding is needed (the caller uses this to decide
	// whether to insert a decode layer). The implementation is **not
	// responsible** for closing resp.Body (the caller owns that).
	DecodeTransport(resp *http.Response) io.Reader
}

// Classifier is an optional interface: lets a vendor refine an error
// response body into a domain.ErrorClass with custom logic.
//
// The invoker calls it via type-assert on non-2xx HTTP responses: e.g.
// OpenAI distinguishes insufficient_quota (permanent) from a genuine
// rate-limit (capacity); Anthropic's 529 overloaded_error → capacity.
//
// **Typical use cases**: when the HTTP status alone isn't granular enough
//   - Same 429: OpenAI distinguishes insufficient_quota (permanent) vs a
//     genuine rate-limit (capacity)
//   - 200 + error body: a few vendors stuff errors into a 200 response,
//     invisible from HTTP status alone
//   - 5xx refinement: Anthropic's 529 overloaded_error should be capacity,
//     not transient
//
// **Contract**:
//   - implementations MUST be safe for concurrent use (classified from
//     multiple goroutines simultaneously)
//   - body argument: implementations must not retain a reference to the
//     slice — if it needs to be stored in the returned AdapterError, it must
//     be string(body) / copied
//   - body may be partial (M7 limit-reads to 1KiB); implementations must be
//     tolerant of truncated JSON
type Classifier interface {
	Classify(status int, body []byte) *domain.AdapterError
}

// DefaultClassifier classifies purely by HTTP status. Used as a fallback
// when the Factory doesn't implement Classifier.
type DefaultClassifier struct{}

// Classify maps an HTTP status to an ErrorClass.
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

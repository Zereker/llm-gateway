package protocol

import (
	"context"
	"net/http"

	"github.com/zereker/llm-gateway/internal/domain"
)

// =============================================================================
// vendor Factory / Session / Metadata
// =============================================================================
//
// **Architecture relationship**:
//
//	Handler = Combine(Factory, translator.Translator)
//
// Factory owns the vendor HTTP layer (URL / auth headers / TLS / proxy); body
// shape translation runs through internal/translator; end-to-end protocol handling
// goes through the Handler facade.
//
// **Facade boundary** (important — the discipline after v0.7 merged the former pkg/adapter
// into internal/protocol):
//
//   Allowed to consume Factory / Session directly:
//     - inside this package (combine.go / registry.go)
//     - internal/protocol/<vendor>/ subpackages (define their Factory type)
//     - internal/builtin (composition root; assembles the factory map)
//
//   **Forbidden**: type-asserting or directly calling Factory in data-plane
//   packages like internal/dispatch / internal/middleware / internal/invoker / internal/selector /
//   internal/router. They interact with the protocol layer only through the two
//   facade types protocol.Handler / protocol.Lookup.
//
//   Counter-example: dispatch fetching a per-vendor Factory and type-asserting
//   to branch logic per vendor — this leaks vendor knowledge into the scheduling
//   layer and defeats the point of the facade.
//
// **Steps to add a new vendor**:
//  1. Write a struct implementing Factory + Session in internal/protocol/<vendor>/
//  2. Add it to the factory map in internal/builtin.NewLookup, keyed by vendor name
//  3. If there's no coverage between the client protocol and endpoint.Protocol:
//     add a Translator in internal/translator/<src>_<tgt>/ and list it in NewLookup
//
// Examples:
//   - DeepSeek / ARK: vendor=ark, endpoint.Protocol=OpenAI (identity translator)
//   - Vertex Gemini: vendor=gemini, endpoint.Protocol=Gemini (client OpenAI → openai_gemini)

// Metadata is static, vendor-level information (not tied to a specific request).
//
// Returned by Factory.Metadata(); available at startup, used for:
//   - cross-checking against the vendor set in ConfigStore (alerts on missing registration)
//   - protocol.Capabilities exposing SupportedModalities for eligibility filtering
//   - scheduling logs / metric labels
//
// **No NativeProtocol field**: protocol ownership is an endpoint-level attribute
// (domain.Endpoint.Protocol), not a vendor-level one — the same vendor can have
// multiple endpoints on different protocols.
type Metadata struct {
	Vendor              string            // vendor name (aligned with endpoints.vendor)
	SupportedModalities []domain.Modality // modalities it can handle
}

// Factory is a factory registered in the vendor registry.
//
// One factory per vendor; the factory itself is stateless, a single instance.
// Each request has a Session instance constructed by NewSession.
//
// Factory implementations MUST be safe for concurrent use (multiple gin handler
// goroutines call NewSession concurrently).
type Factory interface {
	Metadata() Metadata

	// NewSession creates a Session dedicated to this request.
	NewSession(c context.Context, ep *domain.Endpoint, env *domain.RequestEnvelope) (Session, error)
}

// Session is the **slim version**: only responsible for building the upstream
// HTTP request + releasing resources.
//
// No more Feed / Finalize / FinalizeResult — chunk streaming and usage
// extraction have all moved to internal/translator.ResponseHandler.
//
// **Contract**:
//   - single-goroutine use (same goroutine as the gin handler); implementations
//     need no extra locking
//   - BuildRequest is called once; body / extraHeaders are the final artifacts
//     produced by the caller (internal/protocol.combined) after running translator + quirks
//   - Close must be deferred on every path; idempotent
type Session interface {
	// BuildRequest builds the HTTP request to send upstream.
	//
	// **Params**:
	//   - body: the bytes after translator + quirks.RewriteBody have run (goes
	//     straight into req.Body)
	//   - extraHeaders: the final headers after quirks.RewriteHeader has run; nil
	//     means no extra headers. The adapter should copy extraHeaders into
	//     req.Header first, then write its own protocol-required Auth /
	//     Content-Type etc. (the adapter's protocol headers are written **last,
	//     overriding** quirks — to prevent a deployer from accidentally breaking
	//     Authorization)
	BuildRequest(body []byte, extraHeaders http.Header) (*http.Request, error)

	// Close releases resources held by the Session; must be deferred by dispatch; idempotent.
	Close() error
}

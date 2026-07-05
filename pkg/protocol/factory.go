package protocol

import (
	"context"
	"fmt"
	"net/http"

	"github.com/zereker/llm-gateway/pkg/domain"
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
// shape translation runs through pkg/translator; end-to-end protocol handling
// goes through the Handler facade.
//
// **Facade boundary** (important — the discipline after v0.7 merged pkg/adapter
// into pkg/protocol):
//
//   Allowed to consume Factory / Session / RegisterFactory / LookupFactory directly:
//     - inside this package (combine.go / registry.go)
//     - pkg/protocol/<vendor>/ subpackages (register themselves in init())
//     - cmd/gateway (composition root; currently unused, left as a hook for a
//       future CLI self-check)
//
//   **Forbidden**: type-asserting or directly calling Factory / LookupFactory in
//   data-plane packages like pkg/dispatch / pkg/middleware / pkg/invoker /
//   pkg/selector / pkg/router. They interact with the protocol layer only through
//   the two facade types protocol.Handler / protocol.Lookup.
//
//   Counter-example: dispatch calling LookupFactory(ep.Vendor) and type-asserting
//   to branch logic per vendor — this leaks vendor knowledge into the scheduling
//   layer and defeats the point of the facade.
//
// **Steps to add a new vendor**:
//  1. Write a struct implementing Factory + Session in pkg/protocol/<vendor>/
//  2. Call protocol.RegisterFactory("<vendor>", yourFactory) in init()
//  3. If there's no coverage between the client protocol and endpoint.Protocol:
//     add a Translator in pkg/translator/<src>_<tgt>/
//  4. Add a blank import in cmd/gateway to trigger init()
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
// extraction have all moved to pkg/translator.ResponseHandler.
//
// **Contract**:
//   - single-goroutine use (same goroutine as the gin handler); implementations
//     need no extra locking
//   - BuildRequest is called once; body / extraHeaders are the final artifacts
//     produced by the caller (pkg/protocol.combined) after running translator + quirks
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

// =============================================================================
// vendor Factory registry
// =============================================================================

var factoryRegistry = map[string]Factory{}

// RegisterFactory registers a vendor Factory; the vendor name is the registry key.
//
// **When vendor != Metadata().Vendor**: OpenAI-compatible aliases (ark / deepseek /
// qwen, etc.) reuse the same Factory but need to be registered under multiple
// names. So vendor is an explicit parameter, not derived from Metadata.
//
// Contract:
//   - **MUST** be called during init(); calling at runtime is unsafe (the
//     registry is unlocked, and LookupFactory does lock-free reads on the hot
//     request path, relying on memory visibility guaranteed by init() completion)
//   - registering the same name twice panics (failing fast at startup beats
//     silently overwriting)
func RegisterFactory(vendor string, f Factory) {
	if vendor == "" {
		panic("protocol: RegisterFactory vendor name empty")
	}
	if _, ok := factoryRegistry[vendor]; ok {
		panic(fmt.Sprintf("protocol: vendor %q already registered", vendor))
	}
	factoryRegistry[vendor] = f
}

// LookupFactory retrieves a Factory by vendor; returns nil if unregistered.
// Assumes all RegisterFactory calls have completed during init(); read-only at runtime.
func LookupFactory(vendor string) Factory {
	return factoryRegistry[vendor]
}

// ResetFactories clears the vendor registry — **for tests only**.
//
// Must not be called in production (factories are registered once during
// init(); after Reset, LookupFactory always returns nil → DefaultLookup always
// returns nil → every request gets a 503).
func ResetFactories() {
	factoryRegistry = map[string]Factory{}
}

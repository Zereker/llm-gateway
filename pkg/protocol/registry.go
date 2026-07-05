package protocol

import (
	"log/slog"
	"sync"

	"github.com/zereker/llm-gateway/pkg/domain"
	"github.com/zereker/llm-gateway/pkg/translator"
)

// Lookup is the request-level Handler lookup port — dynamically composes a
// Handler from (endpoint, sourceProtocol).
//
// **Design motivation**: protocol composition is a per-request concern, not
// something enumerable at init() time.
//   - the endpoint carries a Protocol field (configured by the deployer via SQL
//     INSERT) — indicating what protocol this endpoint's upstream speaks
//   - incoming clients use sourceProtocol (written by M3 Envelope into
//     rc.Envelope.SourceProtocol)
//   - DefaultLookup.Get(ep, src) composes on the fly from the (ep.Vendor, src,
//     ep.Protocol) triple:
//   - LookupFactory(ep.Vendor) → the vendor HTTP implementation
//   - translator.Find(src, ep.Protocol) → the protocol translator; when
//     src == ep.Protocol, returns the identity translator (already
//     registered in pkg/translator/identity)
//   - if it can't be obtained → return nil; the eligibility filter excludes the candidate accordingly
//
// **Override scenarios**: multi-tenancy / canary — middleware (e.g. M2 Auth)
// injects a custom Lookup implementation per tenant, which can use its own
// vendor set / custom translator chain.
type Lookup interface {
	// Get composes a Handler from the endpoint's vendor + protocol and the
	// client's srcProto. Returns nil if the adapter or a matching translator
	// can't be found.
	Get(ep *domain.Endpoint, srcProto domain.Protocol) Handler
}

// =============================================================================
// DefaultLookup — wraps the global vendor + translator registries
// =============================================================================

// handlerCache is a process-level Handler cache — key = (vendor, srcProto, ep.Protocol).
//
// **Why it's needed**: M3 Envelope news up a DefaultLookup{} per request;
// within dispatch, the eligibility + invoke paths each do a lookup. If this
// weren't shared at the package level, the combined Handler would be rebuilt
// on every lookup, and its internal quirks compile cache would keep being
// invalidated along with it — the deployer's quirks JSON would get recompiled
// on every single request.
//
// Handler itself is stateless (vendor + translator + an internal sync.Map
// cache), safe for concurrent use; requests sharing the same (vendor, srcProto,
// target) triple share the same Handler instance. The endpoint is passed in via
// the PrepareCall parameter, not bound to the Handler.
//
// **Eviction**: the upper bound on vendor × srcProto × upstreamProto
// combinations is small (<100), so no eviction is done; entries live for the
// lifetime of the process.
var handlerCache sync.Map // key = "vendor|src|target" → Handler

// DefaultLookup composes a Handler using the global vendor + translator
// registries. M3 Envelope fills this in when rc.Handlers is nil.
//
// **Stateless**: all caching lives in the package-level handlerCache. The zero
// value is usable; per-request creation is zero-cost.
type DefaultLookup struct{}

// pivotProtocol is the intermediate protocol used for the missing-direct-pair
// composition fallback (docs/02 §6a).
//
// OpenAI was chosen: it's the de facto industry lingua franca — every existing
// cross-protocol translator has it on one end (anthropic↔openai /
// openai→gemini / responses→openai), so when adding any new protocol, writing
// its conversion pair with OpenAI first automatically maximizes the fallback
// composition's coverage.
const pivotProtocol = domain.ProtoOpenAI

func (DefaultLookup) Get(ep *domain.Endpoint, srcProto domain.Protocol) Handler {
	if ep == nil || ep.Protocol == domain.ProtoUnknown {
		return nil
	}
	key := ep.Vendor + "|" + srcProto.String() + "|" + ep.Protocol.String()
	if h, ok := handlerCache.Load(key); ok {
		return h.(Handler)
	}
	ad := LookupFactory(ep.Vendor)
	if ad == nil {
		return nil
	}
	// Direct translators are preferred (high fidelity); on a miss, fall back to
	// pivot composition (potentially lossy — the double hop drops fields the
	// pivot can't express). Popular combinations should get a direct
	// implementation as soon as possible — once registered, FindVia
	// automatically prefers it and the composition steps aside, transparent to the caller.
	tr := translator.FindVia(srcProto, ep.Protocol, pivotProtocol)
	if tr == nil {
		return nil
	}
	if translator.IsComposed(tr) {
		// handlerCache ensures the same (vendor, src, tgt) only warns once
		slog.Warn("protocol: no direct translator, using lossy pivot composition",
			"src", srcProto.String(), "tgt", ep.Protocol.String(),
			"pivot", pivotProtocol.String(), "vendor", ep.Vendor)
	}
	h := Combine(ad, tr)
	actual, _ := handlerCache.LoadOrStore(key, h)
	return actual.(Handler)
}

// ResetHandlerCache clears DefaultLookup's Handler cache — **for tests only**.
// Must be called alongside ResetFactories / translator.Reset to avoid stale
// Handlers referencing a deleted Factory.
func ResetHandlerCache() {
	handlerCache = sync.Map{}
}

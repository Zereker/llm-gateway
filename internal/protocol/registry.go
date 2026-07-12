package protocol

import (
	"log/slog"
	"sync"

	"github.com/zereker/llm-gateway/internal/domain"
	"github.com/zereker/llm-gateway/internal/translator"
)

// Lookup is the request-level Handler lookup port — dynamically composes a
// Handler from (endpoint, sourceProtocol).
//
// **Design motivation**: protocol composition is a per-request concern, not
// something enumerable at startup.
//   - the endpoint carries a Protocol field (configured by the deployer via SQL
//     INSERT) — indicating what protocol this endpoint's upstream speaks
//   - incoming clients use sourceProtocol (written by M3 Envelope into
//     rc.Envelope.SourceProtocol)
//   - DefaultLookup.Get(ep, src) composes on the fly from the (ep.Vendor, src,
//     ep.Protocol) triple:
//   - factories[ep.Vendor] → the vendor HTTP implementation
//   - translators.FindVia(src, ep.Protocol, pivot) → the protocol translator;
//     when src == ep.Protocol, returns the identity translator (built into the
//     registry from internal/translator/identity)
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
// DefaultLookup — composes Handlers from an application's vendor + translator sets
// =============================================================================

// DefaultLookup composes a Handler from an application-scoped factory map and
// translator registry. Construct it with NewLookup; the zero value is not
// usable (the built-in catalog is assembled in internal/builtin.NewLookup, and
// tenant overrides build their own via NewLookup too).
//
// **Handler caching**: Get memoizes the composed Handler per (vendor, srcProto,
// ep.Protocol) triple in the lookup's own cache. Handler itself is stateless
// (vendor + translator + an internal quirks compile cache), safe for concurrent
// use; requests sharing the same triple share the same Handler instance, so the
// deployer's quirks JSON is compiled once rather than on every request. The
// endpoint is passed in via PrepareCall, not bound to the Handler.
//
// **Eviction**: the upper bound on vendor × srcProto × upstreamProto
// combinations is small (<100), so no eviction is done; entries live for the
// lifetime of the lookup.
type DefaultLookup struct {
	factories   map[string]Factory
	translators *translator.Registry
	cache       *sync.Map
}

// NewLookup constructs an isolated handler registry for one application.
// The supplied maps are copied so callers cannot mutate the capability set
// after startup.
func NewLookup(factories map[string]Factory, translators *translator.Registry) *DefaultLookup {
	copyFactories := make(map[string]Factory, len(factories))
	for vendor, factory := range factories {
		if vendor == "" || factory == nil {
			panic("protocol: invalid factory registration")
		}

		if _, exists := copyFactories[vendor]; exists {
			panic("protocol: duplicate factory " + vendor)
		}

		copyFactories[vendor] = factory
	}

	return &DefaultLookup{factories: copyFactories, translators: translators, cache: &sync.Map{}}
}

// pivotProtocol is the intermediate protocol used for the missing-direct-pair
// composition fallback (docs/02 §6a).
//
// OpenAI was chosen: it's the de facto industry lingua franca — every existing
// cross-protocol translator has it on one end (anthropic↔openai /
// openai→gemini / responses→openai), so when adding any new protocol, writing
// its conversion pair with OpenAI first automatically maximizes the fallback
// composition's coverage.
const pivotProtocol = domain.ProtoOpenAI

func (l DefaultLookup) Get(ep *domain.Endpoint, srcProto domain.Protocol) Handler {
	if ep == nil || ep.Protocol == domain.ProtoUnknown {
		return nil
	}

	key := ep.Vendor + "|" + srcProto.String() + "|" + ep.Protocol.String()
	if h, ok := l.cache.Load(key); ok {
		return h.(Handler)
	}

	ad := l.factories[ep.Vendor]
	if ad == nil {
		return nil
	}
	// Direct translators are preferred (high fidelity); on a miss, fall back to
	// pivot composition (potentially lossy — the double hop drops fields the
	// pivot can't express). Popular combinations should get a direct
	// implementation as soon as possible — once registered, FindVia
	// automatically prefers it and the composition steps aside, transparent to the caller.
	tr := l.translators.FindVia(srcProto, ep.Protocol, pivotProtocol)
	if tr == nil {
		return nil
	}

	if translator.IsComposed(tr) {
		// the cache ensures the same (vendor, src, tgt) only warns once
		slog.Warn("protocol: no direct translator, using lossy pivot composition",
			"src", srcProto.String(), "tgt", ep.Protocol.String(),
			"pivot", pivotProtocol.String(), "vendor", ep.Vendor)
	}

	h := Combine(ad, tr)
	actual, _ := l.cache.LoadOrStore(key, h)

	return actual.(Handler)
}

// HasVendor reports whether this lookup has a vendor factory.
func (l DefaultLookup) HasVendor(vendor string) bool {
	return l.factories[vendor] != nil
}

// CanTranslate reports whether a client protocol can reach target directly
// or through the standard OpenAI pivot.
func (l DefaultLookup) CanTranslate(source, target domain.Protocol) bool {
	return l.translators.FindVia(source, target, pivotProtocol) != nil
}

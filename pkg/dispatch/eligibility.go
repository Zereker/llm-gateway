package dispatch

import (
	"github.com/zereker/llm-gateway/pkg/domain"
	"github.com/zereker/llm-gateway/pkg/protocol"
)

// filterEligible is the Dispatcher's internal eligibility-filtering step —
// it strips out endpoints that "aren't capable of handling the current
// request", so the endpoint-selection step isn't polluted by invalid
// candidates.
//
// **Filtering rules**:
//
//  1. handlers.Get(ep, env.SourceProtocol) returns nil → no usable Handler
//     (missing vendor Factory, missing translator, or ep.Protocol is
//     unknown) → excluded
//  2. endpoint doesn't support env.Modality → excluded. The semantics are
//     **narrowing only, never widening**:
//       - endpoint non-empty + vendor non-empty → **both** must cover the
//         current modality (intersection)
//       - endpoint non-empty + vendor empty     → trust the endpoint
//         (compat for test stubs that don't populate metadata)
//       - endpoint empty     + vendor non-empty  → go by the vendor ceiling
//       - endpoint empty     + vendor empty      → no modality restriction
//     This also stops a deployer misconfiguration like ["tts"] on a
//     chat-only vendor from letting the request sneak into the selector.
//     Typical usage: the OpenAI vendor declares chat / embedding / image,
//     and a deployer who only bought chat quota for ep-A locks it down with
//     capabilities.modalities=["chat"].
//
// **Why it lives in dispatch**: living under pkg/selector/eligibility before
// v0.6 was historical baggage; the logic is tightly coupled to the dispatch
// flow (candidates → filter → select → invoke) and has nothing to do with
// selector algorithms (filter chain / scorer / picker), so it makes more
// sense as an internal dispatch helper.
//
// **Pure function**: stateless; the caller passes the resulting eligible
// list to Selector.Pick.
func filterEligible(candidates []*domain.Endpoint, env *domain.RequestEnvelope, handlers protocol.Lookup) []*domain.Endpoint {
	if env == nil {
		return candidates
	}
	if handlers == nil {
		return nil
	}
	out := make([]*domain.Endpoint, 0, len(candidates))
	for _, ep := range candidates {
		h := handlers.Get(ep, env.SourceProtocol)
		if h == nil {
			continue
		}
		if !endpointSupportsModality(ep, h, env.Modality) {
			continue
		}
		out = append(out, ep)
	}
	return out
}

// endpointSupportsModality decides whether an endpoint can handle the given
// modality.
//
// **Semantics**: endpoint modalities are a **narrowing subset** of the
// vendor ceiling — they can never widen it.
//
//	endpoint non-empty + vendor non-empty → both must support it
//	  (intersection). If a deployer configures ["tts"] but the vendor only
//	  declares chat, the request is excluded — this prevents a deployer
//	  misconfiguration from letting an unsupported modality sneak into the
//	  selector.
//	endpoint non-empty + vendor empty     → trust the endpoint (compat for
//	  fakeAdapter / test stub implementations that don't populate vendor
//	  metadata).
//	endpoint empty     + vendor non-empty → go by the vendor ceiling.
//	endpoint empty     + vendor empty     → no modality restriction.
func endpointSupportsModality(ep *domain.Endpoint, h protocol.Handler, want domain.Modality) bool {
	epMods := ep.Capabilities.Modalities
	vendorMods := h.Capabilities().SupportedModalities

	if len(epMods) == 0 && len(vendorMods) == 0 {
		return true
	}
	if len(epMods) > 0 && !containsModality(epMods, want) {
		return false
	}
	if len(vendorMods) > 0 && !containsModality(vendorMods, want) {
		return false
	}
	return true
}

func containsModality(set []domain.Modality, want domain.Modality) bool {
	for _, m := range set {
		if m == want {
			return true
		}
	}
	return false
}

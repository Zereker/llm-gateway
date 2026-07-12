// Package endpointcheck is the validity checker for endpoint business config — it is shared
// by **the control plane, before writes** and **the data plane, during startup scans**,
// so that "what counts as misconfigured" is defined in exactly one place.
//
// Validation reads a protocol capability Catalog (vendor registration + translator
// reachability), which the caller injects — both cmd/gateway and cmd/console build it from
// internal/builtin.NewLookup. Without a Catalog covering the endpoint's vendor / protocol,
// an otherwise valid endpoint is reported as vendor_not_registered / no_translator_path.
package endpointcheck

import (
	"net/netip"
	"net/url"
	"strings"

	"github.com/zereker/llm-gateway/internal/domain"
	"github.com/zereker/llm-gateway/internal/invoker"
	"github.com/zereker/llm-gateway/internal/protocol/quirks"
)

// clientProtocols are the client-facing entry protocols the gateway exposes (docs/02 §2);
// translator reachability is judged by "at least one client protocol can reach this endpoint".
var clientProtocols = []domain.Protocol{
	domain.ProtoOpenAI,
	domain.ProtoAnthropic,
	domain.ProtoResponses,
}

// Catalog is the protocol capability view needed for endpoint validation.
type Catalog interface {
	HasVendor(vendor string) bool
	CanTranslate(source, target domain.Protocol) bool
}

// Validator checks endpoint configuration against an explicit capability catalog.
type Validator struct{ Catalog Catalog }

// reasonMetadataEndpoint is the validation-failure reason shared by both
// detection paths in validateRoutingURL (well-known hostname vs. resolved
// metadata IP) — same classification either way.
const reasonMetadataEndpoint = "metadata_endpoint"

// Validate returns all misconfiguration reasons for one endpoint (empty slice = healthy).
//
// reason is a stable snake_case identifier, used both as a metric label and as the
// machine-readable error code in the control plane's 400 response.
func (v Validator) Validate(ep *domain.Endpoint) []string {
	var reasons []string

	// 1) protocol validity: ProtoUnknown means ParseProtocol didn't recognize it (typo / trailing space).
	if ep.Protocol == domain.ProtoUnknown {
		reasons = append(reasons, "unknown_protocol")
	}

	// 2) vendor Factory registration check.
	if v.Catalog == nil || !v.Catalog.HasVendor(ep.Vendor) {
		reasons = append(reasons, "vendor_not_registered")
	}

	// 3) translator reachability: reachable as long as any one client protocol can get there
	// (direct or via a pivot combination).
	if ep.Protocol != domain.ProtoUnknown {
		reachable := false
		for _, src := range clientProtocols {
			if v.Catalog != nil && v.Catalog.CanTranslate(src, ep.Protocol) {
				reachable = true
				break
			}
		}

		if !reachable {
			reasons = append(reasons, "no_translator_path")
		}
	}

	// 4) routing.url basic validation + metadata SSRF defense.
	if r := validateRoutingURL(ep.Routing.URL); r != "" {
		reasons = append(reasons, r)
	}

	// 5) quirks compilability: surface typo'd fields here, rather than erroring only at
	// request-time PhaseQuirks.
	if len(ep.Quirks) > 0 {
		if _, err := quirks.CompileJSON(ep.Quirks); err != nil {
			reasons = append(reasons, "invalid_quirks_spec")
		}
	}

	return reasons
}

// validateRoutingURL validates the upstream URL; returns a misconfiguration reason (empty = pass).
//
// **SSRF boundary** (intentionally narrow): only blocks the cloud metadata surface
// (169.254.0.0/16 / fe80::/10 / AWS IMDSv6 + well-known metadata hostnames) — that is never
// a legitimate upstream. **Does not block private-network IPs**: self-hosted vLLM / Ollama
// deployed on an internal network is a first-class scenario for this project (docs/00 §1).
// This is a **pre-check** at startup / write time; the real enforcement at runtime is in the
// invoker dial hook (checked against the resolved IP, to block DNS-rebinding).
func validateRoutingURL(raw string) string {
	if raw == "" {
		return "empty_routing_url"
	}

	u, err := url.Parse(raw)
	if err != nil {
		return "invalid_routing_url"
	}

	if u.Scheme != "http" && u.Scheme != "https" {
		return "invalid_routing_scheme"
	}

	host := strings.ToLower(u.Hostname())
	if host == "" {
		return "invalid_routing_url"
	}
	// Well-known metadata hostnames
	switch host {
	case "metadata.google.internal", "metadata", "instance-data":
		return reasonMetadataEndpoint
	}
	// Metadata IP (shares invoker.IsMetadataIP with the dial-time SSRF defense — single source of truth)
	if ip, err := netip.ParseAddr(host); err == nil && invoker.IsMetadataIP(ip) {
		return reasonMetadataEndpoint
	}

	return ""
}

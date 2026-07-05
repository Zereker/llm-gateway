// Package endpointcheck is the validity checker for endpoint business config — it is shared
// by **the control plane, before writes** and **the data plane, during startup scans**,
// so that "what counts as misconfigured" is defined in exactly one place.
//
// Validation depends on the vendor Factory registry (protocol.LookupFactory) and the
// translator registry (translator.FindVia) — the caller's main must already blank-import
// the corresponding vendor / translator subpackages to complete registration, otherwise a
// valid endpoint will be misjudged as vendor_not_registered / no_translator_path.
// Both cmd/gateway and cmd/console carry this set of blank imports.
package endpointcheck

import (
	"net/netip"
	"net/url"
	"strings"

	"github.com/zereker/llm-gateway/pkg/domain"
	"github.com/zereker/llm-gateway/pkg/invoker"
	"github.com/zereker/llm-gateway/pkg/protocol"
	"github.com/zereker/llm-gateway/pkg/protocol/quirks"
	"github.com/zereker/llm-gateway/pkg/translator"
)

// clientProtocols are the client-facing entry protocols the gateway exposes (docs/02 §2);
// translator reachability is judged by "at least one client protocol can reach this endpoint".
var clientProtocols = []domain.Protocol{
	domain.ProtoOpenAI,
	domain.ProtoAnthropic,
	domain.ProtoResponses,
}

// Validate returns all misconfiguration reasons for one endpoint (empty slice = healthy).
//
// reason is a stable snake_case identifier, used both as a metric label and as the
// machine-readable error code in the control plane's 400 response.
func Validate(ep *domain.Endpoint) []string {
	var reasons []string

	// 1) protocol validity: ProtoUnknown means ParseProtocol didn't recognize it (typo / trailing space).
	if ep.Protocol == domain.ProtoUnknown {
		reasons = append(reasons, "unknown_protocol")
	}

	// 2) vendor Factory registration check.
	if protocol.LookupFactory(ep.Vendor) == nil {
		reasons = append(reasons, "vendor_not_registered")
	}

	// 3) translator reachability: reachable as long as any one client protocol can get there
	// (direct or via a pivot combination).
	if ep.Protocol != domain.ProtoUnknown {
		reachable := false
		for _, src := range clientProtocols {
			if translator.FindVia(src, ep.Protocol, domain.ProtoOpenAI) != nil {
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
		return "metadata_endpoint"
	}
	// Metadata IP (shares invoker.IsMetadataIP with the dial-time SSRF defense — single source of truth)
	if ip, err := netip.ParseAddr(host); err == nil && invoker.IsMetadataIP(ip) {
		return "metadata_endpoint"
	}
	return ""
}

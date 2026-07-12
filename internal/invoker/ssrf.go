package invoker

import (
	"fmt"
	"net"
	"net/netip"
	"syscall"
)

// nonLinkLocalMetadataIPs are the known cloud metadata endpoints that do NOT
// fall in an IPv4/IPv6 link-local range, so IsLinkLocalUnicast can't catch
// them and they must be listed explicitly. Every *other* vendor's metadata
// service (AWS/GCP/Azure/Oracle-modern/DigitalOcean/Tencent/Huawei/IBM/
// OpenStack/k8s) is IPv4 169.254.169.254 (or another 169.254.0.0/16 address,
// e.g. Tencent's 169.254.0.23), already covered by the link-local check.
//
// Each is matched as an exact /32 (or single /128), not its enclosing range,
// so a self-hosted upstream that legitimately sits in shared address space
// (e.g. RFC 6598 CGNAT 100.64.0.0/10) is not false-positived — only the one
// metadata IP is refused.
//
// NOTE: this only covers metadata services addressed by a fixed IP. A vendor
// whose metadata is reached via a hostname that resolves to a *public* IP
// cannot be blocked at the IP layer; that boundary is documented for deployers.
var nonLinkLocalMetadataIPs = []netip.Addr{
	netip.MustParseAddr("fd00:ec2::254"),   // AWS IMDSv6 (ULA, fc00::/7)
	netip.MustParseAddr("100.100.100.200"), // Alibaba Cloud ECS (RFC 6598 CGNAT)
	netip.MustParseAddr("192.0.0.192"),     // Oracle Cloud classic (IETF-reserved 192.0.0.0/24)
}

// blockMetadataDial is the net.Dialer.Control hook: before the connection
// is actually established, it blocks cloud metadata endpoints based on the
// **already-resolved destination IP**.
//
// **Why in Control rather than validating the URL hostname**: Control
// receives the actual IP about to be dialed, after DNS resolution — this
// blocks DNS-rebinding. An attacker configures an endpoint as
// `http://evil.com/...`, and evil.com resolves to 169.254.169.254; a
// hostname-only check would let this through, while a dial-layer check
// blocks it precisely. Every candidate address passes through Control once,
// so multiple A records are also covered.
//
// **Only blocks metadata, not private networks**: self-hosted upstreams
// commonly live in 10/172.16/192.168 private ranges, which docs/07 §5 +
// MED#12 explicitly allow; this only blocks link-local (including all cloud
// IMDS v4 addresses at 169.254.169.254), IPv6 link-local, and the specific
// non-link-local metadata IPs in nonLinkLocalMetadataIPs (AWS IMDSv6 /
// Alibaba / Oracle-classic) — it never mistakenly blocks a self-hosted
// private-network endpoint.
func blockMetadataDial(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		host = address
	}

	// At the Control stage, address should already be IP:port; if it can't be
	// parsed, don't guess at a hostname — allow it through (ip.IsValid() is
	// false and IsMetadataIP never matches an invalid address).
	ip, _ := netip.ParseAddr(host)
	if IsMetadataIP(ip) {
		return fmt.Errorf("invoker: refusing to dial cloud metadata endpoint %s (SSRF guard)", ip)
	}

	return nil
}

// IsMetadataIP reports whether ip is a cloud metadata endpoint. The
// dial-time guard and the startup-time endpoint scan share this function,
// so "what counts as metadata" has exactly one definition.
//
//	169.254.0.0/16 (IPv4 link-local) ── AWS/GCP/Azure/Oracle-modern/DO/Tencent/... IMDS
//	fe80::/10      (IPv6 link-local)
//	plus the non-link-local exceptions in nonLinkLocalMetadataIPs
//	               (AWS IMDSv6 / Alibaba / Oracle-classic)
func IsMetadataIP(ip netip.Addr) bool {
	ip = ip.Unmap() // ::ffff:169.254.169.254 → 169.254.169.254
	if ip.IsLinkLocalUnicast() {
		return true
	}

	for _, m := range nonLinkLocalMetadataIPs {
		if ip == m {
			return true
		}
	}

	return false
}

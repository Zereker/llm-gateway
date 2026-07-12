package invoker

import (
	"fmt"
	"net"
	"net/netip"
	"syscall"
)

// awsIMDSv6 is AWS's Instance Metadata Service (IMDS) IPv6 address
// fd00:ec2::254. It belongs to ULA (fc00::/7), which is **not** link-local,
// so netip.Addr.IsLinkLocalUnicast can't catch it — hence the separate
// hardcoded entry. Every other cloud vendor's (GCP / Azure / Oracle / DO)
// IMDS is IPv4 169.254.169.254, which falls under link-local and is covered
// by IsLinkLocalUnicast.
var awsIMDSv6 = netip.MustParseAddr("fd00:ec2::254")

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
// IMDS v4 addresses at 169.254.169.254), IPv6 link-local, and AWS IMDSv6 —
// it never mistakenly blocks a self-hosted private-network endpoint.
func blockMetadataDial(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		host = address
	}

	ip, err := netip.ParseAddr(host)
	if err != nil {
		// At the Control stage, address should already be IP:port; if it
		// can't be parsed, don't guess at a hostname — allow it through.
		//nolint:nilerr // deliberate allow-through: an unparseable address is not a metadata-IP match (see comment above)
		return nil
	}

	if IsMetadataIP(ip) {
		return fmt.Errorf("invoker: refusing to dial cloud metadata endpoint %s (SSRF guard)", ip)
	}

	return nil
}

// IsMetadataIP reports whether ip is a cloud metadata endpoint. The
// dial-time guard and the startup-time endpoint scan share this function,
// so "what counts as metadata" has exactly one definition.
//
//	169.254.0.0/16 (IPv4 link-local) ── AWS/GCP/Azure/Oracle/DigitalOcean IMDS
//	fe80::/10      (IPv6 link-local)
//	fd00:ec2::254  (AWS IMDSv6, ULA, not caught by the link-local check)
func IsMetadataIP(ip netip.Addr) bool {
	ip = ip.Unmap() // ::ffff:169.254.169.254 → 169.254.169.254
	if ip.IsLinkLocalUnicast() {
		return true
	}

	return ip == awsIMDSv6
}

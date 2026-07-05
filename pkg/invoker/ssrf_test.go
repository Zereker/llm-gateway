package invoker

import (
	"net/netip"
	"testing"
)

func TestIsMetadataIP(t *testing.T) {
	cases := []struct {
		ip    string
		block bool
		note  string
	}{
		// metadata -- must be blocked
		{"169.254.169.254", true, "AWS/GCP/Azure/Oracle/DO IMDS v4"},
		{"169.254.170.2", true, "ECS task metadata (link-local)"},
		{"fe80::1", true, "IPv6 link-local"},
		{"fd00:ec2::254", true, "AWS IMDSv6 (ULA, not link-local)"},
		{"::ffff:169.254.169.254", true, "v4-mapped v6 still normalizes to metadata"},
		// self-hosted upstream / public internet -- must be allowed (docs/07 §5 permits private networks)
		{"10.0.0.5", false, "RFC1918 private network, self-hosted upstream"},
		{"172.16.3.4", false, "RFC1918 private network"},
		{"192.168.1.10", false, "RFC1918 private network"},
		{"100.100.100.200", false, "Alibaba Cloud metadata (CGNAT) -- by design, must not false-positive on self-hosted CGNAT"},
		{"127.0.0.1", false, "loopback (httptest, etc.)"},
		{"8.8.8.8", false, "public internet"},
		{"2001:4860:4860::8888", false, "public internet v6"},
	}
	for _, tc := range cases {
		ip, err := netip.ParseAddr(tc.ip)
		if err != nil {
			t.Fatalf("bad test ip %q: %v", tc.ip, err)
		}
		if got := IsMetadataIP(ip); got != tc.block {
			t.Errorf("IsMetadataIP(%s) = %v, want %v (%s)", tc.ip, got, tc.block, tc.note)
		}
	}
}

// blockMetadataDial takes the address shape (IP:port) the Control hook
// receives: metadata returns an error, everything else is allowed through.
func TestBlockMetadataDial(t *testing.T) {
	if err := blockMetadataDial("tcp", "169.254.169.254:80", nil); err == nil {
		t.Error("dialing 169.254.169.254 should be blocked by the SSRF guard")
	}
	if err := blockMetadataDial("tcp", "[fd00:ec2::254]:80", nil); err == nil {
		t.Error("dialing AWS IMDSv6 should be blocked")
	}
	if err := blockMetadataDial("tcp", "10.0.0.5:8080", nil); err != nil {
		t.Errorf("dialing a private-network self-hosted upstream should not be blocked: %v", err)
	}
	if err := blockMetadataDial("tcp", "93.184.216.34:443", nil); err != nil {
		t.Errorf("dialing the public internet should not be blocked: %v", err)
	}
}

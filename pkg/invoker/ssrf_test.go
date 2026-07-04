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
		// metadata —— 必须挡
		{"169.254.169.254", true, "AWS/GCP/Azure/Oracle/DO IMDS v4"},
		{"169.254.170.2", true, "ECS task metadata (link-local)"},
		{"fe80::1", true, "IPv6 link-local"},
		{"fd00:ec2::254", true, "AWS IMDSv6 (ULA, 非 link-local)"},
		{"::ffff:169.254.169.254", true, "v4-mapped v6 归一后仍是 metadata"},
		// 自建上游 / 公网 —— 必须放行（docs/07 §5 允许私网）
		{"10.0.0.5", false, "RFC1918 私网自建上游"},
		{"172.16.3.4", false, "RFC1918 私网"},
		{"192.168.1.10", false, "RFC1918 私网"},
		{"100.100.100.200", false, "阿里云 metadata（CGNAT）—— 按设计不误伤 CGNAT 自建"},
		{"127.0.0.1", false, "loopback（httptest 等）"},
		{"8.8.8.8", false, "公网"},
		{"2001:4860:4860::8888", false, "公网 v6"},
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

// blockMetadataDial 走 Control 钩子的 address 形态（IP:port）：metadata 返错，
// 其它放行。
func TestBlockMetadataDial(t *testing.T) {
	if err := blockMetadataDial("tcp", "169.254.169.254:80", nil); err == nil {
		t.Error("拨号 169.254.169.254 应被 SSRF 防线拦截")
	}
	if err := blockMetadataDial("tcp", "[fd00:ec2::254]:80", nil); err == nil {
		t.Error("拨号 AWS IMDSv6 应被拦截")
	}
	if err := blockMetadataDial("tcp", "10.0.0.5:8080", nil); err != nil {
		t.Errorf("拨号私网自建上游不应被拦截: %v", err)
	}
	if err := blockMetadataDial("tcp", "93.184.216.34:443", nil); err != nil {
		t.Errorf("拨号公网不应被拦截: %v", err)
	}
}

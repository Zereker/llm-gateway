package endpointcheck

import (
	"testing"

	"github.com/zereker/llm-gateway/pkg/domain"
)

func TestValidateRoutingURL(t *testing.T) {
	cases := []struct {
		url  string
		want string // 空 = 通过
	}{
		{"https://api.openai.com/v1/chat/completions", ""},
		{"http://10.0.3.7:8000/v1/chat/completions", ""}, // 私网 self-hosted vLLM：合法
		{"http://vllm.internal:8000/v1", ""},
		{"", "empty_routing_url"},
		{"ftp://x.com/path", "invalid_routing_scheme"},
		{"http://169.254.169.254/latest/meta-data/", "metadata_endpoint"}, // AWS metadata
		{"http://metadata.google.internal/computeMetadata/v1/", "metadata_endpoint"},
		{"http://METADATA.GOOGLE.INTERNAL/x", "metadata_endpoint"}, // 大小写
		{"http://[fe80::1]/x", "metadata_endpoint"},                // IPv6 链路本地
		{"http://[fd00:ec2::254]/latest/", "metadata_endpoint"},    // AWS IMDSv6
		{"http://instance-data/latest/", "metadata_endpoint"},
	}
	for _, tc := range cases {
		if got := validateRoutingURL(tc.url); got != tc.want {
			t.Errorf("validateRoutingURL(%q) = %q, want %q", tc.url, got, tc.want)
		}
	}
}

func TestValidate_UnknownProtocolAndBadQuirks(t *testing.T) {
	ep := &domain.Endpoint{
		ID: 1, Vendor: "no-such-vendor",
		Protocol: domain.ProtoUnknown, // 'openai ' 尾空格之类 parse 失败
		Routing:  domain.RoutingConfig{URL: "https://ok.example.com/v1"},
		Quirks:   []byte(`{"strips": ["x"]}`), // typo 字段
	}
	reasons := Validate(ep)

	want := map[string]bool{
		"unknown_protocol":      false,
		"vendor_not_registered": false,
		"invalid_quirks_spec":   false,
	}
	for _, r := range reasons {
		if _, ok := want[r]; ok {
			want[r] = true
		}
	}
	for r, hit := range want {
		if !hit {
			t.Errorf("应报 %q，实际 reasons=%v", r, reasons)
		}
	}
}

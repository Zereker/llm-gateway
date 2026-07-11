package endpointcheck

import (
	"testing"

	"github.com/zereker/llm-gateway/pkg/domain"
)

// emptyCatalog is a Catalog with no vendors and no translator paths, used to
// exercise the misconfiguration reasons that don't depend on a populated
// capability set.
type emptyCatalog struct{}

func (emptyCatalog) HasVendor(string) bool                  { return false }
func (emptyCatalog) CanTranslate(_, _ domain.Protocol) bool { return false }

func TestValidateRoutingURL(t *testing.T) {
	cases := []struct {
		url  string
		want string // empty = pass
	}{
		{"https://api.openai.com/v1/chat/completions", ""},
		{"http://10.0.3.7:8000/v1/chat/completions", ""}, // private-network self-hosted vLLM: legal
		{"http://vllm.internal:8000/v1", ""},
		{"", "empty_routing_url"},
		{"ftp://x.com/path", "invalid_routing_scheme"},
		{"http://169.254.169.254/latest/meta-data/", "metadata_endpoint"}, // AWS metadata
		{"http://metadata.google.internal/computeMetadata/v1/", "metadata_endpoint"},
		{"http://METADATA.GOOGLE.INTERNAL/x", "metadata_endpoint"}, // case sensitivity
		{"http://[fe80::1]/x", "metadata_endpoint"},                // IPv6 link-local
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
		Protocol: domain.ProtoUnknown, // parse failure, e.g. 'openai ' with a trailing space
		Routing:  domain.RoutingConfig{URL: "https://ok.example.com/v1"},
		Quirks:   []byte(`{"strips": ["x"]}`), // typo'd field
	}
	reasons := Validator{Catalog: emptyCatalog{}}.Validate(ep)

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
			t.Errorf("expected reason %q, got reasons=%v", r, reasons)
		}
	}
}

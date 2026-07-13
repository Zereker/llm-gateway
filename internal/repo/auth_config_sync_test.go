package repo

import (
	"testing"

	"github.com/zereker/llm-gateway/internal/domain"
)

// TestAuthTypeConstants_DomainRepoSync pins the auth-type wire values that
// are deliberately defined twice — once in internal/domain (consumed by
// adapters via domain.DecodePayload) and once here (consumed by the DB
// encrypt/decrypt layer). The duplication is a known cost of keeping domain
// free of repo imports; this test is the sync guard: if either side renames
// a value or adds one without mirroring it, the mismatch fails here instead
// of surfacing as a runtime "unknown auth type" on the request path.
func TestAuthTypeConstants_DomainRepoSync(t *testing.T) {
	pairs := []struct {
		name          string
		domainV, repV string
	}{
		{"bearer", domain.AuthTypeBearer, AuthTypeBearer},
		{"x-api-key", domain.AuthTypeXAPIKey, AuthTypeXAPIKey},
		{"gemini-key", domain.AuthTypeGeminiKey, AuthTypeGeminiKey},
		{"aws-sigv4", domain.AuthTypeAWSSigV4, AuthTypeAWSSigV4},
		{"oauth2-sa", domain.AuthTypeOAuth2SA, AuthTypeOAuth2SA},
		{"vertex-adc", domain.AuthTypeVertexADC, AuthTypeVertexADC},
	}

	for _, p := range pairs {
		if p.domainV != p.repV {
			t.Errorf("auth type %s: domain=%q repo=%q — the two definitions drifted", p.name, p.domainV, p.repV)
		}

		if p.domainV != p.name {
			t.Errorf("auth type %s: wire value changed to %q — update this test only if the rename is intentional AND both packages moved together", p.name, p.domainV)
		}
	}
}

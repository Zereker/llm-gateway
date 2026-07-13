package builtin

import (
	"testing"

	"github.com/zereker/llm-gateway/internal/domain"
)

// minimalEndpoint builds the smallest endpoint Lookup.Get needs: a vendor
// name plus the upstream protocol.
func minimalEndpoint(vendor string, proto domain.Protocol) *domain.Endpoint {
	return &domain.Endpoint{Vendor: vendor, Protocol: proto}
}

func TestNewLookup_ExtraOpenAIAliasRegisters(t *testing.T) {
	lookup := NewLookup("acme-llm")

	h := lookup.Get(minimalEndpoint("acme-llm", domain.ProtoOpenAI), domain.ProtoOpenAI)
	if h == nil {
		t.Fatal("config-registered OpenAI-compatible vendor should resolve a Handler")
	}

	// built-ins keep working alongside the extra alias
	if lookup.Get(minimalEndpoint("anthropic", domain.ProtoAnthropic), domain.ProtoOpenAI) == nil {
		t.Fatal("built-in vendor broken by extra alias registration")
	}
}

func TestNewLookup_ExtraAliasDuplicateOfBuiltinIsNoop(t *testing.T) {
	// "deepseek" is already a compiled-in alias of the same OpenAI Factory —
	// listing it again in config must not panic.
	lookup := NewLookup("deepseek")
	if lookup.Get(minimalEndpoint("deepseek", domain.ProtoOpenAI), domain.ProtoOpenAI) == nil {
		t.Fatal("duplicate-of-builtin alias should still resolve")
	}
}

func TestNewLookup_ExtraAliasCollidingWithRealVendorPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("aliasing a real non-OpenAI vendor (cohere) must panic at assembly time")
		}
	}()

	NewLookup("cohere")
}

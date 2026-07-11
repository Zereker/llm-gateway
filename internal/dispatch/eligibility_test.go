package dispatch

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/zereker/llm-gateway/internal/domain"
	"github.com/zereker/llm-gateway/internal/protocol"
)

// =============================================================================
// modality-aware Handler/Lookup fakes
// =============================================================================

type fakeModalityHandler struct {
	caps protocol.Capabilities
}

func (h fakeModalityHandler) Capabilities() protocol.Capabilities { return h.caps }
func (fakeModalityHandler) PrepareCall(context.Context, *domain.Endpoint, []byte) (*protocol.Call, error) {
	return nil, nil
}
func (fakeModalityHandler) NewResponseStream() protocol.ResponseStream { return nil }

// modalityLookup returns the same Handler for every endpoint; it simulates the
// "ceiling" modality set declared by vendor metadata.
type modalityLookup struct {
	vendorMods []domain.Modality
}

func (l modalityLookup) Get(_ *domain.Endpoint, _ domain.Protocol) protocol.Handler {
	return fakeModalityHandler{caps: protocol.Capabilities{SupportedModalities: l.vendorMods}}
}

// nilLookup makes any ep return a nil Handler — tests exclusion when the handler is missing.
type nilLookup struct{}

func (nilLookup) Get(*domain.Endpoint, domain.Protocol) protocol.Handler { return nil }

// =============================================================================
// tests
// =============================================================================

func TestFilterEligible_NoHandler_Excluded(t *testing.T) {
	candidates := []*domain.Endpoint{{ID: 1, Vendor: "v"}}
	env := &domain.RequestEnvelope{Modality: domain.ModalityChat}
	got := filterEligible(candidates, env, nilLookup{})
	if len(got) != 0 {
		t.Errorf("nil handler was not excluded: got %d", len(got))
	}
}

// vendor declares [chat, embedding], endpoint declares nothing → use the vendor ceiling
func TestFilterEligible_VendorFallback_When_EndpointEmpty(t *testing.T) {
	ep := &domain.Endpoint{ID: 1}
	candidates := []*domain.Endpoint{ep}
	lookup := modalityLookup{vendorMods: []domain.Modality{domain.ModalityChat, domain.ModalityEmbedding}}

	for _, m := range []domain.Modality{domain.ModalityChat, domain.ModalityEmbedding} {
		env := &domain.RequestEnvelope{Modality: m}
		got := filterEligible(candidates, env, lookup)
		if len(got) != 1 {
			t.Errorf("vendor supports %s but ep was excluded", m)
		}
	}

	envBad := &domain.RequestEnvelope{Modality: domain.ModalityImage}
	if got := filterEligible(candidates, envBad, lookup); len(got) != 0 {
		t.Errorf("ep should be excluded when vendor doesn't support image")
	}
}

// endpoint explicitly declares ["chat"] + vendor supports [chat, embedding, image]:
// only chat passes (the endpoint's narrowing subset takes effect)
func TestFilterEligible_EndpointNarrowsVendor(t *testing.T) {
	ep := &domain.Endpoint{
		ID: 1,
		Capabilities: domain.EndpointCapabilities{
			Modalities: []domain.Modality{domain.ModalityChat},
		},
	}
	candidates := []*domain.Endpoint{ep}
	lookup := modalityLookup{vendorMods: []domain.Modality{
		domain.ModalityChat, domain.ModalityEmbedding, domain.ModalityImage,
	}}

	// chat: both endpoint and vendor support it → passes
	if got := filterEligible(candidates, &domain.RequestEnvelope{Modality: domain.ModalityChat}, lookup); len(got) != 1 {
		t.Errorf("both endpoint and vendor support chat but it was excluded")
	}
	// embedding: vendor supports it but endpoint doesn't declare it → excluded
	if got := filterEligible(candidates, &domain.RequestEnvelope{Modality: domain.ModalityEmbedding}, lookup); len(got) != 0 {
		t.Errorf("endpoint didn't declare embedding but it passed anyway -- deployer should be able to narrow the vendor ceiling")
	}
}

// **Key safety check**: endpoint is configured with ["tts"] but vendor only
// declares chat → the tts request is excluded. This prevents a deployer
// misconfiguration from widening the vendor's actual capabilities and letting
// a request sneak past the selector only to crash later.
func TestFilterEligible_EndpointCannotWidenVendor(t *testing.T) {
	ep := &domain.Endpoint{
		ID: 1,
		Capabilities: domain.EndpointCapabilities{
			Modalities: []domain.Modality{domain.ModalityTTS}, // deployer misconfiguration
		},
	}
	candidates := []*domain.Endpoint{ep}
	// vendor actually only supports chat (e.g. the OpenAI Chat Completions adapter)
	lookup := modalityLookup{vendorMods: []domain.Modality{domain.ModalityChat}}

	// tts: endpoint declares it but vendor doesn't support it → **must be excluded** (prevent widening)
	if got := filterEligible(candidates, &domain.RequestEnvelope{Modality: domain.ModalityTTS}, lookup); len(got) != 0 {
		t.Errorf("tts request that widens vendor capability via endpoint should be excluded, got %d eps", len(got))
	}
	// chat: endpoint doesn't declare it → doesn't match the endpoint whitelist → excluded
	if got := filterEligible(candidates, &domain.RequestEnvelope{Modality: domain.ModalityChat}, lookup); len(got) != 0 {
		t.Errorf("chat request should be excluded when endpoint only declares tts (not chat)")
	}
}

// Two endpoints with the same vendor but different endpoint-level modalities:
// each only accepts its corresponding modality
func TestFilterEligible_MultipleEndpoints_DifferentScopes(t *testing.T) {
	epChat := &domain.Endpoint{
		ID:           1,
		Capabilities: domain.EndpointCapabilities{Modalities: []domain.Modality{domain.ModalityChat}},
	}
	epEmbed := &domain.Endpoint{
		ID:           2,
		Capabilities: domain.EndpointCapabilities{Modalities: []domain.Modality{domain.ModalityEmbedding}},
	}
	candidates := []*domain.Endpoint{epChat, epEmbed}
	lookup := modalityLookup{vendorMods: []domain.Modality{
		domain.ModalityChat, domain.ModalityEmbedding,
	}}

	chatOut := filterEligible(candidates, &domain.RequestEnvelope{Modality: domain.ModalityChat}, lookup)
	if len(chatOut) != 1 || chatOut[0].ID != 1 {
		t.Errorf("chat request should only leave epChat, got %d eps", len(chatOut))
	}

	embedOut := filterEligible(candidates, &domain.RequestEnvelope{Modality: domain.ModalityEmbedding}, lookup)
	if len(embedOut) != 1 || embedOut[0].ID != 2 {
		t.Errorf("embedding request should only leave epEmbed, got %d eps", len(embedOut))
	}
}

// Neither vendor nor endpoint declares anything → unrestricted modality (stubs
// like fakeAdapter go through this path; also covers the compatibility path
// for a plain SQL endpoint with no capabilities filled in)
func TestFilterEligible_BothEmpty_NoConstraint(t *testing.T) {
	candidates := []*domain.Endpoint{{ID: 1}}
	got := filterEligible(candidates, &domain.RequestEnvelope{Modality: domain.ModalityImage}, modalityLookup{})
	if len(got) != 1 {
		t.Errorf("modality should be unrestricted when neither vendor nor endpoint declares anything")
	}
}

// =============================================================================
// Modality JSON round-trip
// =============================================================================

func TestModality_JSON_StringForm(t *testing.T) {
	m := domain.ModalityEmbedding
	raw, err := m.MarshalJSON()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(raw) != `"embedding"` {
		t.Errorf("got %s, want \"embedding\"", raw)
	}
	var back domain.Modality
	if err := back.UnmarshalJSON(raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back != domain.ModalityEmbedding {
		t.Errorf("round-trip mismatch: %v", back)
	}
}

func TestModality_JSON_UnknownErrors(t *testing.T) {
	var m domain.Modality
	if err := m.UnmarshalJSON([]byte(`"chitchat"`)); err == nil {
		t.Error("unknown modality should return an error, so deployers catch it early")
	}
}

// Verifies that when capabilities JSON is persisted / read back, modality is a
// string array (not an enum number)
func TestEndpointCapabilities_JSON_ModalityAsString(t *testing.T) {
	caps := domain.EndpointCapabilities{
		Modalities: []domain.Modality{domain.ModalityChat, domain.ModalityImage},
	}
	body, err := json.Marshal(caps)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `{"modalities":["chat","image"]}`
	if string(body) != want {
		t.Errorf("got %s\nwant %s", body, want)
	}

	var back domain.EndpointCapabilities
	if err := json.Unmarshal([]byte(`{"modalities":["chat","embedding"]}`), &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(back.Modalities) != 2 || back.Modalities[0] != domain.ModalityChat || back.Modalities[1] != domain.ModalityEmbedding {
		t.Errorf("modality slice mismatch: %+v", back.Modalities)
	}
}

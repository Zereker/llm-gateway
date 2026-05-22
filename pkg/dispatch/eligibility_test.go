package dispatch

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/zereker/llm-gateway/pkg/domain"
	"github.com/zereker/llm-gateway/pkg/protocol"
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

// modalityLookup 给所有 endpoint 返同一个 Handler；模拟 vendor metadata 声明的
// "上限"模态集合。
type modalityLookup struct {
	vendorMods []domain.Modality
}

func (l modalityLookup) Get(_ *domain.Endpoint, _ domain.Protocol) protocol.Handler {
	return fakeModalityHandler{caps: protocol.Capabilities{SupportedModalities: l.vendorMods}}
}

// nilLookup 让任意 ep 都返 nil Handler——测 handler 缺失剔除。
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
		t.Errorf("nil handler 不剔除：got %d", len(got))
	}
}

// vendor 声明 [chat, embedding]、endpoint 不声明 → 用 vendor 上限
func TestFilterEligible_VendorFallback_When_EndpointEmpty(t *testing.T) {
	ep := &domain.Endpoint{ID: 1}
	candidates := []*domain.Endpoint{ep}
	lookup := modalityLookup{vendorMods: []domain.Modality{domain.ModalityChat, domain.ModalityEmbedding}}

	for _, m := range []domain.Modality{domain.ModalityChat, domain.ModalityEmbedding} {
		env := &domain.RequestEnvelope{Modality: m}
		got := filterEligible(candidates, env, lookup)
		if len(got) != 1 {
			t.Errorf("vendor 支持 %s 但 ep 被剔除", m)
		}
	}

	envBad := &domain.RequestEnvelope{Modality: domain.ModalityImage}
	if got := filterEligible(candidates, envBad, lookup); len(got) != 0 {
		t.Errorf("vendor 不支持 image 时 ep 应该被剔除")
	}
}

// endpoint 显式声明 ["chat"] → vendor 即使支持 embedding 也剔除 embedding 请求
func TestFilterEligible_EndpointModalitiesAuthoritative(t *testing.T) {
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

	// chat：endpoint 声明支持 → 通过
	if got := filterEligible(candidates, &domain.RequestEnvelope{Modality: domain.ModalityChat}, lookup); len(got) != 1 {
		t.Errorf("endpoint 声明 chat 但被剔除")
	}
	// embedding：vendor 支持但 endpoint 没声明 → 剔除（endpoint authoritative）
	if got := filterEligible(candidates, &domain.RequestEnvelope{Modality: domain.ModalityEmbedding}, lookup); len(got) != 0 {
		t.Errorf("endpoint 没声明 embedding 但被通过——vendor metadata 不该 fallback")
	}
}

// 两个 endpoint 同 vendor 但不同 endpoint-level modalities：分别只接对应模态
func TestFilterEligible_MultipleEndpoints_DifferentScopes(t *testing.T) {
	epChat := &domain.Endpoint{
		ID: 1,
		Capabilities: domain.EndpointCapabilities{Modalities: []domain.Modality{domain.ModalityChat}},
	}
	epEmbed := &domain.Endpoint{
		ID: 2,
		Capabilities: domain.EndpointCapabilities{Modalities: []domain.Modality{domain.ModalityEmbedding}},
	}
	candidates := []*domain.Endpoint{epChat, epEmbed}
	lookup := modalityLookup{vendorMods: []domain.Modality{
		domain.ModalityChat, domain.ModalityEmbedding,
	}}

	chatOut := filterEligible(candidates, &domain.RequestEnvelope{Modality: domain.ModalityChat}, lookup)
	if len(chatOut) != 1 || chatOut[0].ID != 1 {
		t.Errorf("chat 请求应该只剩 epChat，got %d eps", len(chatOut))
	}

	embedOut := filterEligible(candidates, &domain.RequestEnvelope{Modality: domain.ModalityEmbedding}, lookup)
	if len(embedOut) != 1 || embedOut[0].ID != 2 {
		t.Errorf("embedding 请求应该只剩 epEmbed，got %d eps", len(embedOut))
	}
}

// vendor 和 endpoint 都没声明 → 不限模态（fakeAdapter 之类 stub 走这条；
// 也覆盖纯 SQL endpoint 没填 capabilities 的兼容路径）
func TestFilterEligible_BothEmpty_NoConstraint(t *testing.T) {
	candidates := []*domain.Endpoint{{ID: 1}}
	got := filterEligible(candidates, &domain.RequestEnvelope{Modality: domain.ModalityImage}, modalityLookup{})
	if len(got) != 1 {
		t.Errorf("vendor + endpoint 都没声明应不限模态")
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
		t.Error("unknown modality 应当报错，方便 deployer 早暴露")
	}
}

// 验证 capabilities JSON 落库 / 取出来 modality 是字符串数组（不是 enum 数字）
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
		t.Errorf("modality slice 不对: %+v", back.Modalities)
	}
}


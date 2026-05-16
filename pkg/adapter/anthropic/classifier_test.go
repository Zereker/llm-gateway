package anthropic

import (
	"testing"

	"github.com/zereker/llm-gateway/pkg/domain"
)

func TestClassify_NoBody_FallbackToDefault(t *testing.T) {
	got := Factory{}.Classify(500, nil)
	if got == nil {
		t.Fatal("nil result")
	}
	// 500 fallback → ErrTransient
	if got.Class != domain.ErrTransient {
		t.Errorf("class=%v, want=transient", got.Class)
	}
}

func TestClassify_OverloadedError_MapsToRateLimit(t *testing.T) {
	body := []byte(`{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`)
	got := Factory{}.Classify(500, body)
	if got.Class != domain.ErrRateLimit {
		t.Errorf("class=%v, want=rate_limit (overloaded should be capacity)", got.Class)
	}
	if got.UpstreamMessage != "Overloaded" {
		t.Errorf("upstream msg=%q", got.UpstreamMessage)
	}
}

func TestClassify_InvalidRequestError_Overrides5xx(t *testing.T) {
	body := []byte(`{"type":"error","error":{"type":"invalid_request_error","message":"bad input"}}`)
	got := Factory{}.Classify(500, body) // 即使 5xx，invalid_request_error 应映射 ErrInvalid
	if got.Class != domain.ErrInvalid {
		t.Errorf("class=%v, want=invalid", got.Class)
	}
}

func TestClassify_AuthError_Permanent(t *testing.T) {
	body := []byte(`{"type":"error","error":{"type":"authentication_error","message":"unauth"}}`)
	got := Factory{}.Classify(401, body)
	if got.Class != domain.ErrPermanent {
		t.Errorf("class=%v, want=permanent", got.Class)
	}
}

func TestClassify_PermissionError_Permanent(t *testing.T) {
	body := []byte(`{"type":"error","error":{"type":"permission_error"}}`)
	got := Factory{}.Classify(403, body)
	if got.Class != domain.ErrPermanent {
		t.Errorf("class=%v, want=permanent", got.Class)
	}
}

func TestClassify_BadJSON_FallbackToDefault(t *testing.T) {
	got := Factory{}.Classify(500, []byte(`not json`))
	if got.Class != domain.ErrTransient {
		t.Errorf("malformed body should fallback, got=%v", got.Class)
	}
}

func TestClassify_ErrorFieldMissing_FallbackToDefault(t *testing.T) {
	// JSON 解析成功但没有 .error 字段 → fallback
	got := Factory{}.Classify(500, []byte(`{"type":"error"}`))
	if got.Class != domain.ErrTransient {
		t.Errorf("missing error obj should fallback, got=%v", got.Class)
	}
}

func TestClassify_UnknownErrorType_KeepsBaseClass(t *testing.T) {
	body := []byte(`{"type":"error","error":{"type":"weird_error","message":"x"}}`)
	got := Factory{}.Classify(500, body)
	// unknown error.type → 保留 base class（500 → transient）
	if got.Class != domain.ErrTransient {
		t.Errorf("class=%v, want=transient (unknown error.type → base)", got.Class)
	}
	if got.UpstreamMessage != "x" {
		t.Errorf("upstream msg should still be extracted, got=%q", got.UpstreamMessage)
	}
}

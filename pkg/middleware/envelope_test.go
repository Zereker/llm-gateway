package middleware

import (
	"encoding/json"
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/zereker/llm-gateway/pkg/adapter"
	"github.com/zereker/llm-gateway/pkg/dispatch"
	"github.com/zereker/llm-gateway/pkg/domain"
	"github.com/zereker/llm-gateway/pkg/translator"
)

// =============================================================================
// WithSourceProtocol
// =============================================================================

func TestWithSourceProtocol_CreatesEnvelopeShell(t *testing.T) {
	r := newGinTest(TraceContext(), Recover(), WithSourceProtocol(domain.ProtoAnthropic, domain.ModalityChat))
	var gotProto domain.Protocol
	var gotModality domain.Modality
	r.POST("/x", func(c *gin.Context) {
		rc := GetRequestContext(c)
		gotProto = rc.Envelope.SourceProtocol
		gotModality = rc.Envelope.Modality
		c.Status(200)
	})
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("POST", "/x", nil))

	if w.Code != 200 {
		t.Fatalf("status=%d", w.Code)
	}
	if gotProto != domain.ProtoAnthropic {
		t.Errorf("proto=%v, want=anthropic", gotProto)
	}
	if gotModality != domain.ModalityChat {
		t.Errorf("modality=%v, want=chat", gotModality)
	}
}

// =============================================================================
// Envelope
// =============================================================================

func TestEnvelope_HappyPath_ParsesModel(t *testing.T) {
	r := newGinTest(
		TraceContext(), Recover(),
		WithSourceProtocol(domain.ProtoOpenAI, domain.ModalityChat),
		Envelope(),
	)
	var gotModel string
	var gotRaw []byte
	r.POST("/x", func(c *gin.Context) {
		rc := GetRequestContext(c)
		gotModel = rc.Envelope.Model
		gotRaw = rc.Envelope.RawBytes
		c.Status(200)
	})

	body := `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("POST", "/x", strings.NewReader(body)))

	if w.Code != 200 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if gotModel != "gpt-4o" {
		t.Errorf("model=%q", gotModel)
	}
	if string(gotRaw) != body {
		t.Errorf("raw=%q", string(gotRaw))
	}
}

// TestEnvelope_PopulatesDefaultLookups 证明 M3 给 rc.Adapters / rc.Translators 写
// 默认值（DefaultAdapters / DefaultTranslators 包装全局 registry），让后续
// middleware / dispatch / invoker 能通过 dispatch.AdaptersFrom(rc) / TranslatorsFrom(rc)
// 拿到 nil-safe 的请求级查询端口。
func TestEnvelope_PopulatesDefaultLookups(t *testing.T) {
	r := newGinTest(
		TraceContext(), Recover(),
		WithSourceProtocol(domain.ProtoOpenAI, domain.ModalityChat),
		Envelope(),
	)
	var gotAdapters dispatch.AdapterLookup
	var gotTranslators dispatch.TranslatorLookup
	r.POST("/x", func(c *gin.Context) {
		rc := GetRequestContext(c)
		gotAdapters = dispatch.AdaptersFrom(rc)
		gotTranslators = dispatch.TranslatorsFrom(rc)
		c.Status(200)
	})

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("POST", "/x", strings.NewReader(`{"model":"x"}`)))

	if w.Code != 200 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if _, ok := gotAdapters.(dispatch.DefaultAdapters); !ok {
		t.Errorf("rc.Adapters not defaulted to DefaultAdapters; got %T", gotAdapters)
	}
	if _, ok := gotTranslators.(dispatch.DefaultTranslators); !ok {
		t.Errorf("rc.Translators not defaulted to DefaultTranslators; got %T", gotTranslators)
	}
}

// TestEnvelope_PreservesPreSetLookups 证明 M3 不覆盖前置 middleware 已写入的
// 自定义 lookup（多租户 / 灰度场景：M2 Auth 根据 tenant 装上 custom lookup，
// M3 不应该把它覆盖回 default）。
func TestEnvelope_PreservesPreSetLookups(t *testing.T) {
	custom := &fakeLookups{}
	// 一个挂在 Envelope 之前的 fake middleware 模拟"M2 Auth 按 tenant 注入"。
	preSet := func(c *gin.Context) {
		rc := GetRequestContext(c)
		rc.Adapters = custom
		rc.Translators = custom
		c.Next()
	}
	r := newGinTest(
		TraceContext(), Recover(),
		WithSourceProtocol(domain.ProtoOpenAI, domain.ModalityChat),
		preSet,
		Envelope(),
	)
	var gotAdapters dispatch.AdapterLookup
	var gotTranslators dispatch.TranslatorLookup
	r.POST("/x", func(c *gin.Context) {
		rc := GetRequestContext(c)
		gotAdapters = dispatch.AdaptersFrom(rc)
		gotTranslators = dispatch.TranslatorsFrom(rc)
		c.Status(200)
	})

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("POST", "/x", strings.NewReader(`{"model":"x"}`)))

	if w.Code != 200 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if gotAdapters != custom {
		t.Errorf("rc.Adapters overwritten by Envelope; want custom got %v", gotAdapters)
	}
	if gotTranslators != custom {
		t.Errorf("rc.Translators overwritten by Envelope; want custom got %v", gotTranslators)
	}
}

// fakeLookups 同时实现 AdapterLookup + TranslatorLookup（测试用占位，所有方法
// 返 nil；测试只校验"是不是同一指针"）。
type fakeLookups struct{}

func (*fakeLookups) Get(string) adapter.Factory                              { return nil }
func (*fakeLookups) Find(_, _ domain.Protocol) translator.Translator         { return nil }

func TestEnvelope_500_WithSourceProtocolMissing(t *testing.T) {
	r := newGinTest(TraceContext(), Recover(), Envelope())
	r.POST("/x", func(c *gin.Context) { c.Status(200) })

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("POST", "/x", strings.NewReader(`{"model":"x"}`)))
	if w.Code != 500 {
		t.Fatalf("status=%d, want=500", w.Code)
	}
	if !strings.Contains(w.Body.String(), "WithSourceProtocol middleware missing") {
		t.Errorf("body=%s", w.Body.String())
	}
}

func TestEnvelope_400_EmptyBody(t *testing.T) {
	r := newGinTest(
		TraceContext(), Recover(),
		WithSourceProtocol(domain.ProtoOpenAI, domain.ModalityChat),
		Envelope(),
	)
	r.POST("/x", func(c *gin.Context) { c.Status(200) })

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("POST", "/x", nil))
	if w.Code != 400 {
		t.Fatalf("status=%d, want=400", w.Code)
	}
	if !strings.Contains(w.Body.String(), "empty body") {
		t.Errorf("body=%s", w.Body.String())
	}
}

func TestEnvelope_400_MissingModelField(t *testing.T) {
	r := newGinTest(
		TraceContext(), Recover(),
		WithSourceProtocol(domain.ProtoOpenAI, domain.ModalityChat),
		Envelope(),
	)
	r.POST("/x", func(c *gin.Context) { c.Status(200) })

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("POST", "/x", strings.NewReader(`{"messages":[]}`)))
	if w.Code != 400 {
		t.Fatalf("status=%d, want=400", w.Code)
	}
	if !strings.Contains(w.Body.String(), "missing 'model'") {
		t.Errorf("body=%s", w.Body.String())
	}
}

func TestEnvelope_400_EmptyModelString(t *testing.T) {
	r := newGinTest(
		TraceContext(), Recover(),
		WithSourceProtocol(domain.ProtoOpenAI, domain.ModalityChat),
		Envelope(),
	)
	r.POST("/x", func(c *gin.Context) { c.Status(200) })

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("POST", "/x", strings.NewReader(`{"model":""}`)))
	if w.Code != 400 {
		t.Fatalf("status=%d, want=400", w.Code)
	}
	if !strings.Contains(w.Body.String(), "'model' field is empty") {
		t.Errorf("body=%s", w.Body.String())
	}
}

func TestEnvelope_400_ReadBodyError(t *testing.T) {
	r := newGinTest(
		TraceContext(), Recover(),
		WithSourceProtocol(domain.ProtoOpenAI, domain.ModalityChat),
		Envelope(),
	)
	r.POST("/x", func(c *gin.Context) { c.Status(200) })

	// failingReader 模拟 body 读到一半 IO 失败
	req := httptest.NewRequest("POST", "/x", &failingReader{})
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != 400 {
		t.Fatalf("status=%d, want=400", w.Code)
	}
	var body map[string]map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if !strings.Contains(body["error"]["message"], "read body") {
		t.Errorf("error.message=%q", body["error"]["message"])
	}
}

// failingReader 是 io.Reader 实现，永远返回 error。
type failingReader struct{}

func (failingReader) Read(_ []byte) (int, error) { return 0, errors.New("simulated io failure") }
func (failingReader) Close() error                { return nil }

func TestEnvelope_ResponseStartedAlready_StatusCode(t *testing.T) {
	// abort 通过 rc.Error + M9 Recover 写出；status 由 DefaultHTTPStatus 推导
	r := newGinTest(
		TraceContext(), Recover(),
		WithSourceProtocol(domain.ProtoOpenAI, domain.ModalityChat),
		Envelope(),
	)
	r.POST("/x", func(c *gin.Context) { c.Status(200) })
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("POST", "/x", strings.NewReader(`{}`)))

	var body map[string]map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if body["error"]["class"] != domain.ErrInvalid.String() {
		t.Errorf("error.class=%q, want=invalid", body["error"]["class"])
	}
}

// 防止 unused import 警告
var _ = io.EOF

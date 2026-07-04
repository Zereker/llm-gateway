package translator

import (
	"strings"
	"testing"

	"github.com/zereker/llm-gateway/pkg/domain"
)

// fakePairTranslator 可配置 src/tgt + 请求/响应转换标记的 test double。
// TranslateRequest 在 body 后追加 " |req:src→tgt"；handler Flush 输出
// "resp:src→tgt(" + 累积输入 + ")"，usage 可配。
type fakePairTranslator struct {
	src, tgt domain.Protocol
	usage    *domain.Usage
}

func (f fakePairTranslator) Source() domain.Protocol { return f.src }
func (f fakePairTranslator) Target() domain.Protocol { return f.tgt }

func (f fakePairTranslator) TranslateRequest(body []byte) ([]byte, error) {
	tag := " |req:" + f.src.String() + "→" + f.tgt.String()
	return append(append([]byte{}, body...), []byte(tag)...), nil
}

func (f fakePairTranslator) NewResponseHandler() ResponseHandler {
	return &fakePairHandler{src: f.src, tgt: f.tgt, usage: f.usage}
}

type fakePairHandler struct {
	src, tgt domain.Protocol
	usage    *domain.Usage
	buf      []byte
}

// Feed buffer 模式（跟真实跨协议 handler 一致）：全累积，返 nil。
func (h *fakePairHandler) Feed(chunk []byte) ([]byte, error) {
	h.buf = append(h.buf, chunk...)
	return nil, nil
}

func (h *fakePairHandler) Flush() ([]byte, *domain.Usage, error) {
	out := "resp:" + h.src.String() + "→" + h.tgt.String() + "(" + string(h.buf) + ")"
	return []byte(out), h.usage, nil
}

// =============================================================================
// Compose
// =============================================================================

func TestCompose_SourceTargetSpanEnds(t *testing.T) {
	front := fakePairTranslator{src: domain.ProtoAnthropic, tgt: domain.ProtoOpenAI}
	back := fakePairTranslator{src: domain.ProtoOpenAI, tgt: domain.ProtoGemini}
	c := Compose(front, back)

	if c.Source() != domain.ProtoAnthropic || c.Target() != domain.ProtoGemini {
		t.Errorf("composed span = %s→%s, want anthropic→gemini", c.Source(), c.Target())
	}
	if !IsComposed(c) {
		t.Error("IsComposed 应为 true")
	}
	if IsComposed(front) {
		t.Error("直连 translator 不该被判为 composed")
	}
}

func TestCompose_RequestChainsFrontThenBack(t *testing.T) {
	c := Compose(
		fakePairTranslator{src: domain.ProtoAnthropic, tgt: domain.ProtoOpenAI},
		fakePairTranslator{src: domain.ProtoOpenAI, tgt: domain.ProtoGemini},
	)
	out, err := c.TranslateRequest([]byte("BODY"))
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	// front 先跑（src→pivot），back 后跑（pivot→tgt）
	want := "BODY |req:anthropic→openai |req:openai→gemini"
	if string(out) != want {
		t.Errorf("got %q\nwant %q", out, want)
	}
}

func TestCompose_ResponseChainsUpstreamThenClient(t *testing.T) {
	c := Compose(
		fakePairTranslator{src: domain.ProtoAnthropic, tgt: domain.ProtoOpenAI},
		fakePairTranslator{src: domain.ProtoOpenAI, tgt: domain.ProtoGemini},
	)
	h := c.NewResponseHandler()

	// 上游（gemini 格式）chunk 进来
	if out, err := h.Feed([]byte("GEMINI-RESP")); err != nil || out != nil {
		t.Fatalf("buffer 模式 Feed 应返 nil：out=%q err=%v", out, err)
	}
	out, _, err := h.Flush()
	if err != nil {
		t.Fatalf("flush: %v", err)
	}
	// 响应向：back 的 handler 先翻（gemini→openai），front 的 handler 再翻（openai→anthropic）
	// fakePairHandler 的输出格式是 resp:src→tgt(...)——注意 handler 语义是"翻译
	// 自己 tgt 协议的响应回 src 协议"，串起来应该是外层 anthropic←openai 包住内层。
	want := "resp:anthropic→openai(resp:openai→gemini(GEMINI-RESP))"
	if string(out) != want {
		t.Errorf("got %q\nwant %q", out, want)
	}
}

func TestCompose_UsagePrefersUpstreamSide(t *testing.T) {
	upUsage := &domain.Usage{Total: 111}
	clientUsage := &domain.Usage{Total: 999}

	// back（离上游最近）有 usage → 用它
	c := Compose(
		fakePairTranslator{src: domain.ProtoAnthropic, tgt: domain.ProtoOpenAI, usage: clientUsage},
		fakePairTranslator{src: domain.ProtoOpenAI, tgt: domain.ProtoGemini, usage: upUsage},
	)
	h := c.NewResponseHandler()
	_, _ = h.Feed([]byte("x"))
	_, usage, _ := h.Flush()
	if usage == nil || usage.Total != 111 {
		t.Errorf("usage 应优先取上游侧（back handler），got %+v", usage)
	}

	// back 没有 usage → fallback 到 front
	c2 := Compose(
		fakePairTranslator{src: domain.ProtoAnthropic, tgt: domain.ProtoOpenAI, usage: clientUsage},
		fakePairTranslator{src: domain.ProtoOpenAI, tgt: domain.ProtoGemini},
	)
	h2 := c2.NewResponseHandler()
	_, _ = h2.Feed([]byte("x"))
	_, usage2, _ := h2.Flush()
	if usage2 == nil || usage2.Total != 999 {
		t.Errorf("上游侧无 usage 时应 fallback 到 client 侧，got %+v", usage2)
	}
}

func TestCompose_PivotMismatchPanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("pivot 错配应 panic")
		} else if !strings.Contains(r.(string), "pivot mismatch") {
			t.Errorf("panic 信息不对：%v", r)
		}
	}()
	Compose(
		fakePairTranslator{src: domain.ProtoAnthropic, tgt: domain.ProtoOpenAI},
		fakePairTranslator{src: domain.ProtoGemini, tgt: domain.ProtoGemini}, // front.tgt != back.src
	)
}

// =============================================================================
// FindVia
// =============================================================================

func TestFindVia_DirectWinsOverComposition(t *testing.T) {
	Reset()
	t.Cleanup(Reset)
	direct := fakePairTranslator{src: domain.ProtoAnthropic, tgt: domain.ProtoGemini}
	Register(direct)
	Register(fakePairTranslator{src: domain.ProtoAnthropic, tgt: domain.ProtoOpenAI})
	Register(fakePairTranslator{src: domain.ProtoOpenAI, tgt: domain.ProtoGemini})

	got := FindVia(domain.ProtoAnthropic, domain.ProtoGemini, domain.ProtoOpenAI)
	if got == nil {
		t.Fatal("FindVia 返 nil")
	}
	if IsComposed(got) {
		t.Error("有直连对时不该用组合")
	}
}

func TestFindVia_ComposesWhenDirectMissing(t *testing.T) {
	Reset()
	t.Cleanup(Reset)
	Register(fakePairTranslator{src: domain.ProtoAnthropic, tgt: domain.ProtoOpenAI})
	Register(fakePairTranslator{src: domain.ProtoOpenAI, tgt: domain.ProtoGemini})

	got := FindVia(domain.ProtoAnthropic, domain.ProtoGemini, domain.ProtoOpenAI)
	if got == nil {
		t.Fatal("两腿俱在时 FindVia 应组合出 translator")
	}
	if !IsComposed(got) {
		t.Error("应是组合产物")
	}
	if got.Source() != domain.ProtoAnthropic || got.Target() != domain.ProtoGemini {
		t.Errorf("span = %s→%s", got.Source(), got.Target())
	}
}

func TestFindVia_MissingLegReturnsNil(t *testing.T) {
	Reset()
	t.Cleanup(Reset)
	Register(fakePairTranslator{src: domain.ProtoAnthropic, tgt: domain.ProtoOpenAI})
	// 缺 openai→gemini 腿

	if got := FindVia(domain.ProtoAnthropic, domain.ProtoGemini, domain.ProtoOpenAI); got != nil {
		t.Errorf("缺腿应返 nil，got %T", got)
	}
}

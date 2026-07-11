package translator

import (
	"strings"
	"testing"

	"github.com/zereker/llm-gateway/pkg/domain"
)

// fakePairTranslator is a test double with configurable src/tgt plus
// request/response transformation markers.
// TranslateRequest appends " |req:src→tgt" to the body; handler Flush outputs
// "resp:src→tgt(" + accumulated input + ")"; usage is configurable.
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

// Feed uses buffer mode (consistent with real cross-protocol handlers): accumulate everything, return nil.
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
		t.Error("IsComposed should be true")
	}
	if IsComposed(front) {
		t.Error("a direct translator should not be judged as composed")
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
	// front runs first (src→pivot), back runs second (pivot→tgt)
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

	// upstream (gemini format) chunk comes in
	if out, err := h.Feed([]byte("GEMINI-RESP")); err != nil || out != nil {
		t.Fatalf("buffer-mode Feed should return nil: out=%q err=%v", out, err)
	}
	out, _, err := h.Flush()
	if err != nil {
		t.Fatalf("flush: %v", err)
	}
	// Response direction: back's handler translates first (gemini→openai), front's handler translates second (openai→anthropic)
	// fakePairHandler's output format is resp:src→tgt(...) — note the handler semantics are "translate
	// a response in its own tgt protocol back to src protocol", so chained together the outer anthropic←openai should wrap the inner one.
	want := "resp:anthropic→openai(resp:openai→gemini(GEMINI-RESP))"
	if string(out) != want {
		t.Errorf("got %q\nwant %q", out, want)
	}
}

func TestCompose_UsagePrefersUpstreamSide(t *testing.T) {
	upUsage := &domain.Usage{Total: 111}
	clientUsage := &domain.Usage{Total: 999}

	// back (closest to upstream) has usage → use it
	c := Compose(
		fakePairTranslator{src: domain.ProtoAnthropic, tgt: domain.ProtoOpenAI, usage: clientUsage},
		fakePairTranslator{src: domain.ProtoOpenAI, tgt: domain.ProtoGemini, usage: upUsage},
	)
	h := c.NewResponseHandler()
	_, _ = h.Feed([]byte("x"))
	_, usage, _ := h.Flush()
	if usage == nil || usage.Total != 111 {
		t.Errorf("usage should prefer the upstream side (back handler), got %+v", usage)
	}

	// back has no usage → fallback to front
	c2 := Compose(
		fakePairTranslator{src: domain.ProtoAnthropic, tgt: domain.ProtoOpenAI, usage: clientUsage},
		fakePairTranslator{src: domain.ProtoOpenAI, tgt: domain.ProtoGemini},
	)
	h2 := c2.NewResponseHandler()
	_, _ = h2.Feed([]byte("x"))
	_, usage2, _ := h2.Flush()
	if usage2 == nil || usage2.Total != 999 {
		t.Errorf("should fall back to the client side when the upstream side has no usage, got %+v", usage2)
	}
}

func TestCompose_PivotMismatchPanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("pivot mismatch should panic")
		} else if !strings.Contains(r.(string), "pivot mismatch") {
			t.Errorf("panic message is wrong: %v", r)
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
	direct := fakePairTranslator{src: domain.ProtoAnthropic, tgt: domain.ProtoGemini}
	reg := NewRegistry(
		direct,
		fakePairTranslator{src: domain.ProtoAnthropic, tgt: domain.ProtoOpenAI},
		fakePairTranslator{src: domain.ProtoOpenAI, tgt: domain.ProtoGemini},
	)

	got := reg.FindVia(domain.ProtoAnthropic, domain.ProtoGemini, domain.ProtoOpenAI)
	if got == nil {
		t.Fatal("FindVia returned nil")
	}
	if IsComposed(got) {
		t.Error("should not use composition when a direct pair exists")
	}
}

func TestFindVia_ComposesWhenDirectMissing(t *testing.T) {
	reg := NewRegistry(
		fakePairTranslator{src: domain.ProtoAnthropic, tgt: domain.ProtoOpenAI},
		fakePairTranslator{src: domain.ProtoOpenAI, tgt: domain.ProtoGemini},
	)

	got := reg.FindVia(domain.ProtoAnthropic, domain.ProtoGemini, domain.ProtoOpenAI)
	if got == nil {
		t.Fatal("FindVia should compose a translator when both legs are present")
	}
	if !IsComposed(got) {
		t.Error("should be a composed result")
	}
	if got.Source() != domain.ProtoAnthropic || got.Target() != domain.ProtoGemini {
		t.Errorf("span = %s→%s", got.Source(), got.Target())
	}
}

func TestFindVia_MissingLegReturnsNil(t *testing.T) {
	// missing openai→gemini leg
	reg := NewRegistry(fakePairTranslator{src: domain.ProtoAnthropic, tgt: domain.ProtoOpenAI})

	if got := reg.FindVia(domain.ProtoAnthropic, domain.ProtoGemini, domain.ProtoOpenAI); got != nil {
		t.Errorf("should return nil when a leg is missing, got %T", got)
	}
}

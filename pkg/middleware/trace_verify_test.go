package middleware

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/zereker/llm-gateway/pkg/domain"
	"github.com/zereker/llm-gateway/pkg/trace"
)

// installRealTracerProvider 给本测试装一个会真创建子 span 的 TracerProvider，
// 并返回 SpanRecorder 用于断言。默认 otel global 是 noop——noop tracer.Start
// 直接 return 原 ctx + noop span，不会创建独立 span_id。
func installRealTracerProvider(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { otel.SetTracerProvider(prev) })
	return sr
}

// TestE2E_LogsCarryTraceID 端到端验证：CtxHandler 包 slog default → 走 M1 +
// 任意下游 middleware → handler 里调 slog.InfoContext(c.Request.Context(), ...)
// → 日志里必须出现 trace_id / span_id / request_id（来自 baggage）。
//
// **回归保护**：refactor 把 rc.Ctx 删掉后，每个 mw 改成
// c.Request = c.Request.WithContext(ctx)。本测试确保 c.Request.Context() 在
// handler 时刻能拿到 M1 的 span info——如果某个 mw 漏写回 c.Request，trace_id
// 会从日志里消失。
func TestE2E_LogsCarryTraceID(t *testing.T) {
	installRealTracerProvider(t)

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(trace.NewCtxHandler(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))))
	defer slog.SetDefault(prev)

	var handlerTraceID, handlerSpanID, rcRequestID string

	// 串一条「最小但跨多个 mw」的链：M1 → M2(auth) → M3(envelope) → handler
	r := newGinTest(
		TraceContext(),
		Recover(),
		Auth(WithIdentityProvider(stubE2EIdentity{})),
		WithSourceProtocol(domain.ProtoOpenAI, domain.ModalityChat),
		Envelope(),
	)
	r.POST("/v1/chat/completions", func(c *gin.Context) {
		// 在 handler 里：c.Request.Context() 必须能拿到 span info
		ctx := c.Request.Context()
		sc := oteltrace.SpanContextFromContext(ctx)
		handlerTraceID = sc.TraceID().String()
		handlerSpanID = sc.SpanID().String()
		rcRequestID = GetRequestContext(c).RequestID

		slog.InfoContext(ctx, "handler.executed", "where", "business")
		c.Status(200)
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4o","messages":[]}`))
	req.Header.Set("Authorization", "Bearer test-token")
	r.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}

	// 1. handler 拿到的 ctx 必须有 valid SpanContext
	if handlerTraceID == "" || handlerTraceID == "00000000000000000000000000000000" {
		t.Errorf("handler ctx 缺 trace_id：got=%q", handlerTraceID)
	}
	if handlerSpanID == "" || handlerSpanID == "0000000000000000" {
		t.Errorf("handler ctx 缺 span_id：got=%q", handlerSpanID)
	}
	if rcRequestID == "" {
		t.Errorf("rc.RequestID 空")
	}

	// 2. 日志输出每条 record 必须有 trace_id（CtxHandler 自动注入）
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) < 2 {
		t.Fatalf("日志条数 < 2，输出：\n%s", buf.String())
	}
	for i, line := range lines {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Errorf("行 %d 不是 JSON：%s", i, line)
			continue
		}
		// trace_id 必须存在且等于 handler 看到的（同 root span）
		traceID, _ := rec["trace_id"].(string)
		if traceID == "" {
			t.Errorf("行 %d msg=%v 缺 trace_id", i, rec["msg"])
			continue
		}
		if traceID != handlerTraceID {
			t.Errorf("行 %d trace_id=%s, 期望=%s（应跟 handler 同 root span）",
				i, traceID, handlerTraceID)
		}
		// span_id 必须存在
		if sid, _ := rec["span_id"].(string); sid == "" {
			t.Errorf("行 %d msg=%v 缺 span_id", i, rec["msg"])
		}
		// request_id（M1 注入到 baggage） 必须存在
		if rid, _ := rec["request_id"].(string); rid == "" {
			t.Errorf("行 %d msg=%v 缺 request_id（M1 baggage 未传到 ctx？）", i, rec["msg"])
		} else if rid != rcRequestID {
			t.Errorf("行 %d request_id=%s, 期望=%s", i, rid, rcRequestID)
		}
	}
}

// TestE2E_SpansFormHierarchy 用真 OTel SDK 跑链，验证每个 mw 创建的子 span：
//
//  1. trace_id 都 == root span trace_id（同一条链）
//  2. auth.lookup / envelope.parse 的 parent_span_id 链回 root
//  3. handler 处 c.Request.Context() 拿到的是最后一个 mw 的 span
//
// 如果某个 mw 漏 c.Request = c.Request.WithContext(ctx)，子 span 会 orphan
// （parent 是 invalid SpanContext），本测试会捕获。
func TestE2E_SpansFormHierarchy(t *testing.T) {
	sr := installRealTracerProvider(t)

	r := newGinTest(
		TraceContext(),
		Recover(),
		Auth(WithIdentityProvider(stubE2EIdentity{})),
		WithSourceProtocol(domain.ProtoOpenAI, domain.ModalityChat),
		Envelope(),
	)
	r.POST("/v1/chat/completions", func(c *gin.Context) {
		c.Status(200)
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4o","messages":[]}`))
	req.Header.Set("Authorization", "Bearer test-token")
	r.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}

	spans := sr.Ended()
	if len(spans) < 3 {
		t.Fatalf("期望 ≥3 个 span（root + auth + envelope），got=%d", len(spans))
	}

	// 索引 span by name
	byName := make(map[string]sdktrace.ReadOnlySpan, len(spans))
	var rootName string
	for _, s := range spans {
		byName[s.Name()] = s
		// root 是 "POST /v1/chat/completions" 形态
		if strings.HasPrefix(s.Name(), "POST ") {
			rootName = s.Name()
		}
	}
	if rootName == "" {
		t.Fatalf("没找到 root span，实际 spans=%v", spanNames(spans))
	}
	root := byName[rootName]
	auth, ok := byName["auth.lookup"]
	if !ok {
		t.Fatalf("缺 auth.lookup span，spans=%v", spanNames(spans))
	}
	envelope, ok := byName["envelope.parse"]
	if !ok {
		t.Fatalf("缺 envelope.parse span，spans=%v", spanNames(spans))
	}

	rootTID := root.SpanContext().TraceID()
	rootSID := root.SpanContext().SpanID()

	// 所有 span 同 trace_id
	if auth.SpanContext().TraceID() != rootTID {
		t.Errorf("auth.lookup trace_id=%s, 期望=%s", auth.SpanContext().TraceID(), rootTID)
	}
	if envelope.SpanContext().TraceID() != rootTID {
		t.Errorf("envelope.parse trace_id=%s, 期望=%s", envelope.SpanContext().TraceID(), rootTID)
	}

	// auth.lookup parent 必须是 root
	if auth.Parent().SpanID() != rootSID {
		t.Errorf("auth.lookup parent span_id=%s, 期望 root=%s（说明 M2 没继承 M1 的 ctx）",
			auth.Parent().SpanID(), rootSID)
	}
	// envelope.parse parent 必须是 root（M3 紧跟 M2 同级；都是 root 的直接子）
	// 注：envelope 在 c.Request.Context() 上启动 span，那时 c.Request.Context() 是 root span ctx
	// （auth 的 span 已经在 auth 函数 return 时 End 并恢复了——实际上 auth 也只是写 c.Request，没 restore）
	// auth 不 restore c.Request 所以 envelope 启动 span 时 ctx 是 auth ctx
	// 所以 envelope parent 应该是 auth
	if envelope.Parent().SpanID() != auth.SpanContext().SpanID() {
		t.Errorf("envelope.parse parent span_id=%s, 期望 auth=%s（说明 c.Request ctx 接力断了）",
			envelope.Parent().SpanID(), auth.SpanContext().SpanID())
	}

	// 所有 span 的 SpanID 必须各不相同（noop tracer 才会 collapse 成同一个）
	ids := map[oteltrace.SpanID]bool{}
	for _, s := range spans {
		ids[s.SpanContext().SpanID()] = true
	}
	if len(ids) != len(spans) {
		t.Errorf("有 span 共享同一 span_id（noop tracer？），spans=%v", spanNames(spans))
	}
}

func spanNames(ss []sdktrace.ReadOnlySpan) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = s.Name()
	}
	return out
}

// =============================================================================
// stub: IdentityProvider for e2e
// =============================================================================

type stubE2EIdentity struct{}

func (stubE2EIdentity) Resolve(_ context.Context, _ *domain.Credentials) (*domain.UserIdentity, error) {
	return &domain.UserIdentity{
		AccountID:    "acc-1",
		SubAccountID: "sub-1",
		APIKeyID:     "key-1",
		Group:        "default",
	}, nil
}


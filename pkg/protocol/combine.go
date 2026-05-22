package protocol

import (
	"context"
	"net/http"
	"sync"

	"github.com/zereker/llm-gateway/pkg/domain"
	"github.com/zereker/llm-gateway/pkg/protocol/quirks"
	"github.com/zereker/llm-gateway/pkg/translator"
)

// Combine 把 Factory（vendor HTTP 层）+ translator.Translator（body 转换）组装
// 成一个 Handler。**facade 融合的核心 helper**。
//
// **使用形态**：DefaultLookup.Get(ep, srcProto) 在请求时调用 Combine 把当前
// endpoint 的 Factory + 选中的 translator 组合成 Handler；不在 init() 时静态
// 注册。这是 v0.6 把"协议归属"从 vendor 级移到 endpoint 级的体现。
//
// **约束**：translator.Target() 必须 == ep.Protocol；Combine 不验证（运行期
// 高频路径），由 DefaultLookup 在按 translator.Find(src, ep.Protocol) 挑选时保证。
func Combine(ad Factory, tr translator.Translator) Handler {
	if ad == nil {
		panic("protocol.Combine: nil Factory")
	}
	if tr == nil {
		panic("protocol.Combine: nil translator.Translator")
	}
	meta := ad.Metadata()
	return &combined{
		ad: ad,
		tr: tr,
		caps: Capabilities{
			SourceProtocol:      tr.Source(),
			UpstreamProtocol:    tr.Target(),
			SupportedModalities: meta.SupportedModalities,
		},
	}
}

// combined 是 Combine 出来的 Handler 实现——facade 内部仍调原 adapter / translator。
//
// **quirksCache**：endpoint.Quirks JSON 在第一次见时 compile 成 Rewriter；
// 后续同 spec（即 string(rawJSON)）请求直接命中。
//   - key   = string(endpoint.Quirks) — JSON 字面量；同 spec 不同 endpoint 共享
//   - value = quirks.Rewriter
//
// 没主动失效逻辑——deployer 改 SQL 后新 spec 的字符串自然不同，会新增 entry；老
// entry 没 evict（量级一般 < 100，可接受）。需要严格 eviction 时改用 hashicorp lru。
type combined struct {
	ad          Factory
	tr          translator.Translator
	caps        Capabilities
	quirksCache sync.Map // string(ep.Quirks) → quirks.Rewriter
}

func (c *combined) Capabilities() Capabilities { return c.caps }

func (c *combined) PrepareCall(ctx context.Context, ep *domain.Endpoint, srcBody []byte) (*Call, error) {
	// phase 1: translator——客户端协议 → 上游协议 shape
	upstreamBody, err := c.tr.TranslateRequest(srcBody)
	if err != nil {
		return nil, NewPrepareError(PhaseTranslate, err)
	}

	// phase 2: quirks——endpoint 配置的 body + header 微调。
	// **body 和 header 一起跑完再交 adapter**，保持 quirks → adapter 单向管道，
	// adapter 不需要知道 quirks 的存在。
	var extraHeaders http.Header
	if len(ep.Quirks) > 0 {
		rw, err := c.quirksFor(ep.Quirks)
		if err != nil {
			return nil, NewPrepareError(PhaseQuirks, err)
		}
		upstreamBody, err = rw.RewriteBody(upstreamBody)
		if err != nil {
			return nil, NewPrepareError(PhaseQuirks, err)
		}
		extraHeaders = make(http.Header)
		rw.RewriteHeader(extraHeaders) // 对空 header 跑 spec：set / set_default 生效
	}

	// phase 3: adapter——HTTP 信封（URL / Auth / Content-Type），合并 extraHeaders。
	// adapter 内部约定：先拷贝 extraHeaders，再写自己的协议必需 header（后写覆盖
	// quirks），防止 deployer 误改 Authorization / Content-Type 把请求打挂。
	sess, err := c.ad.NewSession(ctx, ep, &domain.RequestEnvelope{
		SourceProtocol: c.caps.SourceProtocol,
		RawBytes:       srcBody, // 备份给可能引用原 body 的 session 实现
	})
	if err != nil {
		return nil, NewPrepareError(PhaseBuild, err)
	}
	req, err := sess.BuildRequest(upstreamBody, extraHeaders)
	if err != nil {
		_ = sess.Close()
		return nil, NewPrepareError(PhaseBuild, err)
	}
	// v0.5 slim session 没有流式状态——构造完即关。
	_ = sess.Close()

	return &Call{Request: req, UpstreamBody: upstreamBody}, nil
}

// quirksFor 拿（或构造）endpoint.Quirks 对应的 Rewriter，sync.Map 缓存。
//
// **key**：string(rawSpec)——同字符串字面量同 Rewriter；不同 endpoint 配置相同
// quirks 时共享同一个编译产物。
//
// **error 不缓存**：compile 失败时不存进 cache，每次重试 compile（让 deployer
// 改了 SQL 之后能立即看到新 spec 生效；缓存错误规则反而难调试）。
func (c *combined) quirksFor(rawSpec []byte) (quirks.Rewriter, error) {
	key := string(rawSpec)
	if cached, ok := c.quirksCache.Load(key); ok {
		return cached.(quirks.Rewriter), nil
	}
	rw, err := quirks.CompileJSON(rawSpec)
	if err != nil {
		return nil, err
	}
	actual, _ := c.quirksCache.LoadOrStore(key, rw)
	return actual.(quirks.Rewriter), nil
}

func (c *combined) NewResponseStream() ResponseStream {
	return &combinedStream{inner: c.tr.NewResponseHandler()}
}

// Classify 透传到 Factory 的 Classifier（如果实现了）。
//
// **接口提升**：Factory 的 Classifier 是可选实现；combined 自动把这能力透出，
// 让上层只 type-assert protocol.Classifier。
func (c *combined) Classify(status int, body []byte) *domain.AdapterError {
	if cls, ok := c.ad.(Classifier); ok {
		return cls.Classify(status, body)
	}
	return nil
}

// combinedStream 包装 translator.ResponseHandler 成 protocol.ResponseStream
// （接口形状一致——只是换包名 + 解 import 循环风险）。
type combinedStream struct {
	inner translator.ResponseHandler
}

func (s *combinedStream) Feed(chunk []byte) ([]byte, error)     { return s.inner.Feed(chunk) }
func (s *combinedStream) Flush() ([]byte, *domain.Usage, error) { return s.inner.Flush() }

package protocol

import (
	"context"

	"github.com/zereker/llm-gateway/pkg/adapter"
	"github.com/zereker/llm-gateway/pkg/domain"
	"github.com/zereker/llm-gateway/pkg/translator"
)

// Combine 把 adapter.Factory（HTTP 层）+ translator.Translator（body 转换）组装
// 成一个 Handler。**facade 融合的核心 helper**——内部仍是 v0.5 的两个抽象，
// 外部只暴露 Handler。
//
// **使用形态**：DefaultLookup.Get(ep, srcProto) 在请求时调用 Combine 把当前
// endpoint 的 adapter + 选中的 translator 组合成 Handler；不在 init() 时静态
// 注册。这是 v0.6 把"协议归属"从 vendor 级移到 endpoint 级的体现。
//
// **约束**：translator.Target() 必须 == ep.Protocol；Combine 不验证（运行期
// 高频路径），由 DefaultLookup 在按 translator.Find(src, ep.Protocol) 挑选时保证。
func Combine(ad adapter.Factory, tr translator.Translator) Handler {
	if ad == nil {
		panic("protocol.Combine: nil adapter.Factory")
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
type combined struct {
	ad   adapter.Factory
	tr   translator.Translator
	caps Capabilities
}

func (c *combined) Capabilities() Capabilities { return c.caps }

func (c *combined) PrepareCall(ctx context.Context, ep *domain.Endpoint, srcBody []byte) (*Call, error) {
	// pre-call phase 1: 协议 body 转换
	upstreamBody, err := c.tr.TranslateRequest(srcBody)
	if err != nil {
		return nil, NewPrepareError(PhaseTranslate, err)
	}

	// pre-call phase 2: vendor HTTP 信封（URL / auth / headers）
	sess, err := c.ad.NewSession(ctx, ep, &domain.RequestEnvelope{
		SourceProtocol: c.caps.SourceProtocol,
		RawBytes:       srcBody, // 备份给可能引用原 body 的 session 实现
	})
	if err != nil {
		return nil, NewPrepareError(PhaseBuild, err)
	}
	req, err := sess.BuildRequest(upstreamBody)
	if err != nil {
		_ = sess.Close()
		return nil, NewPrepareError(PhaseBuild, err)
	}
	// v0.5 slim session 没有流式状态——构造完即关。
	_ = sess.Close()
	return &Call{Request: req, UpstreamBody: upstreamBody}, nil
}

func (c *combined) NewResponseStream() ResponseStream {
	return &combinedStream{inner: c.tr.NewResponseHandler()}
}

// Classify 透传到 adapter 的 Classifier（如果实现了）。
//
// **接口提升**：原本 adapter.Classifier 是单独 type-assert；现在 combined 自动
// 把这能力透出去，让上层只 type-assert protocol.Classifier。
func (c *combined) Classify(status int, body []byte) *domain.AdapterError {
	if cls, ok := c.ad.(adapter.Classifier); ok {
		return cls.Classify(status, body)
	}
	return nil
}

// combinedStream 包装 translator.ResponseHandler 成 protocol.ResponseStream
// （接口形状一致——只是换包名 + 解 import 循环风险）。
type combinedStream struct {
	inner translator.ResponseHandler
}

func (s *combinedStream) Feed(chunk []byte) ([]byte, error)      { return s.inner.Feed(chunk) }
func (s *combinedStream) Flush() ([]byte, *domain.Usage, error) { return s.inner.Flush() }

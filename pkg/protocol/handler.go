// Package protocol 定义端到端协议处理器 Handler——把"vendor HTTP 层 + 协议
// body 转换"两件事融合成一个抽象。
//
// **架构定位**：
//
//	┌──────────┐                                           ┌──────────┐
//	│ Client   │ ────────── Handler ──────────────────── → │ Upstream │
//	│ (proto X)│  PrepareCall (pre-call 协议转换)          │ (proto Y)│
//	│          │  ↓ translate body X→Y                    │          │
//	│          │  ↓ build HTTP request (vendor-specific)  │          │
//	│          │                                           │          │
//	│          │ ← NewResponseStream (post-call 协议转换)─ │          │
//	│          │  ↓ chunk-by-chunk Y→X                    │          │
//	└──────────┘                                           └──────────┘
//
// **跟 v0.5 (split adapter + translator) 的差异**：
//
//	v0.5: 两个独立抽象（adapter + translator），消费侧两次 lookup + match 检查；
//	      protocol.Metadata.NativeProtocol 写死 vendor → 上游协议绑死。
//	v0.6: 一个 Handler 抽象（facade），由 DefaultLookup 按 (endpoint, srcProto)
//	      动态组合 adapter + translator。endpoint 携带 Protocol 字段——同 vendor
//	      可以挂多条不同协议的 endpoint。
//
// **组合时机**：请求级，不是 init()。DefaultLookup.Get(ep, srcProto) 调用：
//  1. protocol.LookupFactory(ep.Vendor) → protocol.Factory（vendor HTTP 实现）
//  2. translator.Find(srcProto, ep.Protocol) → translator.Translator（body 转换）
//  3. Combine(ad, tr) → Handler
//
// 任一缺失 → return nil → eligibility filter 剔除该 endpoint。
package protocol

import (
	"context"
	"io"
	"net/http"

	"github.com/zereker/llm-gateway/pkg/domain"
)

// Capabilities Handler 运行时元信息——描述本次组合的 (srcProto, ep.Protocol)
// + adapter 支持的模态。
//
// **注意**：Vendor 不在这里——它是 endpoint 的属性，不是 Handler 的；Handler
// 是 (adapter, translator) 动态组合，跟具体 endpoint 一一对应只在 PrepareCall
// 调用时才确定。
//
// 用途：metric 标签 / debug log / eligibility 能力过滤。
type Capabilities struct {
	SourceProtocol      domain.Protocol   // 这个 Handler 接受的客户端协议
	UpstreamProtocol    domain.Protocol   // endpoint 上游使用的协议（来源 ep.Protocol）
	SupportedModalities []domain.Modality // adapter 支持的模态
}

// Call PrepareCall 的产出——已准备好发往上游的 HTTP 请求 + 翻译后的 body 副本。
//
// **UpstreamBody 字段**：caller 用来 fan-out audit / observer hook（审计场景
// 要"先记录后发送"上游字节）。Request.Body 已经吃掉了这些字节；UpstreamBody
// 是给 observability 的独立副本——所以即使是大 body 也只 ~2x footprint，
// 不会重复消费 Reader。
type Call struct {
	Request      *http.Request
	UpstreamBody []byte
}

// Handler 一个 (vendor, sourceProtocol) 的端到端协议处理器。
//
// **职责**：
//   - PrepareCall: pre-call 协议转换——把客户端 body 翻译成上游协议，封装成
//     vendor-specific HTTP request
//   - NewResponseStream: post-call 协议转换——chunk-by-chunk 反向翻译响应给客户端
//
// **并发约束**：Handler 实例 MUST be safe for concurrent use（多 gin handler
// 并发调用 PrepareCall / NewResponseStream）。NewResponseStream 返回的
// ResponseStream 是每请求 new 的、单 goroutine 使用的句柄。
type Handler interface {
	Capabilities() Capabilities

	// PrepareCall 把客户端原始 body 转换 + 包装成可发往上游的 HTTP request。
	//
	// 内部两步：
	//   1. translator.TranslateRequest(srcBody) → upstreamBody
	//   2. adapter session BuildRequest(upstreamBody) → *http.Request（URL / auth / headers）
	//
	// 失败分类：
	//   - PrepareError{Phase: PhaseTranslate}: srcBody 不符合 SourceProtocol 的 schema；
	//     调用方应 abort（同请求换 endpoint 也会失败）
	//   - PrepareError{Phase: PhaseBuild}: vendor HTTP 构造失败（极少；通常是 endpoint 配置非法）
	PrepareCall(ctx context.Context, ep *domain.Endpoint, srcBody []byte) (*Call, error)

	// NewResponseStream 每请求一个；负责吃上游响应 chunk → 吐客户端 chunk。
	//
	// 单 goroutine（与 gin handler 同协程）。
	NewResponseStream() ResponseStream
}

// ResponseStream 处理一次上游响应：chunk-by-chunk 喂入；最终 Flush 输出。
//
// **streaming 模式**（identity）：Feed 直接返回 chunk；usage 在 Feed 阶段解析；
// Flush 返回 nil bytes + 已积累的 usage。
//
// **buffer-then-translate 模式**（跨协议）：Feed 全部累积，返回 nil；Flush 一次性
// 翻译累积 body 返回完整客户端格式 body + usage。
type ResponseStream interface {
	Feed(chunk []byte) (clientBytes []byte, err error)
	Flush() (clientBytes []byte, usage *domain.Usage, err error)
}

// TransportDecoder 可选接口：vendor 把上游响应的**传输分帧**解成协议 handler 认识
// 的字节流。用于 wire 传输格式 ≠ 协议 SSE 的场景。
//
// **为什么单独一层**：至今所有 provider 的流式都是 SSE / JSON，传输格式恰好 ≈ 协议
// 格式，一个 ResponseStream 顺手两件事都干了。AWS Bedrock 的 event-stream 是二进制
// 分帧（`vnd.amazon.eventstream`），把 Anthropic 事件裹在帧里——这是**传输层**关注
// 点，跟协议 shape 是两回事。TransportDecoder 在字节进 ResponseStream.Feed **之前**
// 把帧解掉，还原成协议 handler 期望的字节（如 Anthropic SSE）。于是 Bedrock 保持
// protocol=anthropic、复用 anthropic 的 ResponseStream 做 shape 翻译，传输/协议干净分离。
//
// **可选**：Factory 不实现 = 上游字节直接进 handler（SSE/JSON 场景，绝大多数）。
// combined Handler 自动透出 Factory 的这能力（同 Classifier 的透传模式）。
type TransportDecoder interface {
	// DecodeTransport 把 resp.Body 包成"已解帧"的 reader；不需要解码时返 nil
	// （调用方据此判断是否插入解码层）。实现**不负责** Close resp.Body（调用方管）。
	DecodeTransport(resp *http.Response) io.Reader
}

// Classifier 可选接口：vendor 自定义把错误响应 body 细化到 domain.ErrorClass。
//
// invoker 在 HTTP 非 2xx 时通过 type-assert 调用：例 OpenAI 区分 insufficient_quota
// （permanent）vs 真 rate-limit（capacity）；Anthropic 529 overloaded_error → capacity。
//
// **典型用途**：HTTP status 单维度不够细分时
//   - 同样的 429：OpenAI 区分 insufficient_quota（permanent）vs 真 rate-limit（capacity）
//   - 200 + 错误 body：少数厂商把错误塞 200 响应里，HTTP-only 看不出来
//   - 5xx 细分：Anthropic 的 529 overloaded_error 应当走 capacity 而非 transient
//
// **契约**：
//   - 实现 MUST be safe for concurrent use（多 goroutine 同时分类）
//   - body 入参：实现不可保留 slice 引用——如要存进返回的 AdapterError 必须 string(body) / 拷贝
//   - body 可能是部分（M7 limit-read 1KiB），实现要 tolerant of truncated JSON
type Classifier interface {
	Classify(status int, body []byte) *domain.AdapterError
}

// DefaultClassifier 仅按 HTTP 状态分类。Factory 不实现 Classifier 时的 fallback。
type DefaultClassifier struct{}

// Classify 按 HTTP 状态映射到 ErrorClass。
func (DefaultClassifier) Classify(httpStatus int, body []byte) *domain.AdapterError {
	e := &domain.AdapterError{
		HTTPStatus:      httpStatus,
		UpstreamMessage: string(body),
	}
	switch {
	case httpStatus == 429:
		e.Class = domain.ErrRateLimit
	case httpStatus == 401, httpStatus == 403:
		e.Class = domain.ErrPermanent
	case httpStatus >= 400 && httpStatus < 500:
		e.Class = domain.ErrInvalid
	case httpStatus >= 500:
		e.Class = domain.ErrTransient
	default:
		e.Class = domain.ErrUnknown
	}
	return e
}

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
//	      adapter.Metadata.NativeProtocol 写死 vendor → 上游协议绑死。
//	v0.6: 一个 Handler 抽象（facade），由 DefaultLookup 按 (endpoint, srcProto)
//	      动态组合 adapter + translator。endpoint 携带 Protocol 字段——同 vendor
//	      可以挂多条不同协议的 endpoint。
//
// **组合时机**：请求级，不是 init()。DefaultLookup.Get(ep, srcProto) 调用：
//   1. adapter.Get(ep.Vendor) → adapter.Factory（vendor HTTP 实现）
//   2. translator.Find(srcProto, ep.Protocol) → translator.Translator（body 转换）
//   3. Combine(ad, tr) → Handler
//
// 任一缺失 → return nil → eligibility filter 剔除该 endpoint。
package protocol

import (
	"context"
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

// Classifier 可选接口：vendor 自定义把错误响应 body 细化到 domain.ErrorClass。
//
// invoker 在 HTTP 非 2xx 时通过 type-assert 调用：例 OpenAI 区分 insufficient_quota
// （permanent）vs 真 rate-limit（capacity）；Anthropic 529 overloaded_error → capacity。
type Classifier interface {
	Classify(status int, body []byte) *domain.AdapterError
}

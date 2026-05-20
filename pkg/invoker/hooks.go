package invoker

import (
	"context"

	"github.com/zereker/llm-gateway/pkg/domain"
)

// Hook 是 observer 的统一类型；按实现的 Observer 子接口被 Sender 分发。
//
// 一个 Hook 实现可同时满足多个 Observer 接口（duck typing）；Sender 在 New
// 期间一次性分桶，运行期不再 type-assert。
//
// **不修改字节、不中止主流**：observer 用于审计 / 异步日志 / metric 上报这类
// 旁观需求。要改字节 / 拦截违规请走 translator.ResponseHandler 装饰器（M8 moderator
// 形态）。
//
// **同步调用**：Sender 不 spawn goroutine、不做 buffer 池、不做 panic recover。
// hook 慢就拖慢主流；要异步在 OnXxx 内自己 `go func() { ... }()`，并把 chunk
// 拷贝出来（chunk 在回调 return 后会被 io.CopyBuffer / 内部 buf 复用——
// 跟 database/sql.Rows.Scan / bufio.Scanner.Bytes() 同样的约定）。
type Hook any

// =============================================================================
// 5 个 Observer 接口：沿"翻译边界"成对设计
// =============================================================================
//
// Sender 的字节流：
//
//	   client                                            upstream
//	     │                                                  │
//	     ▼                                                  ▼
//	[ srcBody ] ──TranslateRequest──→ [ upstreamBody ] ──→ HTTP
//	     │                                  │
//	  ClientRequest                  UpstreamRequest
//
//	[ client chunk ] ←──Feed──── [ upstream chunk ] ←── HTTP
//	     │                                │
//	  ClientChunk                  UpstreamChunk
//
// "审计原始请求 / 响应"用 Client* 系列；"看上游真实发包内容 / 真实返回"用
// Upstream* 系列。两侧字节在 identity translator 下相同；在跨协议 translator
// （openai_gemini 等）下不同——观察者按需选挂哪几个。
// =============================================================================

// ClientRequestObserver 在 Send 一开始触发（早于 factory / translator 查找）。
//
// 拿到的 body 是 caller 给 Send 的 srcBody——客户端原始请求体（未翻译）。
// 这是网关接收到的原始字节，适合合规审计 / 用户行为分析。
type ClientRequestObserver interface {
	OnClientRequest(ctx context.Context, ep *domain.Endpoint, body []byte)
}

// UpstreamRequestObserver 在 sess.BuildRequest 后、httpc.Do 前触发。
//
// 拿到的 body 是 translator.TranslateRequest 输出——准备发给上游的字节。
// identity 协议下与 ClientRequest 相同；跨协议时是翻译后的形态（如 OpenAI
// 客户端 → Gemini 上游会拿到 Gemini schema body）。
//
// 适合调试 / 上游协议核对；合规场景一般要前者。
type UpstreamRequestObserver interface {
	OnUpstreamRequest(ctx context.Context, ep *domain.Endpoint, body []byte)
}

// UpstreamChunkObserver 在 translatorWriter.Write 入口触发（Feed 之前）。
//
// 拿到的是上游 HTTP body 直接 Read 出来的原始 chunk——未经 translator 翻译、
// 未经 moderator 检查。
//
// 适合调试上游真实返回 / 录制 fixtures 用作回放测试。
//
// **chunk 切片在回调 return 后失效**：要持久化必须 `append([]byte(nil), chunk...)`
// 拷贝。底层 buf 来自 chunkBufPool。
type UpstreamChunkObserver interface {
	OnUpstreamChunk(ctx context.Context, ep *domain.Endpoint, chunk []byte)
}

// ClientChunkObserver 在 translatorWriter inner.Write 之后触发（buffer-then-
// translate 模式下也在 Forward 收尾的 finalOut 写入后触发）。
//
// 拿到的是客户端真实收到的字节——已经过 translator.Feed 翻译 + 装饰器
// （moderator 等）放行；违规被装饰器拦下的 chunk 不会触发本回调。
//
// 适合用户视角的审计 / 与计费数据对账。
//
// **chunk 切片在回调 return 后失效**：要持久化必须 copy。
type ClientChunkObserver interface {
	OnClientChunk(ctx context.Context, ep *domain.Endpoint, chunk []byte)
}

// AttemptCompleteObserver 在 Send 单次调用收尾时触发（成功 / 失败都触发）。
//
// per-attempt 级事件，不是 per-request——一次客户端请求触发多次 Send
// （retry / fallback）就触发多次本回调。per-request 级事件请用 M10 Tracing
// 的 usage.OutboxPublisher。
type AttemptCompleteObserver interface {
	OnAttemptComplete(ctx context.Context, ep *domain.Endpoint, outcome Outcome)
}

// =============================================================================
// 内部分桶 / fan-out
// =============================================================================

// hookSet Sender 启动期分桶后的回调集合；运行期零 type-assert。
type hookSet struct {
	clientReq   []ClientRequestObserver
	upstreamReq []UpstreamRequestObserver
	upstreamChk []UpstreamChunkObserver
	clientChk   []ClientChunkObserver
	complete    []AttemptCompleteObserver
}

// classifyHooks 把 Hook 按实现的子接口分桶；同一个 hook 可同时进多个桶。
//
// 顺序保留：caller 注册顺序就是回调顺序。
func classifyHooks(hooks []Hook) hookSet {
	var hs hookSet
	for _, h := range hooks {
		if o, ok := h.(ClientRequestObserver); ok {
			hs.clientReq = append(hs.clientReq, o)
		}
		if o, ok := h.(UpstreamRequestObserver); ok {
			hs.upstreamReq = append(hs.upstreamReq, o)
		}
		if o, ok := h.(UpstreamChunkObserver); ok {
			hs.upstreamChk = append(hs.upstreamChk, o)
		}
		if o, ok := h.(ClientChunkObserver); ok {
			hs.clientChk = append(hs.clientChk, o)
		}
		if o, ok := h.(AttemptCompleteObserver); ok {
			hs.complete = append(hs.complete, o)
		}
	}
	return hs
}

func (hs hookSet) fireClientRequest(ctx context.Context, ep *domain.Endpoint, body []byte) {
	for _, o := range hs.clientReq {
		o.OnClientRequest(ctx, ep, body)
	}
}

func (hs hookSet) fireUpstreamRequest(ctx context.Context, ep *domain.Endpoint, body []byte) {
	for _, o := range hs.upstreamReq {
		o.OnUpstreamRequest(ctx, ep, body)
	}
}

func (hs hookSet) fireUpstreamChunk(ctx context.Context, ep *domain.Endpoint, chunk []byte) {
	for _, o := range hs.upstreamChk {
		o.OnUpstreamChunk(ctx, ep, chunk)
	}
}

func (hs hookSet) fireClientChunk(ctx context.Context, ep *domain.Endpoint, chunk []byte) {
	for _, o := range hs.clientChk {
		o.OnClientChunk(ctx, ep, chunk)
	}
}

func (hs hookSet) fireComplete(ctx context.Context, ep *domain.Endpoint, out Outcome) {
	for _, o := range hs.complete {
		o.OnAttemptComplete(ctx, ep, out)
	}
}

package dispatch

import (
	"context"
	"net/http"

	"github.com/zereker/llm-gateway/pkg/domain"
	"github.com/zereker/llm-gateway/pkg/protocol"
)

// =============================================================================
// Selector port — 选哪个 endpoint
// =============================================================================

// Selector 在当前 model + 已排除集合下选一个可用 endpoint。
//
// **职责包揽**：候选拉取、eligibility 过滤、filter chain、scoring、cooldown
// 读判定——全部 Selector 内部完成。Dispatcher 不知道 Endpoint 怎么来。
//
// **返回约定**：
//
//	(ep, nil)     ── 选到了
//	(nil, nil)    ── 当前 model 候选耗尽（让 FallbackPolicy 决定切 model 还是 abort）
//	(nil, err)    ── 依赖故障（如 DB / Redis 调用失败），driver 直接 abort 503
type Selector interface {
	Select(ctx context.Context, q Query) (*domain.Endpoint, error)
}

// Query Selector.Select 的入参。
//
// **字段语义**：
//
//	Model    ── 当前轮次的 model（primary 或某个 fallback）
//	Envelope ── 给 eligibility filter 用（modality / protocol 资格判定）
//	Identity ── 给 group-aware filter 用
//	Exclude  ── 本请求里已尝试过的 endpoint ID 集合（跨 model 累加）
//	Handlers ── 请求级 Handler 查询端口（给 eligibility filter 用）
type Query struct {
	Model    string
	Envelope *domain.RequestEnvelope
	Identity domain.UserIdentity
	Exclude  map[int64]struct{}
	Handlers protocol.Lookup
}

// =============================================================================
// Invoker port — 调一次下游
// =============================================================================

// InvokerFactory 按 (endpoint, envelope, body, handler) 造一个待执行的 Invoker。
//
// **不是 interface 是约定**：不同实现可以有完全不同的 For 签名（HTTPFactory.For /
// BatchFactory.For / MockFactory.Pin 等）。Dispatcher 拿到一个 concrete factory
// 即可——装配点（cmd/gateway）决定用哪个实现。
//
// **handler 入参**：请求级端到端协议处理器（dispatcher 已根据 ep + srcProto
// 从 rc.Handlers 取出）；invoker 用 handler.PrepareCall + handler.NewResponseStream
// 走 HTTP，不再自己查 adapter / translator。
//
// 这里给个最小接口，方便 Dispatcher 单测时换 fake 实现。
type InvokerFactory interface {
	For(ep *domain.Endpoint, env *domain.RequestEnvelope, body []byte, handler protocol.Handler) Invoker
}

// Invoker 一次已配置好的下游调用。无参执行。
//
// **职责包揽**：endpoint quota reserve、adapter / translator lookup、HTTP do、
// classify、Selector.Report 回调（通过 hook 内部触发）。Dispatcher 不知道这些。
//
// **返回约定**：err 仅在"无法构造调用"时非 nil（极少见，如 nil endpoint）；
// 上游错误归到 Result.Verdict().Class。
type Invoker interface {
	Invoke(ctx context.Context) (Result, error)
}

// Result Invoke 的产出句柄。
//
// **生命周期**：StreamTo / Close 必须二选一调用一次。
// - StreamTo：仅 Verdict.Class == Success 时可调；开始写 w 即不可回滚。
// - Close：释放未消费的 body + session，可在任何时候调；StreamTo 之后调是 no-op。
//
// **建议用法**：driver 拿到 Result 立即 `defer res.Close()`，StreamTo 后兜底 no-op。
type Result interface {
	Verdict() Verdict
	Endpoint() *domain.Endpoint
	StreamTo(ctx context.Context, w http.ResponseWriter) StreamReport
	Close() error
}

// StreamReport Result.StreamTo 的返回值。
//
// **失败语义**：流式开始（header 已写）后任何错误都不能回滚状态码；Err 仅供
// log / metric / rc.Error 写入，客户端看到的是截断流。
type StreamReport struct {
	Usage  *domain.Usage
	Err    error
	TTFTMs int64
}

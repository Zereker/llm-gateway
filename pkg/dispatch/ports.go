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

// Selector 在当前 model + 已排除集合下选一个可用 endpoint；也接收每次 attempt
// 的 Verdict 反馈用于 cooldown 决策。
//
// **职责范围**：候选拉取由 CandidateSource port 单独负责；Selector 本身只做
// "已知候选集 → 选一个"——eligibility 过滤 + filter chain + scoring + cooldown
// 读判定。Dispatcher 调用 Select 之前应当先经过 CandidateSource 拿候选。
//
// **Select 返回约定**：
//
//	(ep, nil)     ── 选到了
//	(nil, nil)    ── 当前 model 候选耗尽（让 FallbackPolicy 决定切 model 还是 abort）
//	(nil, err)    ── 依赖故障（如 DB / Redis 调用失败），driver 直接 abort 503
//
// **Report**：每次 invoke / reserve 出 verdict 后，Dispatcher 调一次反馈给 Selector
// 内部的 cooldown 状态机（实现侧把 Verdict.Class 翻成自己的 ErrorClass）。
type Selector interface {
	Select(ctx context.Context, q Query) (*domain.Endpoint, error)
	Report(ctx context.Context, ep *domain.Endpoint, v Verdict)
}

// =============================================================================
// CandidateSource port — 按 (model, group) 拉候选 endpoints
// =============================================================================

// CandidateSource 按 (model, group) 拉候选 endpoints 的 port。
//
// **跟 Selector 的关系**：CandidateSource 负责"endpoint 从哪里来"（DB / 缓存 /
// CDC snapshot 都可），Selector 负责"已知候选集挑一个"。两个职责分开，让 selector
// 算法不需要知道存储细节。
type CandidateSource interface {
	ListForModel(ctx context.Context, model, group string) ([]*domain.Endpoint, error)
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

// InvokerFactory 按 (endpoint, handler, envelope) 造一个待执行的 Invoker。
//
// **不是 interface 是约定**：不同实现可以有完全不同的 For 签名（HTTPFactory.For /
// BatchFactory.For / MockFactory.Pin 等）。Dispatcher 拿到一个 concrete factory
// 即可——装配点（cmd/gateway）决定用哪个实现。
//
// **handler 入参**：请求级端到端协议处理器（dispatcher 已根据 ep + srcProto 从
// state.Handlers().Get(ep, srcProto) 取出）。
//
// **body 从哪来**：env.RawBytes（不再单独传 body 参数；invoker 内部读 env）。
//
// 这里给个最小接口，方便 Dispatcher 单测时换 fake 实现。
type InvokerFactory interface {
	For(ep *domain.Endpoint, handler protocol.Handler, env *domain.RequestEnvelope) Invoker
}

// Invoker 一次已配置好的下游调用。无参执行。
//
// **职责包揽**：HTTP do、classify、handler.PrepareCall。
//
// **不职责**：endpoint quota reserve（由 EndpointQuota 单独负责）、Selector.Report
// （由 Dispatcher 在 Invoke 返回后调）。这是 v0.6 把 "reserve → send → report"
// 三件事拆 3 个 port 的结果。
//
// **返回约定**：err 仅在"无法构造调用"时非 nil（极少见，如 nil endpoint）；
// 上游错误归到 Result.Verdict().Class。
type Invoker interface {
	Invoke(ctx context.Context) (Result, error)
}

// =============================================================================
// EndpointQuota port — endpoint 级 ratelimit 前扣 + 后扣
// =============================================================================

// QuotaVerdict 是 EndpointQuota.Reserve 的拒绝结果——比 Verdict 更窄，只描述
// "为什么被拒"。Dispatcher 在拿到 QuotaVerdict 后翻成 Verdict 走 retry / Report 流程。
type QuotaVerdict struct {
	Class     Class  // 一般 ClassCapacity；依赖故障时仍 ClassCapacity（让 retry 换 ep）
	BucketKey string // 哪个 bucket 拒了（rl:endpoint:<id>:rpm 等）；空 = 依赖故障
	Reason    string
}

// EndpointQuota 是 endpoint 级配额的"前扣 + 后扣"端口——dispatcher 在调 invoker
// 之前 Reserve（RPM/RPS hold），成功 stream 之后 ChargeUsage（实际 token 用量）。
//
// **跟用户级 quota 的区别**：用户级 quota 在 M6 middleware（Limit）里做；
// EndpointQuota 是 endpoint 一侧的硬约束（vendor 自身限制 / 自建 fleet 容量保护），
// dispatcher 在 attempt 粒度执行。
type EndpointQuota interface {
	// Reserve 试图给 ep 占用一次 attempt 配额。
	//   返回 (nil, nil)            ── 没配置 quota 或预扣成功
	//   返回 (*QuotaVerdict, nil)  ── 配额拒绝；Dispatcher 翻成 Verdict 走 retry
	//   返回 (_, err)              ── 依赖故障（如 Redis 错）；同样按拒绝处理
	Reserve(ctx context.Context, ep *domain.Endpoint) (denied *QuotaVerdict, err error)

	// ChargeUsage 在成功 stream 之后写真实 token 用量到 TPM bucket（fire-and-forget）。
	// usage / ep 任一为 nil 时 no-op；charge 失败只记 metric，不阻塞响应。
	ChargeUsage(ctx context.Context, ep *domain.Endpoint, usage *domain.Usage)
}

// NoopQuota 永不拒绝、永不 charge——给"没配 ratelimit"的部署用。
type NoopQuota struct{}

func (NoopQuota) Reserve(_ context.Context, _ *domain.Endpoint) (*QuotaVerdict, error) {
	return nil, nil
}
func (NoopQuota) ChargeUsage(_ context.Context, _ *domain.Endpoint, _ *domain.Usage) {}

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

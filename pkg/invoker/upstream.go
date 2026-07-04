// Package upstream 封装"调一次上游 + 流式回写"两个动作；M7 driver loop
// 把 retry / fallback / cooldown 编排留给自己，把 HTTP / protocol Handler /
// classify / 流式 chunk 转发的细节交给本包。
//
// **职责边界（v0.6 融合后）**：
//
//   - 知道：HTTP 调用、按 caller 传入的 protocol.Handler 走 PrepareCall /
//     NewResponseStream、按 HTTP status + Handler.Classify 给 outcome 分类、
//     流式 chunk 拷贝。
//   - 不知道：retry 策略、cooldown、Selection 状态机、HTTP framework
//     （gin / echo / chi 都行），protocol.Handler 实现来源（全局 registry /
//     租户级覆盖均可——caller 决定）。
//
// **使用形态**（M7 内部）：
//
//	sender := invoker.New()
//	for {
//	    ep := sel.Pick()
//	    if ep == nil { break }
//	    handler := lookups.Get(ep, srcProto)
//	    outcome, err := sender.Send(ctx, ep, env, rawBody, handler)
//	    sel.Report(ep, outcome.ToScheduleResult())
//	    if outcome.Success() {
//	        sender.Forward(w, outcome.Response, outcome.Handler.NewResponseStream())
//	        break
//	    }
//	}
//
// 详见 docs/architecture/03-endpoint-scheduling.md。
package invoker

import (
	"errors"
	"net"
	"net/http"
	"time"

	"github.com/zereker/llm-gateway/pkg/protocol"
	"github.com/zereker/llm-gateway/pkg/selector"
)

// HTTPDoer 抽象 HTTP 客户端。*http.Client 自动满足；测试可注入 RoundTripper-like fake。
type HTTPDoer interface {
	Do(*http.Request) (*http.Response, error)
}

// Stage 标记 Send 内部哪一阶段产出本 Outcome——给 wiring 层翻译成
// dispatch.Stage 用，让 Policy.Decide 区分 prepare 失败 vs invoke 失败。
type Stage int

const (
	// StageInvoke HTTP 调用阶段（默认；成功 / 网络错 / 上游 4xx-5xx 都属此阶段）。
	StageInvoke Stage = iota
	// StagePrepare handler.PrepareCall 阶段失败（pre-call 协议转换 / vendor HTTP 构造）。
	StagePrepare
)

// Outcome Send 的结果。
//
// 成功 = Class==ClassSuccess && Response != nil。Response.Body 由 caller 关
// （通常是 Forward 内部 defer Close）。
// 失败 = Response==nil（Send 已自己 close 失败响应的 body）。
type Outcome struct {
	Response *http.Response // 仅成功时填；失败 nil
	Stage    Stage          // 本次 Outcome 产自哪一阶段
	Class    selector.ErrorClass
	HTTPCode int
	Reason   string
	Latency  time.Duration

	// Handler 成功路径下 Forward 时要用的 protocol.Handler；失败时无意义。
	// caller 用 outcome.Handler.NewResponseStream() 拿响应流处理器传给 Forward。
	Handler protocol.Handler
}

// Success outcome 是否成功（HTTP 2xx + 无协议层错）。
func (o Outcome) Success() bool {
	return o.Class == selector.ClassSuccess && o.Response != nil
}

// ToScheduleResult 转成 sel.Report 需要的 selector.Result。
func (o Outcome) ToScheduleResult() selector.Result {
	return selector.Result{
		Class:    o.Class,
		HTTPCode: o.HTTPCode,
		Reason:   o.Reason,
		Latency:  o.Latency,
	}
}

// ErrInvalidRequest Send 翻译请求体失败时返回（caller 应直接 abort 400，
// 不要 retry——同一请求换 endpoint 也会失败）。
var ErrInvalidRequest = errors.New("upstream: invalid request body")

// =============================================================================
// Sender
// =============================================================================

// Sender 封装"调一次上游 + 流式 forward"两个动作。
//
// 不持有请求级状态；Send / Forward 两个方法都可被多请求并发调用。
// **不持任何 lookup**——adapter / translator / handler 都是请求级依赖，
// caller 在调用 Send 时透传。
type Sender struct {
	client HTTPDoer
	hooks  hookSet
}

// Option 装配 Sender 的可选项。
type Option func(*senderConfig)

// senderConfig New 期间 Option 写入的临时配置；New 收尾后产出 Sender。
type senderConfig struct {
	client HTTPDoer
	hooks  []Hook
}

// WithHTTPClient 注入自定义 HTTP 客户端；不调 = http.DefaultClient。
func WithHTTPClient(c HTTPDoer) Option {
	return func(cfg *senderConfig) { cfg.client = c }
}

// WithHooks 注册一组 Hook（observer）。多次调用累加；同一 hook 实现多个
// Observer 接口时同时进多个桶，运行期一次回调（每个桶一次）。
//
// 详见 hooks.go。
func WithHooks(hooks ...Hook) Option {
	return func(c *senderConfig) { c.hooks = append(c.hooks, hooks...) }
}

// per-attempt 超时边界（Transport 级，对本 client 的所有请求生效）：
//
//   - dialTimeout / tlsHandshakeTimeout：连接建立阶段。挂死的 endpoint（accept
//     后不响应 / 半开连接）在这里快速失败，而不是烧光整个请求预算。
//   - responseHeaderTimeout：请求写完到响应头到达（≈ TTFB）。LLM 首 token 可能
//     慢（长 prompt / 冷启动），30s 给足余量，但**有界**——留出 retry / fallback
//     的预算（request 级总超时默认 60s+）。
//   - **不限制**响应 body 读取时长：流式响应合法地可以跑几分钟，由 request 级
//     总超时（middleware.Timeout）兜底。
//
// **为什么不用 http.DefaultClient**：它无任何超时（挂死 = 永久占用），且
// DefaultTransport 的 MaxIdleConnsPerHost=2——高 QPS 打同一上游 host 时连接
// 疯狂重建（延迟 + 端口耗尽）。
const (
	dialTimeout           = 5 * time.Second
	tlsHandshakeTimeout   = 5 * time.Second
	responseHeaderTimeout = 30 * time.Second
	idleConnTimeout       = 90 * time.Second
	maxIdleConns          = 512
	maxIdleConnsPerHost   = 128
)

// defaultHTTPClient 数据面上游调用的默认 client；需要不同参数时用
// WithHTTPClient 覆盖（如 mTLS / 代理 / 自定义超时）。
func defaultHTTPClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			DialContext: (&net.Dialer{
				Timeout:   dialTimeout,
				KeepAlive: 30 * time.Second,
				// SSRF 防线：拨号前拦截云 metadata 端点（按解析后的真实 IP，挡
				// DNS-rebinding）。只挡 metadata，不挡私网自建上游。见 ssrf.go。
				Control: blockMetadataDial,
			}).DialContext,
			ForceAttemptHTTP2:     true,
			TLSHandshakeTimeout:   tlsHandshakeTimeout,
			ResponseHeaderTimeout: responseHeaderTimeout,
			IdleConnTimeout:       idleConnTimeout,
			MaxIdleConns:          maxIdleConns,
			MaxIdleConnsPerHost:   maxIdleConnsPerHost,
		},
		// 不设 Client.Timeout——它包含 body 读取，会掐断长流。
		// 阶段性超时全部在 Transport 上。
	}
}

// New 构造 Sender；零配置走 defaultHTTPClient + 无 hook。
//
// protocol.Handler 在 Send 调用时由 caller 传入；Sender 本身不持有，
// 支持多租户 / 灰度场景按请求覆盖。
func New(opts ...Option) *Sender {
	cfg := &senderConfig{
		client: defaultHTTPClient(),
	}

	for _, opt := range opts {
		opt(cfg)
	}

	return &Sender{
		client: cfg.client,
		hooks:  classifyHooks(cfg.hooks),
	}
}

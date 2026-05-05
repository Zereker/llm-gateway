// Package upstream 封装"调一次上游 + 流式回写"两个动作；M7 driver loop
// 把 retry / fallback / cooldown 编排留给自己，把 HTTP / adapter / translator /
// classify / 流式 chunk 转发的细节交给本包。
//
// **职责边界**：
//
//   - 知道：HTTP 调用、找 adapter.Factory / translator.Translator、按 HTTP status +
//     Adapter Classifier 给 outcome 分类、流式 chunk 拷贝。
//   - 不知道：retry 策略、cooldown、Selection 状态机、HTTP framework
//     （gin / echo / chi 都行——只要满足 stdlib http.ResponseWriter 接口）。
//
// **使用形态**（M7 内部）：
//
//	sender := upstream.New()  // 默认 = adapter 全局 registry + http.DefaultClient
//	for {
//	    ep := sel.Pick()
//	    if ep == nil { break }
//	    outcome, err := sender.Send(ctx, ep, env, rawBody)
//	    sel.Report(ep, outcome.ToScheduleResult())
//	    if outcome.Success() {
//	        sender.Forward(w, outcome.Response, handler)
//	        break
//	    }
//	}
//
// 详见 docs/architecture/03-endpoint-scheduling.md。
package upstream

import (
	"errors"
	"net/http"
	"time"

	"github.com/zereker-labs/ai-gateway/pkg/adapter"
	"github.com/zereker-labs/ai-gateway/pkg/schedule"
	"github.com/zereker-labs/ai-gateway/pkg/translator"
)

// FactoryLookup 抽象 vendor → adapter.Factory 查询。
//
// 默认走全局 registry（adapter.Get）；测试可注入 fake 实现避开 init() 注册副作用。
type FactoryLookup interface {
	Get(vendor string) adapter.Factory
}

// HTTPDoer 抽象 HTTP 客户端。*http.Client 自动满足；测试可注入 RoundTripper-like fake。
type HTTPDoer interface {
	Do(*http.Request) (*http.Response, error)
}

// Outcome Send 的结果。
//
// 成功 = Class==ClassSuccess && Response != nil。Response.Body 由 caller 关
// （通常是 Forward 内部 defer Close）。
// 失败 = Response==nil（Send 已自己 close 失败响应的 body）。
type Outcome struct {
	Response *http.Response // 仅成功时填；失败 nil
	Class    schedule.ErrorClass
	HTTPCode int
	Reason   string
	Latency  time.Duration

	// Translator 成功路径下 Forward 时要用的 translator；失败时无意义。
	// Send 已经选好（factory + translator 都查过 registry），caller 直接用。
	Translator translator.Translator
}

// Success outcome 是否成功（HTTP 2xx + 无协议层错）。
func (o Outcome) Success() bool {
	return o.Class == schedule.ClassSuccess && o.Response != nil
}

// ToScheduleResult 转成 sel.Report 需要的 schedule.Result。
func (o Outcome) ToScheduleResult() schedule.Result {
	return schedule.Result{
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
type Sender struct {
	lookup FactoryLookup
	client HTTPDoer
	hooks  hookSet
}

// Option 装配 Sender 的可选项。
type Option func(*senderConfig)

// senderConfig New 期间 Option 写入的临时配置；New 收尾后产出 Sender。
type senderConfig struct {
	lookup FactoryLookup
	client HTTPDoer
	hooks  []Hook
}

// WithFactoryLookup 注入自定义 vendor → factory 查询；不调 = 走 adapter.Get。
func WithFactoryLookup(l FactoryLookup) Option {
	return func(c *senderConfig) { c.lookup = l }
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

// New 构造 Sender；零配置走 stdlib + adapter 全局 registry + 无 hook。
func New(opts ...Option) *Sender {
	cfg := &senderConfig{
		lookup: defaultFactoryLookup{},
		client: http.DefaultClient,
	}

	for _, opt := range opts {
		opt(cfg)
	}

	return &Sender{
		lookup: cfg.lookup,
		client: cfg.client,
		hooks:  classifyHooks(cfg.hooks),
	}
}

// defaultFactoryLookup 走 adapter 全局 registry。
type defaultFactoryLookup struct{}

func (defaultFactoryLookup) Get(vendor string) adapter.Factory { return adapter.Get(vendor) }

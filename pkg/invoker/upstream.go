// Package upstream 封装"调一次上游 + 流式回写"两个动作；M7 driver loop
// 把 retry / fallback / cooldown 编排留给自己，把 HTTP / adapter / translator /
// classify / 流式 chunk 转发的细节交给本包。
//
// **职责边界**：
//
//   - 知道：HTTP 调用、按 caller 传入的 AdapterLookup / TranslatorLookup 查
//     adapter.Factory / translator.Translator、按 HTTP status + Adapter
//     Classifier 给 outcome 分类、流式 chunk 拷贝。
//   - 不知道：retry 策略、cooldown、Selection 状态机、HTTP framework
//     （gin / echo / chi 都行——只要满足 stdlib http.ResponseWriter 接口）、
//     lookup 实现来源（全局 registry / 租户级覆盖均可——caller 决定）。
//
// **使用形态**（M7 内部）：
//
//	sender := invoker.New()
//	for {
//	    ep := sel.Pick()
//	    if ep == nil { break }
//	    outcome, err := sender.Send(ctx, ep, env, rawBody, adapters, translators)
//	    sel.Report(ep, outcome.ToScheduleResult())
//	    if outcome.Success() {
//	        sender.Forward(w, outcome.Response, handler)
//	        break
//	    }
//	}
//
// 详见 docs/architecture/03-endpoint-scheduling.md。
package invoker

import (
	"errors"
	"net/http"
	"time"

	"github.com/zereker/llm-gateway/pkg/selector"
	"github.com/zereker/llm-gateway/pkg/translator"
)

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
	Class    selector.ErrorClass
	HTTPCode int
	Reason   string
	Latency  time.Duration

	// Translator 成功路径下 Forward 时要用的 translator；失败时无意义。
	// Send 已经选好（factory + translator 都查过 registry），caller 直接用。
	Translator translator.Translator
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
// **不再持 adapter / translator 查询端口**——这些是请求级依赖，每次 Send 由
// caller（dispatch wiring 层）按 rc 取出后透传。
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

// New 构造 Sender；零配置走 stdlib + 无 hook。
//
// AdapterLookup / TranslatorLookup 在 Send 调用时由 caller 传入；Sender 本身
// 不持有，以支持多租户 / 灰度场景按请求覆盖。
func New(opts ...Option) *Sender {
	cfg := &senderConfig{
		client: http.DefaultClient,
	}

	for _, opt := range opts {
		opt(cfg)
	}

	return &Sender{
		client: cfg.client,
		hooks:  classifyHooks(cfg.hooks),
	}
}

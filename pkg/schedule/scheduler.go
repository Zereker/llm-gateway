// Package schedule 实现端点选择主调度链 + 加权随机 + Profile。
//
// 各 Filter 实现见 pkg/schedule/filter；RetryExecutor 见 pkg/retry。
// 详见 docs/architecture/03-endpoint-scheduling.md。
package schedule

import (
	"context"

	"github.com/zereker-labs/ai-gateway/pkg/ctx"
)

// Scheduler 调度链路的入口；输入候选池 + 上下文，输出一个 endpoint。
//
// 调用方（pkg/retry.Executor）在 fallback 时通过 PickInput.Excluded 排除已尝试的 endpoint。
type Scheduler interface {
	Pick(c context.Context, in PickInput) (*ctx.Endpoint, *ctx.SchedulingDecision, error)
}

// PickInput Scheduler.Pick 的输入。
type PickInput struct {
	Identity     ctx.UserIdentity
	ModelService *ctx.ModelServiceSnapshot
	Excluded     map[string]struct{} // 已尝试过的 endpoint ID（L2 fallback 用）
	PromptHash   string              // M3 / M5 预计算的 prompt 前 N 字符 hash
	Profile      *Profile            // 该 model 的调度 profile
}

// Profile 每个 model 的调度策略；通过 ConfigStore 下发，秒级生效。
//
// RetryPolicy 不放在这里（避免 schedule → retry 循环依赖）；
// pkg/retry 自带 DefaultPolicy，运行时由 cmd 装配组合。
type Profile struct {
	EnablePrefixCache  bool
	EnableBusy         bool
	EnableRPSScheduler bool
	EnableTPMScheduler bool
	EnableRPMScheduler bool
	PrefixHashLength   int      // prompt 前 N 字符
	GroupStrict        bool
	FilterChain        []string // 自定义 Filter 顺序；空时用默认
}

// DefaultProfile 给新模型用的默认 profile。
var DefaultProfile = Profile{
	EnablePrefixCache:  true,
	EnableBusy:         true,
	EnableRPSScheduler: true,
	EnableTPMScheduler: true,
	EnableRPMScheduler: true,
	PrefixHashLength:   32,
	GroupStrict:        true,
}

// Package schedule 端点选择全家桶：Scheduler + Filter + RetryExecutor + Cooldown + Health。
//
// 详见 docs/architecture/03-endpoint-scheduling.md。
package schedule

import (
	"context"

	"github.com/zereker-labs/ai-gateway/pkg/domain"
)

// Scheduler 调度链路的入口；输入候选池 + 上下文，输出一个 endpoint。
//
// 调用方（RetryExecutor）在 fallback 时通过 PickInput.Excluded 排除已尝试的 endpoint。
type Scheduler interface {
	Pick(c context.Context, in PickInput) (*domain.Endpoint, *domain.SchedulingDecision, error)
}

// PickInput Scheduler.Pick 的输入。
type PickInput struct {
	Identity     domain.UserIdentity
	ModelService *domain.ModelServiceSnapshot
	Excluded     map[string]struct{} // 已尝试过的 endpoint ID（L2 fallback 用）
	PromptHash   string              // M3 / M5 预计算的 prompt 前 N 字符 hash
	Profile      *Profile            // 该 model 的调度 profile
}

// Profile 每个 model 的调度策略；通过 ConfigStore 下发，秒级生效。
type Profile struct {
	EnablePrefixCache  bool
	EnableBusy         bool
	EnableRPSScheduler bool
	EnableTPMScheduler bool
	EnableRPMScheduler bool
	PrefixHashLength   int      // prompt 前 N 字符
	GroupStrict        bool
	FilterChain        []string // 自定义 Filter 顺序；空时用默认
	RetryPolicy        RetryPolicy
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
	RetryPolicy:        DefaultRetryPolicy,
}

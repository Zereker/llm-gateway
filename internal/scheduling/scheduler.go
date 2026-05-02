package scheduling

import (
	"context"

	"github.com/gin-gonic/gin"

	"github.com/zereker-labs/ai-gateway/internal/identity"
	"github.com/zereker-labs/ai-gateway/internal/modelservice"
)

// Scheduler 调度链路的入口；输入候选池 + 上下文，输出一个 endpoint。
//
// 调用方（RetryExecutor）在 fallback 时通过 PickInput.Excluded 排除已尝试的 endpoint。
type Scheduler interface {
	Pick(ctx context.Context, in PickInput) (*Endpoint, *Decision, error)
}

// PickInput Scheduler.Pick 的输入。
type PickInput struct {
	Identity     identity.User
	ModelService *modelservice.Snapshot
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
	RetryPolicy        Policy
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
	RetryPolicy: Policy{
		MaxAttemptsPerEndpoint: 2,
		MaxTotalAttempts:       5,
		Backoff: BackoffStrategy{
			InitialMs: 100,
			MaxMs:     5000,
			Factor:    2.0,
			Jitter:    0.2,
		},
	},
}

// RetryExecutor M7 Schedule middleware 的执行体：
// 选 endpoint → 调 Adapter → 失败决定 retry / fallback。
//
// 实现位于 internal/scheduling/executor/（依赖 request.Context，避免本包循环引用）。
type RetryExecutor interface {
	Run(c *gin.Context) error
}

// Policy RetryExecutor 的策略配置。
type Policy struct {
	MaxAttemptsPerEndpoint  int  // L1 retry 上限（默认 2，含首次共 3 次）
	MaxTotalAttempts        int  // 跨 endpoint 总上限（默认 5）
	Backoff                 BackoffStrategy
	AllowCrossModelFallback bool // L3 开关（默认 false）
}

// BackoffStrategy 指数退避配置。
type BackoffStrategy struct {
	InitialMs int
	MaxMs     int
	Factor    float64
	Jitter    float64
}

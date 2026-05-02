// Package retry 实现 RetryExecutor 三层容灾：
// L1 同 endpoint retry / L2 换 endpoint / L3 跨模型 (可选)。
//
// 详见 docs/architecture/03-endpoint-scheduling.md 第 6 节。
package retry

import "github.com/gin-gonic/gin"

// Executor M7 Schedule middleware 的执行体。
//
// 接口仅声明 Run；具体实现需要持有 Scheduler / AdapterFactory / CooldownManager 等依赖。
type Executor interface {
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

// DefaultPolicy 默认重试策略。
var DefaultPolicy = Policy{
	MaxAttemptsPerEndpoint:  2,
	MaxTotalAttempts:        5,
	Backoff:                 BackoffStrategy{InitialMs: 100, MaxMs: 5000, Factor: 2.0, Jitter: 0.2},
	AllowCrossModelFallback: false,
}

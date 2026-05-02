package schedule

import "github.com/gin-gonic/gin"

// RetryExecutor M7 Schedule middleware 的执行体：
// L1 同 endpoint retry / L2 换 endpoint / L3 跨模型 (可选)。
//
// 详见 docs/architecture/03-endpoint-scheduling.md 第 6 节。
//
// 接口仅声明 Run；具体实现需要持有 Scheduler / AdapterFactory / CooldownManager 等依赖。
//
// Implementations MUST be safe for concurrent use（每个 gin handler 一个 goroutine 调用 Run）。
// 注：Run 同时把分类错误写入 rc.Error，并 return error 给 caller 用于 metric 统计；
// 两个真值源应保持一致（实现责任）。
type RetryExecutor interface {
	Run(c *gin.Context) error
}

// RetryPolicy RetryExecutor 的策略配置。
type RetryPolicy struct {
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

// DefaultRetryPolicy 默认重试策略。
var DefaultRetryPolicy = RetryPolicy{
	MaxAttemptsPerEndpoint:  2,
	MaxTotalAttempts:        5,
	Backoff:                 BackoffStrategy{InitialMs: 100, MaxMs: 5000, Factor: 2.0, Jitter: 0.2},
	AllowCrossModelFallback: false,
}

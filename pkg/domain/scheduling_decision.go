package domain

import "time"

// SchedulingDecision 调度决策的完整 trace。
//
// 由 RetryExecutor 在执行过程中累积填充，最终写到 RequestContext.SchedulingDecision。
type SchedulingDecision struct {
	Model             string         // ModelServiceSnapshot.Model
	UserGroup         string         // UserIdentity.Group
	CandidatesInitial int            // LoadEndpoints 后的数量
	CandidatesFinal   int            // 所有 Filter 后剩余数量
	Selected          *Endpoint      // 首次选中的 endpoint（nil 表示无可用）
	Filters           []FilterRecord // 每个 Filter 的产出
	Attempts          []Attempt      // 实际请求尝试链（含 retry / fallback）
	DurationMs        int64          // 调度本身耗时（不含上游耗时）
}

// FilterRecord 单个 Filter 的产出。
type FilterRecord struct {
	Name      string   // "CooldownFilter" / "GroupFilter" / "HealthFilter" / ...
	Removed   []string // 被淘汰的 endpoint ID 列表
	Reason    string   // 一行说明
	Preferred string   // PrefixCacheScheduler 等"打分倾向"产出（可选）
}

// Attempt 单次请求尝试。
type Attempt struct {
	Index      int // 第几次尝试（1 起）
	EndpointID string
	Outcome    AttemptOutcome
	LatencyMs  int64
	ErrorClass string // ErrorClass.String()，成功时为空
	Started    time.Time
}

// AttemptOutcome 尝试的结果分类。
type AttemptOutcome int

const (
	AttemptUnknown  AttemptOutcome = iota
	AttemptSuccess                 // 上游返回成功
	AttemptRetry                   // 同 endpoint 重试中（中间状态）
	AttemptFallback                // 失败，已切到下一 endpoint
	AttemptFail                    // 终态失败
)

func (o AttemptOutcome) String() string {
	switch o {
	case AttemptSuccess:
		return "success"
	case AttemptRetry:
		return "retry"
	case AttemptFallback:
		return "fallback"
	case AttemptFail:
		return "fail"
	default:
		return "unknown"
	}
}

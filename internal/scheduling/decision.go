package scheduling

import "time"

// Decision 调度决策的完整 trace。
//
// 由 RetryExecutor 在执行过程中累积填充，最终写到 request.Context.SchedulingDecision。
type Decision struct {
	Model             string         // ModelService.Model
	UserGroup         string         // Identity.Group
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
	Index      int    // 第几次尝试（1 起）
	EndpointID string
	Outcome    string    // "success" / "retry" / "fallback" / "fail"
	LatencyMs  int64
	ErrorClass string    // errs.Class.String()，成功时为空
	Started    time.Time
}

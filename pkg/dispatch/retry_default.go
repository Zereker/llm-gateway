package dispatch

// DefaultRetry 复现现状（pkg/middleware/selector.go driver loop）的 verdict → action 映射。
//
// 映射规则：
//
//	Success     → Stream{}            （让 Dispatcher 调 res.StreamTo）
//	Invalid     → Abort{400}          （translator 失败 / 客户端错；不重试）
//	Retryable   → Continue{}          （Transient / Capacity / Permanent / Unknown）
//	Non-retry   → Abort{502}          （兜底，理论上 IsRetryable 已覆盖）
//
// **可替换**：实现 RetryPolicy 接口写新策略——cost-aware retry / circuit breaker /
// exponential backoff / 时间窗口 budget 等。
type DefaultRetry struct{}

// Decide 把 Verdict 翻译成 Action。
func (DefaultRetry) Decide(_ State, v Verdict) Action {
	switch v.Class {
	case ClassSuccess:
		return Stream{}
	case ClassInvalid:
		return Abort{
			Result:   OutcomeInvalid,
			Class:    v.Class,
			HTTPCode: 400,
			Reason:   v.Reason,
		}
	}
	if v.Class.IsRetryable() {
		return Continue{}
	}
	return Abort{
		Result:   OutcomeTerminal,
		Class:    v.Class,
		HTTPCode: 502,
		Reason:   v.Reason,
	}
}

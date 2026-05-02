// Package budget 定义预算 / 配额状态枚举。
//
// Status 是 M4 Budget middleware 的产物，由 budget.Checker 实现填充。
package budget

// Status 预算状态。
type Status int

const (
	Unknown  Status = iota // 全 miss 或检查未发生
	Active                 // 通过
	Inactive               // 欠费 / 订阅过期 / 配额耗尽
)

func (s Status) String() string {
	switch s {
	case Active:
		return "active"
	case Inactive:
		return "inactive"
	default:
		return "unknown"
	}
}

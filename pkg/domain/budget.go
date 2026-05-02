package domain

// BudgetStatus M4 Budget middleware 的产物。
type BudgetStatus int

const (
	BudgetUnknown  BudgetStatus = iota
	BudgetActive
	BudgetInactive // 欠费 / 订阅过期 / 配额耗尽
)

func (s BudgetStatus) String() string {
	switch s {
	case BudgetActive:
		return "active"
	case BudgetInactive:
		return "inactive"
	default:
		return "unknown"
	}
}

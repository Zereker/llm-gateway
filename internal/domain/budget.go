package domain

// BudgetStatus is the product of the M4 Budget middleware.
type BudgetStatus int

const (
	BudgetUnknown BudgetStatus = iota
	BudgetActive
	BudgetInactive // in arrears / subscription expired / quota exhausted
)

func (s BudgetStatus) String() string {
	switch s {
	case BudgetActive:
		return "active"
	case BudgetInactive:
		return "inactive"
	default:
		return unknownLabel
	}
}

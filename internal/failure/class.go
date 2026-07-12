// Package failure defines the small, shared failure vocabulary used across
// transport, dispatch and endpoint-selection boundaries.
package failure

// Class describes whether an operation succeeded and, on failure, whether a
// different endpoint may reasonably succeed. Protocol/HTTP response mapping
// remains the responsibility of the transport boundary.
type Class int

const (
	Unknown Class = iota
	Success
	Transient
	Capacity
	Permanent
	Invalid
)

func (c Class) String() string {
	switch c {
	case Success:
		return "success"
	case Transient:
		return "transient"
	case Capacity:
		return "capacity"
	case Permanent:
		return "permanent"
	case Invalid:
		return "invalid"
	default:
		return "unknown"
	}
}

// IsRetryable reports whether trying another endpoint can be useful.
func (c Class) IsRetryable() bool {
	return c != Success && c != Invalid
}

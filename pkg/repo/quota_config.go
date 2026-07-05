package repo

import (
	"database/sql/driver"
	"encoding/json"
)

// QuotaConfig is the endpoints.quota column: the hard rate-limit constraints
// imposed by the upstream API.
//
// **Sparse-field semantics**:
//   - nil = this quota doesn't exist for this vendor (e.g. GeminiAuth only has
//     RPM; TPM/RPS are both nil)
//   - 0   = the quota exists but is "unlimited" (rare, usually a positive nonzero number)
//   - positive number = the actual quota value
//
// M6 RateLimit middleware (fully implemented in v0.5+) decides whether to
// enforce based on field presence:
//   - present -> counted against the rate limiter
//   - absent  -> not enforced
//
// Not encrypted — quota numbers aren't sensitive.
type QuotaConfig struct {
	RPM                *uint32 `json:"rpm,omitempty"`
	TPM                *uint32 `json:"tpm,omitempty"`
	RPS                *uint32 `json:"rps,omitempty"`
	ConcurrentRequests *uint32 `json:"concurrent_requests,omitempty"`
}

// Scan implements sql.Scanner.
func (q *QuotaConfig) Scan(value any) error {
	if value == nil {
		*q = QuotaConfig{}
		return nil
	}
	b, err := bytesFromScan(value, "QuotaConfig")
	if err != nil {
		return err
	}
	if len(b) == 0 {
		*q = QuotaConfig{}
		return nil
	}
	return json.Unmarshal(b, q)
}

// Value implements driver.Valuer; writes NULL when all fields are empty (the
// schema column is nullable JSON).
func (q QuotaConfig) Value() (driver.Value, error) {
	if q.RPM == nil && q.TPM == nil && q.RPS == nil && q.ConcurrentRequests == nil {
		return nil, nil
	}
	return json.Marshal(q)
}

// IsEmpty reports whether QuotaConfig has no quota fields set at all.
func (q QuotaConfig) IsEmpty() bool {
	return q.RPM == nil && q.TPM == nil && q.RPS == nil && q.ConcurrentRequests == nil
}

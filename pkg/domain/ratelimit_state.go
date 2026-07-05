package domain

// RateLimitState is the product of the M6 RateLimit middleware, passed to
// M10 for TPM reconciliation.
//
// **TPM two-phase**:
//   - M6 reserve estimates cost (input chars/4 + max_tokens); records TPMBucketKeys + ReservedTPM
//   - M10 computes delta from the real rc.Usage.Total - ReservedTPM; when nonzero, store.AdjustBatch(TPMBucketKeys, delta)
//
// **TightestBucketKey + TightestLimit + TightestUsed + TightestResetAtUnix**:
// once M6 reserve succeeds, it snapshots the tightest bucket (ascending by
// Limit) here; M10 attaches it as the basis for X-RateLimit-* headers when
// dumping the usage event.
//
// There's no reference to ratelimit.Bucket, to avoid domain reverse-depending
// on pkg/ratelimit.
type RateLimitState struct {
	// TPM two-phase reconciliation data
	ReservedTPM   uint32   // total token cost estimated by M6
	TPMBucketKeys []string // these buckets get AdjustBatch'd together at M10 commit time

	// Tightest bucket (used for X-RateLimit-* headers / debug)
	TightestBucketKey  string
	TightestLimit      uint32
	TightestUsed       uint32
	TightestResetAtSec int64
	TightestWindowSec  int64
}

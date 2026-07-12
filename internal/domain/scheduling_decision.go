package domain

import "time"

// SchedulingDecision is the full trace of a scheduling decision.
//
// Accumulated by M7 during execution, and finally written to
// RequestContext.SchedulingDecision.
type SchedulingDecision struct {
	Model             string         // the original requested model
	RoutedModel       string         // the model actually succeeded; = Model when no fallback occurred
	UserGroup         string         // UserIdentity.Group
	CandidatesInitial int            // count after LoadEndpoints
	CandidatesFinal   int            // count remaining after all Filters
	Selected          *Endpoint      // the first endpoint selected (nil means none available)
	Filters           []FilterRecord // output of each Filter
	Attempts          []Attempt      // the actual request attempt chain (including retry / fallback)
	DurationMs        int64          // time spent on scheduling itself (excludes upstream time)
}

// FilterRecord is the output of a single Filter.
type FilterRecord struct {
	Name      string   // "CooldownFilter" / "GroupFilter" / "HealthFilter" / ...
	Removed   []string // list of eliminated endpoint IDs
	Reason    string   // one-line explanation
	Preferred string   // output of a "scoring preference" like PrefixCacheScheduler (optional)
}

// AttemptRole identifies the model role this attempt corresponds to.
//
// Sourced from docs/architecture/03-endpoint-scheduling.md §11; used as the
// single source of truth for the trace / metric attempt_role label.
type AttemptRole string

const (
	AttemptRolePrimary  AttemptRole = "primary"  // the original requested model
	AttemptRoleFallback AttemptRole = "fallback" // from X-Gateway-Fallback-Models
)

// Attempt is a single request attempt.
type Attempt struct {
	Index       int    // which attempt number (starting at 1)
	Model       string // the model this attempt corresponds to (differs across fallback)
	EndpointID  string
	AttemptRole AttemptRole // primary | fallback
	Outcome     AttemptOutcome
	LatencyMs   int64
	ErrorClass  string // ErrorClass.String() / selector.ErrorClass.String()
	Started     time.Time
}

// AttemptOutcome classifies the outcome of an attempt.
type AttemptOutcome int

const (
	AttemptUnknown  AttemptOutcome = iota
	AttemptSuccess                 // upstream returned success
	AttemptRetry                   // retrying on the same endpoint (intermediate state)
	AttemptFallback                // failed, already switched to the next endpoint
	AttemptFail                    // terminal failure
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
		return unknownLabel
	}
}

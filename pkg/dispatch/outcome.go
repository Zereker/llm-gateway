package dispatch

import "github.com/zereker/llm-gateway/pkg/domain"

// Outcome is Dispatch's final output, translated by middleware into an HTTP
// response + written back onto RC.
//
// **Semantics**:
//
//	Result == OutcomeStreamed ── the response has already been written to w
//	                              via Result.StreamTo; middleware must not write again
//	Result != OutcomeStreamed ── middleware needs to write an error response
//	                              based on HTTPCode / Class / Reason
//
// **Decision**: always populated (even when attempts == 0, a
// SchedulingDecision is still written, for auditing / logging).
// **StreamErr**: may be non-nil only when Result == OutcomeStreamed; a
// failure that happened mid-stream (headers already written, bytes already
// sent, can't roll back).
// **RoutedModel**: the model that actually succeeded (!= Input.PrimaryModel()
// on fallback); nil when Result != OutcomeStreamed.
// **Error**: the dispatcher never writes rc.Error directly — it puts the
// AdapterError in Outcome and lets the middleware write it back onto RC.
type Outcome struct {
	Result      OutcomeResult
	HTTPCode    int    // only meaningful when Result != OutcomeStreamed
	Class       Class  // only meaningful when Result == OutcomeAbort
	Reason      string // only meaningful when Result != OutcomeStreamed
	Decision    *domain.SchedulingDecision
	Usage       *domain.Usage        // filled only when Result == OutcomeStreamed
	StreamErr   error                // only when Result == OutcomeStreamed and streaming failed
	TTFTMs      int64                // only when Result == OutcomeStreamed
	RoutedModel *domain.ModelService // the model that actually succeeded; nil unless streamed
	Error       *domain.AdapterError // typed wrapper for a stream-stage error; nil = no error
}

// OutcomeResult is Dispatch's terminal state.
type OutcomeResult int

const (
	OutcomeUnknown    OutcomeResult = iota
	OutcomeStreamed                 // success, response already streamed to the client
	OutcomeInvalid                  // client error (400)
	OutcomeTerminal                 // non-retryable upstream error (502)
	OutcomeNoEndpoint               // all models / attempts exhausted (503)
	OutcomeDepFail                  // Selector dependency failure (503, Reason contains the SQL/Redis error)
)

func (r OutcomeResult) String() string {
	switch r {
	case OutcomeStreamed:
		return "streamed"
	case OutcomeInvalid:
		return "invalid"
	case OutcomeTerminal:
		return "terminal"
	case OutcomeNoEndpoint:
		return "no_endpoint"
	case OutcomeDepFail:
		return "dep_fail"
	default:
		return "unknown"
	}
}

package dispatch

import (
	"strconv"
	"time"

	"github.com/zereker/llm-gateway/internal/domain"
	"github.com/zereker/llm-gateway/internal/protocol"
)

// State is the read-only runtime projection exposed to Policy.
//
// **Design principle**: Policy is an externally injectable implementation;
// State exposes an interface rather than a *state pointer, to prevent an
// external Policy from mutating driver state by mistake. Dispatcher uses
// *state internally to get the mutable view.
//
// **Field semantics**:
//
//	Attempts        ── number of completed attempts (accumulated across
//	                    models, starting at 1)
//	AttemptsCap     ── the max attempts allowed for this request (from
//	                    AttemptCap.Resolve)
//	IsExcluded      ── tests whether an endpoint was already tried
//	                    (accumulated across models)
//	CurrentModel    ── the model used by the current attempt loop (updated
//	                    on fallback switches)
//	NextFallback    ── returns a copy of the next unused model
//	LastVerdict     ── the Verdict from the last Invoker.Invoke (zero value
//	                    before the first attempt)
type State interface {
	Attempts() int
	AttemptsCap() int
	CurrentModel() *domain.ModelService
	IsExcluded(endpointID int64) bool
	NextFallback() (*domain.ModelService, bool)
	LastVerdict() Verdict
}

// =============================================================================
// state — Dispatcher's internal runtime facade; implements State + carries mutators
// =============================================================================
//
// **Doesn't hold a *RequestContext**: since dispatch is decoupled from RC,
// state only sees the typed Input; side effects (Decision / Usage /
// RoutedModel / Error) are all written into s.outcome, and the caller
// (middleware/schedule.go) translates Outcome back into RC fields.

type state struct {
	in Input

	attemptsCap int
	attempts    int
	excluded    map[int64]struct{}

	modelChain []*domain.ModelService
	curIdx     int

	lastVerdict Verdict
	decisions   []domain.Attempt

	startTime time.Time
	outcome   Outcome
}

// newState initializes runtime state from Input plus an already-resolved cap.
func newState(in Input, cap int) *state {
	return &state{
		in:          in,
		attemptsCap: cap,
		excluded:    make(map[int64]struct{}, cap),
		modelChain:  in.ModelChain,
		curIdx:      0,
		decisions:   make([]domain.Attempt, 0, cap),
		startTime:   time.Now(),
	}
}

// =============================================================================
// State interface (read-only, used by Policy)
// =============================================================================

func (s *state) Attempts() int    { return s.attempts }
func (s *state) AttemptsCap() int { return s.attemptsCap }

func (s *state) IsExcluded(endpointID int64) bool {
	_, ok := s.excluded[endpointID]
	return ok
}

func (s *state) currentModel() *domain.ModelService {
	if s.curIdx >= len(s.modelChain) {
		return nil
	}
	return s.modelChain[s.curIdx]
}

func (s *state) CurrentModel() *domain.ModelService {
	model := s.currentModel()
	if model == nil {
		return nil
	}
	copy := *model
	return &copy
}

func (s *state) NextFallback() (*domain.ModelService, bool) {
	next := s.curIdx + 1
	if next >= len(s.modelChain) {
		return nil, false
	}
	copy := *s.modelChain[next]
	return &copy, true
}

func (s *state) LastVerdict() Verdict { return s.lastVerdict }

// =============================================================================
// Internal mutators (called by Dispatcher internally)
// =============================================================================

// Exhausted reports whether the attempt count has hit the cap.
func (s *state) Exhausted() bool { return s.attempts >= s.attemptsCap }

// PickQuery builds the input for Selector.Pick (model / group / exclude).
func (s *state) PickQuery() PickQuery {
	cur := s.currentModel()
	model := ""
	if cur != nil {
		model = cur.Model
	}
	return PickQuery{
		Model:      model,
		Group:      s.in.Identity.Group,
		SessionKey: s.in.SessionKey,
		Exclude:    s.excluded,
	}
}

// CurrentModelName returns the current round's model string (input for
// CandidateSource.ListForModel); returns "" once the chain is exhausted.
func (s *state) CurrentModelName() string {
	cur := s.currentModel()
	if cur == nil {
		return ""
	}
	return cur.Model
}

// Group returns the user group (input for CandidateSource.ListForModel).
func (s *state) Group() string { return s.in.Identity.Group }

// Envelope is used by InvokerFactory.For (includes RawBytes) and by
// filterEligible.
func (s *state) Envelope() *domain.RequestEnvelope { return s.in.Envelope }

// Handlers is used by dispatcher.step — Input's request-level Handler
// lookup port (needed by both filterEligible and the dispatcher's handler
// selection).
func (s *state) Handlers() protocol.Lookup { return s.in.Handlers }

// Record logs one attempt: attempts++ / excluded / lastVerdict / decisions
// append. Outcome fields are initially filled as Unknown and corrected to
// the terminal state during finalize.
//
// **ClassUnknown is not excluded**: Unknown means a classification blind
// spot / dependency failure (e.g. a Redis reserve-store error), not "this
// endpoint is broken". Just like cooldown (scheduler.Report also doesn't
// Mark on Unknown), we don't add the endpoint to excluded here either —
// otherwise a single Redis blip would permanently remove a healthy endpoint
// from the candidate pool for subsequent fallback models: say endpoint E
// serves both models A and B, with chain [A→B]; if E hits a store error on
// A and gets excluded, then when falling back to B, E — the only
// candidate — gets filtered out by excluded → eligible is empty → a
// healthy endpoint gets reported as 503. The attempt count is still bounded
// by attemptsCap, so not excluding it doesn't cause infinite re-selection
// of the same endpoint.
func (s *state) Record(ep *domain.Endpoint, v Verdict) {
	s.attempts++
	if v.Class != ClassUnknown {
		s.excluded[ep.ID] = struct{}{}
	}
	s.lastVerdict = v

	role := domain.AttemptRolePrimary
	if s.curIdx > 0 {
		role = domain.AttemptRoleFallback
	}

	model := ""
	if cur := s.currentModel(); cur != nil {
		model = cur.Model
	}

	s.decisions = append(s.decisions, domain.Attempt{
		Index:       s.attempts,
		Model:       model,
		EndpointID:  strconv.FormatInt(ep.ID, 10),
		AttemptRole: role,
		Outcome:     domain.AttemptUnknown, // filled during finalize
		LatencyMs:   v.Latency.Milliseconds(),
		ErrorClass:  v.Class.String(),
		Started:     time.Now().Add(-v.Latency),
	})
}

// SetModel switches to a fallback model. By default it switches in chain
// order; if ms isn't in the chain (an external FallbackPolicy chose one
// outside the chain), it's appended to the end of the chain.
func (s *state) SetModel(ms *domain.ModelService) {
	for i := s.curIdx + 1; i < len(s.modelChain); i++ {
		if sameModelService(s.modelChain[i], ms) {
			s.curIdx = i
			return
		}
	}
	s.modelChain = append(s.modelChain, ms)
	s.curIdx = len(s.modelChain) - 1
}

// ApplyStream writes RoutedModel + Usage + TTFT + Outcome.Error after a
// successful Stream; everything goes into s.outcome, never touching RC
// directly (dispatch is decoupled from RC).
func (s *state) ApplyStream(rep StreamReport) {
	routed := s.currentModel()
	usage := rep.Usage
	if usage != nil && rep.TTFTMs > 0 {
		usage.Meta.TTFTMs = rep.TTFTMs
	}
	var streamErr *domain.AdapterError
	if rep.Err != nil {
		streamErr = &domain.AdapterError{
			Class:   domain.ErrTransient,
			Code:    domain.ErrCodeUpstreamError,
			Message: "stream: " + rep.Err.Error(),
		}
	}
	s.outcome = Outcome{
		Result:      OutcomeStreamed,
		Usage:       usage,
		StreamErr:   rep.Err,
		TTFTMs:      rep.TTFTMs,
		RoutedModel: routed,
		Error:       streamErr,
	}
	s.finalize()
}

func sameModelService(a, b *domain.ModelService) bool {
	if a == nil || b == nil {
		return a == b
	}
	if a.ID != 0 || b.ID != 0 {
		return a.ID == b.ID
	}
	return a.ServiceID == b.ServiceID && a.Model == b.Model
}

// SetAbort terminates the request; finalize writes the SchedulingDecision.
func (s *state) SetAbort(a Abort) {
	result := a.Result
	if result == OutcomeUnknown {
		// Policy didn't fill Result — fall back to inferring from HTTPCode
		// (kept for compatibility; ideally every Policy fills it explicitly)
		result = inferResultFromHTTPCode(a.HTTPCode)
	}
	s.outcome = Outcome{
		Result:   result,
		HTTPCode: a.HTTPCode,
		Class:    a.Class,
		Reason:   a.Reason,
	}
	s.finalize()
}

// Outcome returns the final output (including SchedulingDecision).
func (s *state) Outcome() Outcome { return s.outcome }

// =============================================================================
// finalize — at the terminal state, backfill decisions[].Outcome and write Outcome.Decision
// =============================================================================

func (s *state) finalize() {
	// attempt labels can only be backfilled if there are attempts; skip this
	// step when attempts is empty (no candidates / no eligible / AttemptCap
	// == 0) — Decision still gets filled in below regardless.
	if n := len(s.decisions); n > 0 {
		for i := 0; i < n-1; i++ {
			s.decisions[i].Outcome = domain.AttemptFallback
		}
		// a stream interruption (body forwarding failed after a 200)
		// doesn't count as success — from an audit standpoint this attempt
		// didn't fully deliver; only a cleanly finished stream is marked
		// AttemptSuccess.
		if s.outcome.Result == OutcomeStreamed && s.outcome.StreamErr == nil {
			s.decisions[n-1].Outcome = domain.AttemptSuccess
		} else {
			s.decisions[n-1].Outcome = domain.AttemptFail
		}
	}

	primary := s.in.PrimaryModel()
	model := ""
	if primary != nil {
		model = primary.Model
	}
	routedName := ""
	if s.outcome.RoutedModel != nil {
		routedName = s.outcome.RoutedModel.Model
	} else if primary != nil {
		// when routing never succeeded, fall back the audited routed name
		// to primary, to make downstream joins easier.
		routedName = primary.Model
	}

	// **Decision is always filled** (even when attempts is empty) — see the
	// contract in Outcome.Decision's comment. Scheduling failures like no
	// candidates / no eligible / attempts exhausted from the start still
	// get an audit structure, so downstream auditing / logging / metrics
	// never need to special-case a nil Decision.
	s.outcome.Decision = &domain.SchedulingDecision{
		Model:       model,
		RoutedModel: routedName,
		UserGroup:   s.in.Identity.Group,
		Attempts:    s.decisions, // may be a nil/empty slice
		DurationMs:  time.Since(s.startTime).Milliseconds(),
	}
}

// inferResultFromHTTPCode is the fallback mapping used when Policy didn't
// explicitly fill Result.
func inferResultFromHTTPCode(code int) OutcomeResult {
	switch code {
	case 400:
		return OutcomeInvalid
	case 502:
		return OutcomeTerminal
	case 503:
		return OutcomeNoEndpoint
	default:
		return OutcomeDepFail
	}
}

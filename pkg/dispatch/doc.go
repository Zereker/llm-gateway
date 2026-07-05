// Package dispatch coordinates Selector + Invoker to route a request to the
// right endpoint and execute it (including the model fallback chain, attempt
// cap, and retry policy).
//
// **Design philosophy**: the M7 middleware is a framework-thin adapter; all
// business orchestration (retry / fallback / verdict decisions) lives inside
// this package and has nothing to do with gin/echo/chi.
//
// **Four roles**:
//
//	Dispatcher  ── business orchestration + Action consumption loop (this package)
//	Selector    ── endpoint-selection abstraction (pkg/selector, currently = pkg/schedule)
//	Invoker     ── downstream-call abstraction (pkg/invoker, currently = pkg/upstream)
//	Policy×3    ── decision-point policies: AttemptCap / RetryPolicy / FallbackPolicy (this package)
//
// **Driver loop shape**:
//
//	for {
//	    switch a := dispatcher.step(...).(type) {
//	    case Continue: ...
//	    case Switch:   ...
//	    case Stream:   ...
//	    case Abort:    ...
//	    }
//	}
//
// The business truth lives in the Policy implementations. Dispatcher is
// merely the reducer for Action.
//
// See docs/architecture/03a-schedule-overview.md (scheduling + dispatch
// orchestration overview) and docs/architecture/03-endpoint-scheduling.md
// (endpoint selection / cooldown / retry) for details.
package dispatch

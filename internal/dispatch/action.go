package dispatch

import "github.com/zereker/llm-gateway/internal/domain"

// Action is the output of a Policy decision; the driver loop consumes it via
// a type switch.
//
// This is the sealed-interface pattern — the private isAction() marker
// prevents outside packages from adding new types, guaranteeing at compile
// time that the 4 cases are exhaustive.
type Action interface {
	isAction()
}

// Continue moves on to the next attempt for the current model (excluding
// already-tried endpoints).
type Continue struct{}

// Switch switches to the next fallback model, resetting the per-model
// attempt state.
type Switch struct {
	Next *domain.ModelService
}

// Stream is returned when verdict.Class == Success; the driver calls
// Result.StreamTo to stream the response.
type Stream struct{}

// Abort terminates the request; the driver translates Class/HTTPCode/Reason
// into an HTTP error response.
//
// The Result field explicitly identifies the terminal-state type — HTTPCode
// alone can't distinguish NoEndpoint (503, exhausted) from DepFail (503,
// SQL/Redis error), so Policy implementations must fill it in explicitly.
type Abort struct {
	Result   OutcomeResult // Invalid / Terminal / NoEndpoint / DepFail
	Class    Class
	HTTPCode int
	Reason   string
}

func (Continue) isAction() {}
func (Switch) isAction()   {}
func (Stream) isAction()   {}
func (Abort) isAction()    {}

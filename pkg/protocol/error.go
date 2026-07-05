package protocol

import (
	"errors"
	"fmt"
)

// PreparePhase marks which internal stage of PrepareCall failed — lets the
// dispatcher, when translating to a Verdict, distinguish "pre-call protocol
// conversion vs HTTP construction".
type PreparePhase int

const (
	// PhaseTranslate translator.TranslateRequest failed (srcBody doesn't match the
	// SourceProtocol schema). Maps to dispatch.ClassInvalid — retrying with a
	// different endpoint would fail the same way, so it should abort directly.
	PhaseTranslate PreparePhase = iota
	// PhaseQuirks vendor / model-level body rewriter failed (pkg/protocol/quirks).
	// Usually a bug in the quirks implementation or a mismatch between the upstream
	// body schema and the rule; maps to dispatch.ClassInvalid, abort directly
	// (retrying the same request would fail the same way).
	PhaseQuirks
	// PhaseBuild adapter session BuildRequest failed (vendor HTTP construction error;
	// rare, usually an invalid endpoint config such as an unparsable URL).
	// Maps to dispatch.ClassPermanent.
	PhaseBuild
)

func (p PreparePhase) String() string {
	switch p {
	case PhaseTranslate:
		return "translate"
	case PhaseQuirks:
		return "quirks"
	case PhaseBuild:
		return "build"
	default:
		return "unknown"
	}
}

// PrepareError wraps the details of a PrepareCall failure.
//
// The caller (dispatcher) uses errors.As to extract it for classification:
//
//	var pe *PrepareError
//	if errors.As(err, &pe) {
//	    switch pe.Phase {
//	    case protocol.PhaseTranslate: ... // → ClassInvalid
//	    case protocol.PhaseBuild:     ... // → ClassPermanent
//	    }
//	}
type PrepareError struct {
	Phase PreparePhase
	Err   error
}

func (e *PrepareError) Error() string {
	return fmt.Sprintf("prepare %s: %v", e.Phase, e.Err)
}

func (e *PrepareError) Unwrap() error { return e.Err }

// NewPrepareError sugar.
func NewPrepareError(phase PreparePhase, err error) *PrepareError {
	return &PrepareError{Phase: phase, Err: err}
}

// IsPrepareError sugar — use when the caller doesn't care about the specific phase.
func IsPrepareError(err error) bool {
	var pe *PrepareError
	return errors.As(err, &pe)
}

package dispatch

import (
	"github.com/zereker/llm-gateway/pkg/domain"
	"github.com/zereker/llm-gateway/pkg/protocol"
)

// Input is Dispatch's read-only input — all the request-level information
// the dispatch driver loop needs.
//
// **Design motivation**: decouples dispatch from *domain.RequestContext. RC
// is the state carrier for the middleware chain; dispatch is an
// orchestrator that only needs a pure data view of "what this request needs
// to do".
//
// **Construction point**: middleware/schedule.go extracts this from rc
// before calling Dispatch; don't construct it inside dispatch.
//
// **Field semantics**:
//
//	Envelope            ── written by M3: client body + srcProto + modality
//	Identity            ── written by M2: account / api key / group
//	ModelChain          ── written by M5: [0]=primary, followed by models
//	                        validated from X-Gateway-Fallback-Models
//	Handlers            ── written by M3: protocol.Lookup (DefaultLookup or tenant override)
//	AttemptCapOverride  ── raw value of the client's X-Gateway-Max-Attempts header; parsed by Policy
//	SessionKey          ── client's X-Gateway-Session header; session affinity (sticky routing)
type Input struct {
	Envelope           *domain.RequestEnvelope
	Identity           domain.UserIdentity
	ModelChain         []*domain.ModelService
	Handlers           protocol.Lookup
	AttemptCapOverride string
	SessionKey         string
}

// PrimaryModel returns the first entry of ModelChain; returns nil when
// empty (the dispatcher should validate ModelChain is non-empty early on).
func (in Input) PrimaryModel() *domain.ModelService {
	if len(in.ModelChain) == 0 {
		return nil
	}
	return in.ModelChain[0]
}

// SourceProtocol returns envelope.SourceProtocol; returns ProtoUnknown when
// Envelope is nil.
func (in Input) SourceProtocol() domain.Protocol {
	if in.Envelope == nil {
		return domain.ProtoUnknown
	}
	return in.Envelope.SourceProtocol
}

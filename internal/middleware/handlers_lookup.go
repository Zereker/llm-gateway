package middleware

import (
	"github.com/zereker/llm-gateway/internal/protocol"
	"github.com/zereker/llm-gateway/internal/requeststate"
)

// HandlersFrom retrieves the typed protocol.Lookup from request state.
//
// **Lives in middleware**: dispatch has been fully decoupled from
// RequestContext; only the middleware layer touches RC, so this RC ↔ typed
// lookup bridging function lives here in middleware.
//
// M3 Envelope always installs a lookup before schedule runs, so a missing one
// is a pipeline-wiring bug — we panic (caught by M9 Recover as a 500) rather
// than silently substitute an empty lookup that would fail every endpoint.
func HandlersFrom(rc *requeststate.State) protocol.Lookup {
	if rc == nil || rc.Handlers == nil {
		panic("middleware: request state has no protocol.Lookup; Envelope must run before schedule")
	}

	return rc.Handlers
}

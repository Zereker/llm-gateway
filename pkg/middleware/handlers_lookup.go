package middleware

import (
	"github.com/zereker/llm-gateway/internal/requeststate"
	"github.com/zereker/llm-gateway/pkg/protocol"
)

// HandlersFrom retrieves the typed protocol.Lookup from request state.
//
// **Lives in middleware**: dispatch has been fully decoupled from
// RequestContext; only the middleware layer touches RC, so this RC ↔ typed
// lookup bridging function lives here in middleware.
func HandlersFrom(rc *requeststate.State) protocol.Lookup {
	if rc == nil || rc.Handlers == nil {
		return protocol.DefaultLookup{}
	}
	return rc.Handlers
}

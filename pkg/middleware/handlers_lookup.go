package middleware

import (
	"github.com/zereker/llm-gateway/pkg/domain"
	"github.com/zereker/llm-gateway/pkg/protocol"
)

// HandlersFrom retrieves protocol.Lookup from the RequestContext; falls back
// to protocol.DefaultLookup when nil / of the wrong type.
//
// **Type-safe helper**: rc.Handlers is declared as any in order to avoid a
// pkg/domain → pkg/protocol → pkg/protocol → pkg/domain circular dependency;
// all consumers go through this helper instead of type-asserting directly.
//
// **Lives in middleware**: dispatch has been fully decoupled from
// RequestContext; only the middleware layer touches RC, so this RC ↔ typed
// lookup bridging function lives here in middleware.
func HandlersFrom(rc *domain.RequestContext) protocol.Lookup {
	if rc == nil {
		return protocol.DefaultLookup{}
	}
	if l, ok := rc.Handlers.(protocol.Lookup); ok && l != nil {
		return l
	}
	return protocol.DefaultLookup{}
}

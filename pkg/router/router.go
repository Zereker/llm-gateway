// Package router assembles the gin.Engine: registers the modality-split LLM
// routes plus operational endpoints.
package router

import (
	"time"

	"github.com/gin-gonic/gin"

	"github.com/zereker/llm-gateway/pkg/dispatch"
	"github.com/zereker/llm-gateway/pkg/middleware"
	"github.com/zereker/llm-gateway/pkg/protocol"
	"github.com/zereker/llm-gateway/pkg/ratelimit"
)

// Deps is the dependency set for NewEngine: it directly holds reference
// implementations for each middleware port, plus a handful of pre-middleware
// scalar parameters.
//
// **No longer wrapped in `[]middleware.XxxOption`**——that wrapping layer used
// to leak middleware's internal assembly syntax into router's public
// interface, forcing callers / tests to write `middleware.WithFoo(stubFoo)`
// just to pass a stub. Now Deps fields are the port types themselves; callers
// pass a stub / impl directly, and each modality file inside router wraps it
// into a With* option to feed the middleware factory.
//
// When adding a middleware option: extend Deps with a port-typed field; each
// modality file wraps it as needed.
type Deps struct {
	// BodyLimit / Timeout are pre-middleware scalar parameters (per-request
	// HTTP defaults).
	BodyLimit int64
	Timeout   time.Duration
	Handlers  protocol.Lookup

	// M2 Auth
	IdentityProvider middleware.IdentityProvider

	// M4 Budget
	BudgetGate middleware.BudgetGate

	// M5 ModelService
	ModelCatalog        middleware.ModelCatalog
	SubscriptionChecker middleware.SubscriptionChecker

	// M6 Limit (user-side RPM/RPS + TPM)
	RateLimitStore ratelimit.Store
	QuotaPolicies  middleware.QuotaPolicies

	// M7 Schedule
	//
	// Dispatcher is the coordinator of Selector + Invoker + Policy
	// (pkg/dispatch). The M7 middleware is now a thin adapter that just calls
	// Dispatcher.Dispatch — fallback model / retry / streaming are all
	// orchestrated inside dispatch, and router no longer knows about
	// EndpointReader / Scheduler / Sender / MaxAttempts details.
	Dispatcher *dispatch.Dispatcher

	// Response cache middleware (after M6, before M7), assembled by cmd
	// (exact / semantic / no-op). Used by the chat modality; modality files
	// skip mounting it when nil.
	Cache gin.HandlerFunc

	// EmbeddingCache is the embedding-modality-specific cache — **exact match
	// only** (semantic similarity is meaningless for embeddings and must be
	// exact). Assembled independently by cmd (exact cache is mounted whenever
	// either cache switch is on).
	EmbeddingCache gin.HandlerFunc

	// M8 Moderation
	Moderator middleware.Moderator

	// M10 Tracing
	UsageOutbox UsageOutbox
	AuditTracer AuditTracer

	// Readiness holds the dependency checks for /readyz (SQL ping / Redis
	// ping); empty = static 200. Injected by cmd during assembly; see the
	// check logic in helpers.go readyzHandler.
	Readiness []ReadinessChecker
}

// UsageOutbox / AuditTracer type aliases keep the Deps field type description
// consistent with the middleware port names, and give router test stubs a
// clear target.
type (
	UsageOutbox = middleware.UsageOutbox
	AuditTracer = middleware.AuditTracer
)

// NewEngine constructs the gin.Engine and completes all assembly.
func NewEngine(deps Deps) *gin.Engine {
	engine := gin.New()

	// Fallback recovery: M9 Recover is mounted after M1, so it doesn't cover
	// panics from BodyLimit / Timeout (which run before M1) — without this
	// layer, that class of panic only gets net/http's connection-level
	// recover, and the client sees a connection reset instead of a 500.
	// gin.Recovery's 500 doesn't carry our JSON error structure, but it's
	// only the last line of defense; the normal path never reaches it.
	engine.Use(gin.Recovery())

	registerOpsRoutes(engine, deps.Readiness)
	registerChatRoutes(engine, deps)
	registerImageRoutes(engine, deps)
	registerAudioRoutes(engine, deps)
	registerEmbeddingRoutes(engine, deps)

	return engine
}

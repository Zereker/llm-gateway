// Package router assembles the gin.Engine: it registers the modality-split LLM
// routes plus the ops endpoints.
package router

import (
	"time"

	"github.com/gin-gonic/gin"

	"github.com/zereker/llm-gateway/pkg/dispatch"
	"github.com/zereker/llm-gateway/pkg/middleware"
	"github.com/zereker/llm-gateway/pkg/ratelimit"
)

// Deps is the dependency set for NewEngine: it holds direct references to
// each middleware port's implementation plus a handful of pre-middleware
// scalar parameters.
//
// **No longer wrapped in `[]middleware.XxxOption`** — that wrapping layer
// leaked middleware's internal assembly syntax into router's public
// interface, forcing callers/tests to write `middleware.WithFoo(stubFoo)`
// just to pass a stub. Now Deps fields are the port types themselves; callers
// pass a stub/impl directly, and each modality file inside router wraps it
// into a With* option to feed the middleware factory.
//
// When adding a middleware option: extend Deps with a port-typed field; each
// modality file wraps it as needed.
type Deps struct {
	// BodyLimit / Timeout are pre-middleware scalar parameters (per-request
	// HTTP defaults).
	BodyLimit int64
	Timeout   time.Duration

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

	// Response cache (after M6, before M7); nil = no caching (middleware is a no-op).
	ResponseCache middleware.ResponseCacheStore
	CacheTTL      time.Duration

	// M8 Moderation
	Moderator middleware.Moderator

	// M10 Tracing
	UsageOutbox UsageOutbox
	AuditTracer AuditTracer

	// Readiness dependency checks for /readyz (SQL ping / Redis ping); empty =
	// static 200. Injected during cmd assembly; see helpers.go readyzHandler
	// for the check logic.
	Readiness []ReadinessChecker
}

// UsageOutbox / AuditTracer type aliases keep Deps' field type descriptions
// consistent with the middleware port names, and give router test stubs a
// clear target.
type (
	UsageOutbox = middleware.UsageOutbox
	AuditTracer = middleware.AuditTracer
)

// NewEngine builds the gin.Engine and completes all assembly.
func NewEngine(deps Deps) *gin.Engine {
	engine := gin.New()

	// Fallback recovery: M9 Recover is mounted after M1, so it doesn't cover
	// panics from BodyLimit / Timeout (which run before M1) — without this
	// layer, such panics only get net/http's connection-level recover, and
	// the client sees a connection reset instead of a 500. gin.Recovery's 500
	// doesn't have our JSON error structure, but it's only the last line of
	// defense; the normal path never reaches it.
	engine.Use(gin.Recovery())

	registerOpsRoutes(engine, deps.Readiness)
	registerChatRoutes(engine, deps)
	registerImageRoutes(engine, deps)
	registerAudioRoutes(engine, deps)
	registerEmbeddingRoutes(engine, deps)

	return engine
}

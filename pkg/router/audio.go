package router

import (
	"github.com/gin-gonic/gin"

	"github.com/zereker/llm-gateway/pkg/domain"
	"github.com/zereker/llm-gateway/pkg/middleware"
)

// registerAudioRoutes registers the audio modality routes (TTS + ASR) plus
// their dedicated middleware chain.
//
// Paths (each `.POST` carries its own full /v1 prefix):
//
//	POST /v1/audio/speech          TTS: text -> audio
//	POST /v1/audio/transcriptions  ASR: audio -> text (same language, multipart)
//	POST /v1/audio/translations    ASR: audio -> English text (multipart)
//
// v0.1: the routes are registered, but there's no audio-capable Adapter yet;
// transcriptions / translations are multipart requests, same as image.
func registerAudioRoutes(engine *gin.Engine, deps Deps) {
	pre := engine.Group("/",
		middleware.BodyLimit(deps.BodyLimit),
		middleware.Timeout(deps.Timeout),
		middleware.TraceContext(),
		// M10 Tracing is mounted **outside** Recover (its finishing logic runs
		// post-c.Next()):
		//   - any subsequent middleware abort (401/429/503) -> the onion's
		//     return leg still runs the finishing logic, so request metrics /
		//     usage events / decision audit have no blind spot
		//   - panic -> the inner Recover recovers first and writes 500,
		//     control flow returns normally, finishing logic still runs and
		//     sees the final 500 status
		// (the old version was mounted at the end of the chain, so any abort
		// was skipped entirely -> 429 storms went invisible in M10 metrics)
		middleware.Tracing(
			middleware.WithUsageOutbox(deps.UsageOutbox),
			middleware.WithTracer(deps.AuditTracer),
		),
		middleware.Recover(),
		middleware.Auth(middleware.WithIdentityProvider(deps.IdentityProvider)),
	)

	routes := []struct {
		path string
		mod  domain.Modality
	}{
		{"/v1/audio/speech", domain.ModalityTTS},
		{"/v1/audio/transcriptions", domain.ModalityASR},
		{"/v1/audio/translations", domain.ModalityASR},
	}
	for _, r := range routes {
		pre.POST(r.path,
			middleware.WithSourceProtocol(domain.ProtoOpenAI, r.mod),
			middleware.Envelope(),
			middleware.Budget(middleware.WithBudgetGate(deps.BudgetGate)),
			middleware.ModelService(
				middleware.WithModelCatalog(deps.ModelCatalog),
				middleware.WithSubscriptionChecker(deps.SubscriptionChecker),
			),
			middleware.Moderation(middleware.WithModerator(deps.Moderator)),
			middleware.Limit(
				middleware.WithLimitStore(deps.RateLimitStore),
				middleware.WithLimitPolicies(deps.QuotaPolicies),
			),
			middleware.Schedule(deps.Dispatcher),
			noopHandler,
		)
	}
}

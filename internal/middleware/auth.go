package middleware

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/baggage"

	"github.com/zereker/llm-gateway/internal/domain"
	"github.com/zereker/llm-gateway/internal/metric"
)

// IdentityProvider is the credentials → identity resolution port depended on by M2 Auth.
//
// The interface is middleware-owned; implementers (internal/repo.SQLAPIKeyProvider, etc.)
// write code for their own domain and happen to satisfy this port. The small SQL
// wiring adapter lives in internal/app/gateway/adapters.go (to avoid a
// middleware → ratelimit → repo → middleware import cycle).
//
// Implementations MUST be safe for concurrent use (called concurrently from
// multiple gin handler goroutines).
type IdentityProvider interface {
	Resolve(ctx context.Context, creds *domain.Credentials) (*domain.UserIdentity, error)
}

// AuthOption configures the Auth middleware.
//
// Follows the same interface-Option pattern as otelgin v0.68.0: cfg is built once
// at Auth() startup, the hot-path closure holds the tracer, zero per-request lookup.
type AuthOption interface {
	apply(*authConfig)
}

type authOptionFunc func(*authConfig)

func (f authOptionFunc) apply(c *authConfig) { f(c) }

// authConfig holds private configuration for the Auth middleware.
type authConfig struct {
	provider IdentityProvider
}

// WithIdentityProvider injects an IdentityProvider implementation. Required;
// Auth() panics at construction time if missing.
func WithIdentityProvider(p IdentityProvider) AuthOption {
	return authOptionFunc(func(c *authConfig) { c.provider = p })
}

// Auth is M2: extracts credentials from headers → calls IdentityProvider → writes rc.Identity.
//
// Failure behavior (all go through abort → M9 writes out JSON):
//   - Missing credentials → 401 / ErrPermanent / "missing credentials"
//   - Provider returns an error → 401 / ErrPermanent / "invalid credentials: <err>"
//
// On success:
//   - All rc.Identity fields are populated
//   - sub_account_id is written into OTel baggage; trace.CtxHandler makes all
//     subsequent log records automatically carry the sub_account_id field
func Auth(opts ...AuthOption) gin.HandlerFunc {
	cfg := authConfig{}
	for _, opt := range opts {
		opt.apply(&cfg)
	}
	if cfg.provider == nil {
		panic("middleware.Auth: WithIdentityProvider required")
	}
	tracer := otel.GetTracerProvider().Tracer(ScopeName)

	return func(c *gin.Context) {
		ctx, span := tracer.Start(c.Request.Context(), "auth.lookup")
		defer span.End()
		c.Request = c.Request.WithContext(ctx)

		creds := extractCredentials(c)
		if creds == nil {
			metric.Inc(metric.AuthTotal, "result", "missing")
			abortWithCode(c, 401, domain.ErrPermanent, domain.ErrCodeUnauthorized, "missing credentials")
			return
		}

		u, err := cfg.provider.Resolve(ctx, creds)
		if err != nil {
			// Errors fall into two classes (docs/01 §5 + §7; sentinel contract in
			// domain.ErrInvalidCredentials):
			//   invalid credentials → 401; fixed message, no err detail (does not
			//               distinguish unknown/disabled/expired, to prevent
			//               enumeration and internal info leakage)
			//   dependency failure  → fail-closed 503 + Retry-After; must not be
			//               disguised as a 401. Details go to logs only, never
			//               into the response body.
			if errors.Is(err, domain.ErrInvalidCredentials) {
				metric.Inc(metric.AuthTotal, "result", "invalid")
				abortWithCode(c, 401, domain.ErrPermanent, domain.ErrCodeUnauthorized, "invalid credentials")
				return
			}
			metric.Inc(metric.AuthTotal, "result", "error")
			slog.ErrorContext(ctx, "auth: identity lookup failed", "err", err)
			c.Header("Retry-After", "1")
			abortWithCode(c, 503, domain.ErrTransient, domain.ErrCodeDependencyUnavailable, "identity lookup unavailable")
			return
		}

		rc := GetRequestContext(c)
		rc.Identity = *u
		if member, err := baggage.NewMember("sub_account_id", u.SubAccountID); err == nil {
			if newBag, err := baggage.FromContext(ctx).SetMember(member); err == nil {
				ctx = baggage.ContextWithBaggage(ctx, newBag)
				c.Request = c.Request.WithContext(ctx)
			}
		}

		metric.Inc(metric.AuthTotal, "result", "ok")
		c.Next()
	}
}

// extractCredentials extracts Credentials from the request headers.
//
// Priority (when the same field is set by both, the latter wins):
//  1. Authorization: Bearer xxx → BearerToken (compatible with OpenAI / Anthropic SDK)
//     If X-API-Key is not set, this also fills in APIKey
//  2. X-API-Key: xxx → APIKey (overrides the value synced from Bearer above)
//
// Returns nil if there are no credentials at all.
func extractCredentials(c *gin.Context) *domain.Credentials {
	creds := &domain.Credentials{Headers: make(map[string]string, len(c.Request.Header))}
	for k, v := range c.Request.Header {
		if len(v) > 0 {
			creds.Headers[k] = v[0]
		}
	}

	if auth := c.GetHeader("Authorization"); auth != "" {
		if strings.HasPrefix(auth, "Bearer ") {
			tok := strings.TrimPrefix(auth, "Bearer ")
			creds.BearerToken = tok
			creds.APIKey = tok
		}
	}

	if k := c.GetHeader("X-API-Key"); k != "" {
		creds.APIKey = k
	}

	if creds.APIKey == "" && creds.BearerToken == "" {
		return nil
	}

	return creds
}

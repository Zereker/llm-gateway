package gateway

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	appRuntime "github.com/zereker/llm-gateway/internal/app/runtime"
	"github.com/zereker/llm-gateway/internal/config"
	"github.com/zereker/llm-gateway/internal/contentlog"
	"github.com/zereker/llm-gateway/internal/embed"
	"github.com/zereker/llm-gateway/internal/health"
	"github.com/zereker/llm-gateway/internal/middleware"
	"github.com/zereker/llm-gateway/internal/moderation"
	"github.com/zereker/llm-gateway/internal/ratelimit"
	"github.com/zereker/llm-gateway/internal/respcache"
	"github.com/zereker/llm-gateway/internal/selector"
	"github.com/zereker/llm-gateway/internal/trace"
	"github.com/zereker/llm-gateway/internal/usage"
)

// buildPicker maps cfg.Selector.Picker to a Picker instance (docs/03 §4).
//
//   - "" / "weighted_random": pure EffectiveWeight-weighted random (default)
//   - "p2c": power-of-two-choices — sample two candidates by weight, take the
//     one with fewer pending calls; returns the Inflight tracker the scheduler
//     must maintain for it
//
// An unrecognized name panics directly (fail-fast; surfaces config errors at startup).
// buildRateLimitStore constructs the M6 rate-limit counter store per
// cfg.RateLimit.Driver: redis (default; fleet-wide counters) or inmemory
// (process-local counters, single-replica/dev only — see config.RateLimitConfig).
func buildRateLimitStore(cfg config.RateLimitConfig, rdb *redis.Client) ratelimit.Store {
	switch cfg.Driver {
	case "", config.DriverRedis:
		return ratelimit.NewRedisStore(rdb)
	case config.DriverInMemory:
		return ratelimit.NewInMemoryStore()
	default:
		panic("unknown rate_limit.driver: " + cfg.Driver)
	}
}

func buildPicker(name string) (selector.Picker, *selector.Inflight) {
	switch name {
	case "", "weighted_random":
		return selector.NewWeightedRandomPicker(), nil
	case "p2c":
		inflight := selector.NewInflight()
		return selector.NewP2CPicker(inflight), inflight
	default:
		panic("unknown selector picker: " + name)
	}
}

// buildSchedulerFilters maps names from cfg.Selector.Filters to Filter instances.
//
// This mapping isn't hardcoded in the schedule package — only cmd knows what
// deps are available (redis client / store / cooldown manager). Add a case
// here when introducing a new filter type.
//
// An unrecognized name panics directly (fail-fast; surfaces config errors at startup).
func buildSchedulerFilters(names []string, store ratelimit.Store, cd selector.CooldownManager) []selector.Filter {
	out := make([]selector.Filter, 0, len(names))
	for _, n := range names {
		switch n {
		case "cooldown":
			out = append(out, selector.NewCooldownFilter(cd))
		case "limit_read":
			out = append(out, selector.NewLimitReadFilter(endpointCapacityAdapter{store: store}))
		case "weighted_random":
			// weighted_random is a Selector, not a Filter; it's configured
			// separately under cfg.Selector, so it's ignored here (kept only for
			// backward compatibility with older yaml lists).
			continue
		case "prefix_cache":
			out = append(out, selector.NewPrefixCacheFilter(0)) // 0 = default vnodes=64
		case "busy":
			out = append(out, selector.NewBusyFilter(0)) // 0 = default threshold=0.85
		default:
			panic("unknown scheduler filter: " + n)
		}
	}

	return out
}

// buildTracer constructs a trace.Tracer according to cfg.Driver.
//
//   - slog: default; local structured logging (log/slog)
//   - otel: OpenTelemetry OTLP gRPC export; registers srv.AddCloser to flush on exit
//
// An unrecognized driver panics directly (fail-fast).
func buildTracer(srv *appRuntime.Runtime, cfg config.TraceConfig) trace.Tracer {
	switch cfg.Driver {
	case "", "slog":
		return trace.NewSlogTracer(slog.Default())
	case "otel":
		tp, err := trace.NewOtelProvider(context.Background(), cfg.ServiceName, cfg.Endpoint)
		if err != nil {
			panic(fmt.Sprintf("init otel tracer: %v", err))
		}

		srv.AddCloser("otel-tracer", func() error {
			return tp.Shutdown(context.Background())
		})

		return trace.NewOtelTracer(tp)
	default:
		panic("unknown trace driver: " + cfg.Driver)
	}
}

// buildScoring constructs the Runtime Scoring stats store + scorer (docs/03 §8).
//
// Returns (nil, nil) when disabled — the scheduler then falls back to pure
// static weight, with no runtime scoring at all.
//
// **driver** (cfg.Driver, fail-fast per P5):
//   - inmemory (default): in-process EMA, accumulated independently per
//     replica; use for a single replica or when cross-replica variance is tolerable.
//   - redis: replicas share the same per-endpoint EMA for consistent scoring;
//     use for production multi-replica deployments.
//   - unknown driver → panic (surfaces config errors).
func buildScoring(cfg config.ScoringConfig, rdb *redis.Client) (selector.EndpointStatsStore, selector.Scorer) {
	if !cfg.Enabled {
		return nil, nil
	}

	decay := cfg.EMADecay
	if decay <= 0 {
		decay = 0.2
	}

	var store selector.EndpointStatsStore
	switch cfg.Driver {
	case "", config.DriverInMemory:
		store = selector.NewInMemoryStatsStore(decay)
	case config.DriverRedis:
		store = selector.NewRedisStatsStore(rdb, "llm-gateway:sched", decay, cfg.StatsTTL)
	default:
		panic("buildScoring: unknown scoring.driver " + cfg.Driver + " (want inmemory|redis)")
	}

	baselineMs := float64(200)
	if cfg.LatencyBaseline > 0 {
		baselineMs = float64(cfg.LatencyBaseline.Milliseconds())
	}

	scorer := selector.NewDefaultScorer(store, cfg.MinSamples, baselineMs)

	return store, scorer
}

// buildCacheMiddleware assembles the response cache middleware (after M6, before M7).
//
//   - semantic.enabled → semantic cache (embed the prompt + cosine hit, replaces the exact cache)
//   - enabled          → exact cache (SHA256 body hit)
//   - both off         → no-op (chain unchanged)
//
// Both are Redis-backed (shared across replicas). If the semantic cache's
// embedder isn't configured properly, startup fails fast.
func buildCacheMiddleware(cfg config.CacheConfig, rdb *redis.Client) gin.HandlerFunc {
	switch {
	case cfg.Semantic.Enabled:
		embedder := buildEmbedder(cfg.Semantic.Embedder)
		store := respcache.NewRedisSemanticStore(rdb, "llm-gateway:respcache", cfg.Semantic.MaxEntries)

		threshold := cfg.Semantic.Threshold
		if threshold <= 0 {
			threshold = 0.9
		}

		return middleware.SemanticCache(store, embedder, threshold, cfg.TTL)
	case cfg.Enabled:
		return middleware.ResponseCache(respcache.NewRedisStore(rdb, "llm-gateway:respcache"), cfg.TTL)
	default:
		return func(c *gin.Context) { c.Next() } // no-op
	}
}

// buildEmbeddingCache assembles the cache dedicated to the embedding
// modality — **exact-match only**.
//
// Embeddings are inherently deterministic (no sampling), so an exact cache is
// pure win (RAG workloads re-embedding the same batch of text repeatedly get
// good hit rates); a semantic cache makes no sense for embeddings (they must
// match exactly). So this doesn't reuse buildCacheMiddleware (which may be
// semantic) — whichever cache switch is on, embeddings get the exact cache,
// sharing the same Redis store. cacheKey already folds in modality, so chat and
// embeddings won't collide on key even with an identical body.
func buildEmbeddingCache(cfg config.CacheConfig, rdb *redis.Client) gin.HandlerFunc {
	if cfg.Enabled || cfg.Semantic.Enabled {
		return middleware.ResponseCache(respcache.NewRedisStore(rdb, "llm-gateway:respcache"), cfg.TTL)
	}

	return func(c *gin.Context) { c.Next() } // no-op
}

// buildEmbedder assembles the text embedding backend (P5 fail-fast: unknown driver panics).
func buildEmbedder(cfg config.EmbedderConfig) embed.Embedder {
	switch cfg.Driver {
	case config.DriverOpenAI:
		if cfg.APIKey == "" {
			panic("cache.semantic.embedder.driver=openai requires api_key")
		}

		return embed.NewOpenAIEmbedder(cfg.APIKey, cfg.BaseURL, cfg.Model)
	default:
		panic("cache.semantic enabled but embedder.driver unknown: " + cfg.Driver + " (want openai)")
	}
}

// buildAffinity constructs the session affinity store (Redis-backed, shared across replicas).
//
// Returns nil when disabled — the scheduler won't stick sessions. When
// enabled, a client sending the X-Gateway-Session header gets that session
// pinned to the same endpoint (soft affinity: automatically re-selects if the
// pinned endpoint is cooled down / excluded).
func buildAffinity(cfg config.SessionAffinityConfig, rdb *redis.Client) selector.AffinityStore {
	if !cfg.Enabled {
		return nil
	}

	return selector.NewRedisAffinityStore(rdb, "llm-gateway:sched", cfg.TTL)
}

// startHealthProber starts the Health Prober according to cfg (docs/03 §10).
//
// Does nothing when disabled. Also skipped when stats == nil (no point
// probing if nothing consumes the results). With cfg.RecoverCooldown, the
// prober also gets the CooldownManager so a successful probe of a cooling
// endpoint releases it early (probe-gated recovery).
func startHealthProber(srv *appRuntime.Runtime, cfg config.HealthConfig, lister health.EndpointLister, stats selector.EndpointStatsStore, cooldown selector.CooldownManager) {
	if !cfg.Enabled || stats == nil {
		return
	}

	var recover health.Recovery
	if cfg.RecoverCooldown {
		recover = cooldown
	}

	prober := health.New(health.Config{
		Source:     health.FilteredSource{Lister: lister},
		Feedback:   healthFeedbackAdapter{stats: stats},
		Recovery:   recover,
		Interval:   cfg.Interval,
		Timeout:    cfg.Timeout,
		Concurrent: cfg.Concurrent,
	})
	ctx, cancel := context.WithCancel(context.Background())
	prober.Run(ctx)
	srv.AddCloser("health-prober", func() error {
		cancel()
		prober.Stop()

		return nil
	})
}

// buildContentLogger constructs a ContentLogger according to cfg.Driver
// (returns nil = disabled).
//
//   - none/"":  returns nil, zero overhead (no hooks attached)
//   - file:     JSONL-appends to a local file; downstream fan-out (S3 / Loki /
//     Kafka content-safety / training-data replay) is left to fluent-bit /
//     vector — gateway doesn't embed a Kafka producer. See
//     docs/architecture/05-metering-billing.md §2 for the rationale.
//
// An unrecognized driver panics directly (surfaces config errors at startup).
func buildContentLogger(srv *appRuntime.Runtime, cfg config.ContentLogConfig) *contentlog.Logger {
	var pub contentlog.Publisher
	switch cfg.Driver {
	case "", config.DriverNone:
		return nil
	case config.DriverFile:
		fp, err := contentlog.NewFilePublisher(cfg.File.Path)
		if err != nil {
			panic(fmt.Sprintf("content_log: open file %s: %v", cfg.File.Path, err))
		}

		srv.AddCloser("content-log-file", fp.Close)
		pub = fp
	default:
		panic("unknown content_log driver: " + cfg.Driver)
	}

	bp := contentlog.BackpressureDropOldest
	switch cfg.Backpressure {
	case "drop_newest":
		bp = contentlog.BackpressureDropNewest
	case "block":
		bp = contentlog.BackpressureBlock
	}

	logger := contentlog.New(contentlog.Config{
		Publisher:    pub,
		SampleRate:   cfg.SampleRate,
		MaxBodyBytes: cfg.MaxBodyBytes,
		BufferSize:   cfg.BufferSize,
		Backpressure: bp,
		BlockTimeout: cfg.BlockTimeout,
	})
	srv.AddCloser("content-log-logger", func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		return logger.Close(ctx)
	})

	return logger
}

// buildBudgetGate constructs a BudgetGate according to cfg.Driver.
//
//   - alwayspass: always allow (default; dev / no billing system)
//   - inmemory:   in-process balance tracking (demo / single primary account;
//     resets to zero on restart since it's in memory)
//
// An unrecognized driver panics directly (surfaces config errors at startup).
func buildBudgetGate(cfg config.BudgetConfig) middleware.BudgetGate {
	switch cfg.Driver {
	case "", "alwayspass":
		return middleware.AlwaysPassGate{}
	case config.DriverInMemory:
		return middleware.NewInMemoryBudgetGate(cfg.DefaultBalance)
	default:
		panic("unknown budget driver: " + cfg.Driver)
	}
}

// buildModerator constructs a Moderator according to cfg.Driver. When it
// returns nil, M8 silently passes through.
//
//   - none:   nil (default; no moderation)
//   - openai: OpenAI moderation API client (requires cfg.APIKey)
//
// buildModerator assembles the guardrail chain (Chain itself is a Moderator,
// so it slots into M8 with zero changes there).
//
// Combines the driver's moderator (openai) with an optional denylist guard:
//   - 0 guards  → nil (M8 pass-through)
//   - 1 guard   → return it directly (saves the Chain overhead)
//   - ≥2 guards → Chain runs them in order
func buildModerator(cfg config.ModerationConfig) middleware.Moderator {
	var guards []moderation.NamedGuard

	switch cfg.Driver {
	case "", config.DriverNone:
		// no driver-side moderator
	case config.DriverOpenAI:
		if cfg.APIKey == "" {
			panic("moderation.driver=openai requires moderation.api_key")
		}

		guards = append(guards, moderation.NamedGuard{
			Name: config.DriverOpenAI, Guard: middleware.NewOpenAIModerator(cfg.APIKey, cfg.BaseURL),
		})
	default:
		panic("unknown moderation driver: " + cfg.Driver)
	}

	if len(cfg.Denylist.Patterns) > 0 {
		g, err := moderation.NewDenylistGuard(cfg.Denylist.Patterns, cfg.Denylist.CheckOutput)
		if err != nil {
			panic("moderation.denylist: " + err.Error()) // startup fail-fast: bad regex
		}

		guards = append(guards, moderation.NamedGuard{Name: "denylist", Guard: g})
	}

	switch len(guards) {
	case 0:
		return nil
	case 1:
		return guards[0].Guard
	default:
		return moderation.NewChain(guards...)
	}
}

// buildOutbox constructs an OutboxPublisher according to cfg.Driver.
//
// Registers close with srv:
//   - file: closes the file handle
//   - kafka: producer close is auto-registered by srv.NewKafkaProducer;
//     KafkaOutbox shares the same producer reference and doesn't add its own
//     AddCloser (to avoid closing it twice).
func buildOutbox(srv *appRuntime.Runtime, cfg config.UsageEventsConfig) (usage.OutboxPublisher, error) {
	switch cfg.Driver {
	case config.DriverFile:
		ob, err := usage.NewFileOutbox(cfg.File.Path)
		if err != nil {
			return nil, err
		}

		srv.AddCloser("file-outbox", ob.Close)

		return ob, nil
	case config.DriverKafka:
		producer, err := srv.NewKafkaProducer(cfg.Kafka.KafkaConfig)
		if err != nil {
			return nil, err
		}

		if cfg.Kafka.Async {
			ob := usage.NewAsyncKafkaOutbox(producer, cfg.Kafka.Topic, usage.AsyncOptions{
				BufferSize:  cfg.Kafka.BufferSize,
				MaxRetries:  cfg.Kafka.MaxRetries,
				BackoffBase: cfg.Kafka.BackoffBase,
				DLQTopic:    cfg.Kafka.DLQTopic,
				Logger:      slog.Default(),
			})
			srv.AddCloser("async-kafka-outbox", ob.Close)

			return ob, nil
		}

		return usage.NewKafkaOutbox(producer, cfg.Kafka.Topic), nil
	default:
		return nil, fmt.Errorf("unknown usage_events driver %q (want file|kafka)", cfg.Driver)
	}
}

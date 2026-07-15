// Package config loads the gateway's startup configuration (the root of
// gateway.yaml is the Config type).
//
// **The infra subsystem's Config types live in internal/infra** (infra.DBConfig /
// infra.KafkaConfig, etc.); internal/config references them via import — this way,
// ownership of schema evolution stays with infra when new infra is added, and
// internal/config is just the yaml orchestration layer.
//
// Distinct from internal/repo — internal/repo holds "business records" (ModelService /
// Endpoint, etc., maintained by the deployer via direct SQL); internal/config holds
// the "process's own settings" read once at startup (listen address, timeouts,
// DB connections, log paths, etc.).
//
// Example: examples/local/configs/gateway.yaml.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/zereker/llm-gateway/internal/infra"
)

// DriverNone is the shared "no-op driver" value across the config's several
// independent driver: fields (content_log, moderation, ...).
const DriverNone = "none"

// DriverFile is the shared "write locally" driver value across the config's
// several independent driver: fields (usage_events, content_log).
const DriverFile = "file"

// DriverKafka is the acknowledged or optionally asynchronous usage-event
// publisher selected by UsageEvents.Kafka.Async.
const DriverKafka = "kafka"

// DriverOpenAI is the shared "real OpenAI-compatible API" driver value across
// the config's several independent driver: fields (moderation, embedder).
// Exported so internal/app/gateway's wiring switch (which must construct the
// matching implementation) can't drift from what Validate accepts here.
const DriverOpenAI = "openai"

// DriverRedis / DriverInMemory are the shared store-backend driver values
// across the config's driver: fields (rate_limit, scoring); exported for the
// same wiring-sync reason as DriverOpenAI.
const (
	DriverRedis    = "redis"
	DriverInMemory = "inmemory"
)

// selectorFilterWeightedRandom is the default/always-available selector
// filter name, referenced both in the driver switch and in the default
// filter chain below.
const selectorFilterWeightedRandom = "weighted_random"

// validSelectorFilters is the allowlist Validate enforces for
// selector.filters. It MUST stay in sync with the switch in
// internal/app/gateway's buildSchedulerFilters — a name accepted here but
// unknown there panics at startup. That sync is guarded by a test in the
// gateway package via ValidSelectorFilters.
var validSelectorFilters = map[string]bool{
	"cooldown": true, "limit_read": true, selectorFilterWeightedRandom: true,
	"prefix_cache": true, "busy": true,
}

// ValidSelectorFilters returns the selector.filters allowlist, for the
// wiring-consistency test in internal/app/gateway (see validSelectorFilters).
func ValidSelectorFilters() []string {
	out := make([]string, 0, len(validSelectorFilters))
	for k := range validSelectorFilters {
		out = append(out, k)
	}

	sort.Strings(out)

	return out
}

// Config is the root of gateway.yaml.
//
// Fields left unset get filled with sensible defaults by ApplyDefaults; the
// user's YAML only needs to declare the fields it wants to override.
type Config struct {
	Server      ServerConfig      `yaml:"server"`
	Request     RequestConfig     `yaml:"request"`
	Paths       PathsConfig       `yaml:"paths"`
	Database    infra.DBConfig    `yaml:"database"`   // schema lives in internal/infra
	Redis       infra.RedisConfig `yaml:"redis"`      // shared by M6 RateLimit + future cache layers
	RateLimit   RateLimitConfig   `yaml:"rate_limit"` // M6 rate-limit counter store driver
	Vendors     VendorsConfig     `yaml:"vendors"`    // deployment-local vendor registrations (no rebuild)
	UsageEvents UsageEventsConfig `yaml:"usage_events"`
	Selector    SelectorConfig    `yaml:"selector"`    // M7 endpoint scheduling + cooldown config
	Budget      BudgetConfig      `yaml:"budget"`      // M4 Budget driver
	Moderation  ModerationConfig  `yaml:"moderation"`  // M8 content moderation driver
	Trace       TraceConfig       `yaml:"trace"`       // M10 Tracer driver (slog / otel)
	ContentLog  ContentLogConfig  `yaml:"content_log"` // content logging channel (docs/05 §2 + docs/08 §6)
	Health      HealthConfig      `yaml:"health"`      // Health Probing (docs/03 §10)
	Scoring     ScoringConfig     `yaml:"scoring"`     // Runtime Scoring (docs/03 §8)
	Cache       CacheConfig       `yaml:"cache"`       // response cache (after M6, before M7)

	// DataKey is the AES-256-GCM KEK (hex-encoded 32 bytes = 64 characters).
	// gateway loads it at startup via repo.SetDataKey; it's used to decrypt the
	// endpoints.auth column.
	// The deployer must use the same KEK when encrypting the endpoints.auth column.
	// In production this should come from a secret manager — **never commit the real key**.
	DataKey string `yaml:"data_key"`
}

// RateLimitConfig selects the M6 rate-limit counter store.
//
//	driver:
//	  redis    — default; counters shared fleet-wide via Redis Lua scripts
//	             (the only correct choice for multi-replica deployments)
//	  inmemory — process-local counters with identical sliding-window
//	             semantics; limits are enforced per replica. For
//	             single-replica deployments, local development, and tests.
type RateLimitConfig struct {
	Driver string `yaml:"driver"`
}

// VendorsConfig registers deployment-local vendors without a rebuild.
//
// OpenAICompatible lists extra vendor names to serve with the shared OpenAI
// Factory (same /v1/chat/completions wire shape + Bearer auth), on top of the
// built-in list in internal/protocol/openai.Aliases(). An endpoint SQL row
// can then use the name as its `vendor` with `protocol: openai`. Names that
// collide with a built-in non-OpenAI vendor (anthropic / gemini / cohere /
// bedrock / azureopenai) are rejected at startup — a config alias must never
// silently shadow a real adapter. Applied at startup only (config is not
// hot-reloaded).
type VendorsConfig struct {
	OpenAICompatible []string `yaml:"openai_compatible"`
}

// BudgetConfig selects the M4 Budget Gate implementation.
//
//	driver:
//	  alwayspass — default; always allow (dev / no billing system)
//	  inmemory   — in-process balance tracking (suited for a single-instance demo
//	               / single primary account); resets to zero on restart since it's in memory
//
// With inmemory, default_balance is the balance (USD) assigned the first time
// a new user shows up.
// 0 = safe-by-default rejection (to use inmemory budget you must explicitly SetBalance).
type BudgetConfig struct {
	Driver         string  `yaml:"driver"`
	DefaultBalance float64 `yaml:"default_balance"`
}

// TraceConfig selects the M10 Tracer implementation.
//
//	driver:
//	  slog — default; local structured logging (log/slog)
//	  otel — OpenTelemetry OTLP gRPC export
//
// With otel, endpoint is the collector address (e.g. "otel-collector:4317");
// service_name is written to the OTel resource (default "llm-gateway").
type TraceConfig struct {
	Driver      string `yaml:"driver"`
	Endpoint    string `yaml:"endpoint"`
	ServiceName string `yaml:"service_name"`
}

// ModerationConfig selects the M8 Moderator implementation.
//
//	driver:
//	  none   — default; no moderation (pass-through)
//	  openai — calls the OpenAI /v1/moderations endpoint
//
// With openai, api_key is required (inject from a secret manager in
// production); leave base_url empty to hit the official OpenAI endpoint.
type ModerationConfig struct {
	Driver  string `yaml:"driver"`
	APIKey  string `yaml:"api_key"`
	BaseURL string `yaml:"base_url"`
	// Denylist is an optional regex content guardrail (one link in the guard
	// chain); it combines with the driver's moderator into a Chain. Empty
	// patterns = this guard isn't added.
	Denylist DenylistConfig `yaml:"denylist"`
}

// DenylistConfig is a regex-based content-blocking guardrail.
type DenylistConfig struct {
	Patterns    []string `yaml:"patterns"`     // Go RE2 regexes; matching any one blocks
	CheckOutput bool     `yaml:"check_output"` // when true, also scans the response chunk by chunk
}

// ContentLogConfig configures content logging (docs/architecture/05-metering-billing.md §2, docs/08 §6).
//
//	driver:
//	  none — default; fully disabled, zero overhead
//	  file — JSONL-appends to a local file; fluent-bit / vector deliver it to
//	         downstream sinks (archival / search / post-hoc content-safety review /
//	         training-data replay)
//
// gateway deliberately does **not** embed a Kafka producer. Content Log is, in
// nature, a logging/audit channel rather than a business event; downstream
// usually fans out to multiple sinks (Loki + S3 + Kafka + ES), and having
// gateway also own that fan-out would couple the main request path's
// availability to every one of those downstreams. By making the file the sole
// output, the fluent-bit layer owns fan-out + retry, and gateway's main
// process only has to care that "writing doesn't affect the response." File
// rotation / compression / cleanup itself is handled externally by logrotate
// or the log collector (fluent-bit's tail input supports following by inode).
//
// sample_rate:        [0,1], 1.0 = sample everything, 0 = drop everything
// backpressure:       drop_oldest (default) / drop_newest / block; block requires block_timeout
// max_body_bytes:     truncates the body when >0
// buffer_size:        async queue capacity; default 1024
// file.path:          JSONL append path when driver=file; expected to be an absolute path
type ContentLogConfig struct {
	Driver       string            `yaml:"driver"`
	SampleRate   float64           `yaml:"sample_rate"`
	Backpressure string            `yaml:"backpressure"`
	BlockTimeout time.Duration     `yaml:"block_timeout"`
	MaxBodyBytes int               `yaml:"max_body_bytes"`
	BufferSize   int               `yaml:"buffer_size"`
	File         FileOutboxSection `yaml:"file"`
}

// HealthConfig configures active health probing (docs/architecture/03-endpoint-scheduling.md §10).
//
//	enabled:          default false; true starts the periodic prober
//	interval:         probe period (default 30s)
//	timeout:          per-probe timeout (default 5s)
//	concurrent:       concurrency cap (default 8)
//	recover_cooldown: default false; true lets a successful probe of a cooling
//	                  endpoint clear its cooldown early (probe-gated recovery)
type HealthConfig struct {
	Enabled         bool          `yaml:"enabled"`
	Interval        time.Duration `yaml:"interval"`
	Timeout         time.Duration `yaml:"timeout"`
	Concurrent      int           `yaml:"concurrent"`
	RecoverCooldown bool          `yaml:"recover_cooldown"`
}

// ScoringConfig configures Runtime Scoring (docs/architecture/03-endpoint-scheduling.md §8).
//
//	enabled:           default false; true lets the Scorer adjust weights
//	driver:            stats storage: inmemory (default, per-replica) | redis (shared across replicas)
//	min_samples:       fewer samples than min_samples gets a neutral factor=1 (default 5)
//	latency_baseline:  baseline used to normalize latency (default 200ms)
//	ema_decay:         EMA decay (0..1, default 0.2)
//	stats_ttl:         TTL for a single endpoint's stats under the redis driver (default 1h)
type ScoringConfig struct {
	Enabled         bool          `yaml:"enabled"`
	Driver          string        `yaml:"driver"`
	MinSamples      uint32        `yaml:"min_samples"`
	LatencyBaseline time.Duration `yaml:"latency_baseline"`
	EMADecay        float64       `yaml:"ema_decay"`
	StatsTTL        time.Duration `yaml:"stats_ttl"`
}

// ServerConfig configures the HTTP server layer.
type ServerConfig struct {
	Addr              string        `yaml:"addr"`
	ReadHeaderTimeout time.Duration `yaml:"read_header_timeout"`
	ShutdownTimeout   time.Duration `yaml:"shutdown_timeout"`
}

// RequestConfig sets global default limits for every inbound HTTP request.
//
// Historically these two fields lived under `middleware:`, which was
// misleading:
//   - `body_limit_bytes` must reject an oversized body at the router / server
//     layer, before M1 even runs;
//   - `timeout` wraps the entire M1-M10 chain with gin's TimeoutMiddleware —
//     it isn't any individual M_n's own timeout.
//
// The actual per-middleware config (M4 budget driver / M7 scheduler / M8
// moderation / M10 trace, etc.) already lives in its own top-level section;
// this section is only per-request defaults, hence the name request.
type RequestConfig struct {
	BodyLimitBytes int64         `yaml:"body_limit_bytes"`
	Timeout        time.Duration `yaml:"timeout"`
}

// PathsConfig holds file-based data paths.
//
// v0.1: apikeys / model_services / endpoints have all moved to the DB
// (maintained via direct SQL), and usage output moved to the outbox section.
// This struct is currently empty but kept around — if a resource that "must
// be a file" comes along in the future (e.g. TLS certificates), add it here.
type PathsConfig struct{}

// UsageEventsConfig selects the downstream channel M10 Tracing publishes usage
// events to.
//
// The yaml section name is `usage_events:` — named by "purpose" consistent
// with `content_log:` / `trace:`; the implementation layer uses the Outbox
// Pattern (the internal/usage.OutboxPublisher interface), but that's an internal
// pattern name and isn't exposed on the yaml surface.
//
//	driver:
//	  file  — appends local JSONL; storage durability and collection belong to the operator
//	  kafka — publishes to Kafka; kafka.async selects synchronous acknowledgement
//	          or an in-memory best-effort queue with retry and optional DLQ
//
// Field usage:
//
//	driver=file  → reads file.path
//	driver=kafka → reads kafka.{brokers, topic, async, ...}
//
// Fields belonging to the other branches are ignored.
type UsageEventsConfig struct {
	Driver string             `yaml:"driver"`
	File   FileOutboxSection  `yaml:"file"`
	Kafka  KafkaOutboxSection `yaml:"kafka"`
}

// FileOutboxSection holds the fields used when driver=file.
type FileOutboxSection struct {
	Path string `yaml:"path"` // JSONL append path; expected to be absolute, not resolved relatively
}

// KafkaOutboxSection holds the fields used when driver=kafka: embeds
// infra.KafkaConfig (brokers and other connection fields) + Topic (a business
// concern, not part of infra) + async / DLQ options.
//
// yaml `,inline` makes the embedded fields appear directly at the
// usage_events.kafka level, without nesting:
//
//	usage_events:
//	  kafka:
//	    brokers: [...]   # from infra.KafkaConfig
//	    topic: ...       # unique to this type
//	    async: true      # use AsyncKafkaOutbox (recommended for production)
//	    buffer_size: 1024
//	    max_retries: 3
//	    dlq_topic: billing.usage.recorded.v1.dlq
type KafkaOutboxSection struct {
	infra.KafkaConfig `yaml:",inline"`
	Topic             string        `yaml:"topic"`
	Async             bool          `yaml:"async"`        // true = in-memory best-effort queue; false = wait for broker acknowledgement
	BufferSize        int           `yaml:"buffer_size"`  // channel capacity in async mode; 0 = default 1024
	MaxRetries        int           `yaml:"max_retries"`  // max retries per event in async mode; 0 = default 3
	BackoffBase       time.Duration `yaml:"backoff_base"` // exponential backoff starting point; 0 = default 200ms
	DLQTopic          string        `yaml:"dlq_topic"`    // DLQ topic once retries are exhausted; empty = drop directly
}

// SelectorConfig configures M7 endpoint routing + cooldown + retry.
//
// **filters**: execution order matches array order; available values (v0.5):
//   - `cooldown`         excludes endpoints currently in cooldown
//   - `limit_read`       excludes endpoints over their quota
//   - `weighted_random`  makes the final pick (must be last; runs after the other filters)
//
// **cooldown.<class>**: cooldown duration after an endpoint fails, keyed by
// ErrorClass. 0 = no cooldown.
//
// **max_attempts**: M7's global attempt cap (including L1 same-endpoint
// internal retries); the client's X-Gateway-Max-Attempts header can override it.
type SelectorConfig struct {
	Filters []string `yaml:"filters"`
	// Picker selects the final pick strategy after filters + scoring:
	// "weighted_random" (default) or "p2c" (power-of-two-choices — sample two
	// candidates by weight, take the one with fewer pending calls).
	Picker          string                `yaml:"picker"`
	Cooldown        CooldownConfig        `yaml:"cooldown"`
	MaxAttempts     int                   `yaml:"max_attempts"`
	SessionAffinity SessionAffinityConfig `yaml:"session_affinity"`
}

// CacheConfig configures the response cache: a hit returns directly, skipping
// upstream. Redis-backed (shared across replicas). By default, only
// non-streaming + temperature=0 deterministic requests are cached; the
// client's X-Gateway-Cache header can override this.
//
// When semantic.enabled=true, it uses the **semantic cache** instead (hits by
// prompt vector similarity), replacing the exact cache.
type CacheConfig struct {
	Enabled  bool                `yaml:"enabled"`
	TTL      time.Duration       `yaml:"ttl"` // default 5m
	Semantic SemanticCacheConfig `yaml:"semantic"`
}

// SemanticCacheConfig configures the semantic cache: embed the prompt + hit
// by cosine similarity (paraphrases hit too).
type SemanticCacheConfig struct {
	Enabled    bool           `yaml:"enabled"`
	Threshold  float64        `yaml:"threshold"`   // cosine hit threshold (default 0.9)
	MaxEntries int            `yaml:"max_entries"` // entry cap per namespace (default 500)
	Embedder   EmbedderConfig `yaml:"embedder"`
}

// EmbedderConfig configures the text embedding backend.
type EmbedderConfig struct {
	Driver  string `yaml:"driver"` // openai (OpenAI-compatible /v1/embeddings)
	APIKey  string `yaml:"api_key"`
	BaseURL string `yaml:"base_url"`
	Model   string `yaml:"model"` // default text-embedding-3-small
}

// SessionAffinityConfig configures session affinity (sticky routing): a
// client's X-Gateway-Session header carries a session id, and the gateway
// pins it to the same upstream endpoint (for prefix/KV cache hits).
// Redis-backed (shared across replicas); has no effect at all when enabled=false.
type SessionAffinityConfig struct {
	Enabled bool          `yaml:"enabled"`
	TTL     time.Duration `yaml:"ttl"` // TTL for the session→endpoint mapping; default 10m
}

// CooldownConfig holds the cooldown duration for each ErrorClass.
//
// Once a cooldown mark is hit, the candidate is excluded by CooldownFilter
// within the TTL.
type CooldownConfig struct {
	Transient time.Duration `yaml:"transient"` // transient: upstream 5xx / network error / timeout, etc.
	Capacity  time.Duration `yaml:"capacity"`  // upstream 429 / quota exhausted / overloaded
	Permanent time.Duration `yaml:"permanent"` // upstream 401 / misconfigured / the endpoint itself is broken
	Invalid   time.Duration `yaml:"invalid"`   // client-side 400-class (generally not cooled down)
	Unknown   time.Duration `yaml:"unknown"`   // fallback when it can't be classified
}

// Load reads a Config in from a YAML file and applies defaults.
//
// The MySQL DSN is a connection string, and Outbox.File.Path is expected to be
// an absolute path; both are kept as literal values.
func Load(path string) (*Config, error) {
	c, err := decode(path)
	if err != nil {
		return nil, err
	}

	c.ApplyEnv()
	c.ApplyDefaults()

	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("config: validate %q: %w", path, err)
	}

	return c, nil
}

func decode(path string) (*Config, error) {
	if path == "" {
		return nil, errors.New("config: empty path")
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: read %q: %w", path, err)
	}

	var c Config
	if len(bytes.TrimSpace(data)) > 0 {
		decoder := yaml.NewDecoder(bytes.NewReader(data))
		decoder.KnownFields(true)

		if err := decoder.Decode(&c); err != nil {
			return nil, fmt.Errorf("config: parse %q: %w", path, err)
		}
	}

	return &c, nil
}

// ApplyEnv applies the production secret overrides shared with cmd/console.
// YAML remains the source for non-sensitive behavioral configuration.
func (c *Config) ApplyEnv() {
	setStringFromEnv(&c.Database.DSN, "LLM_GATEWAY_DATABASE_DSN")
	setStringFromEnv(&c.Redis.Addr, "LLM_GATEWAY_REDIS_ADDR")
	setStringFromEnv(&c.Redis.Password, "LLM_GATEWAY_REDIS_PASSWORD")
	setStringFromEnv(&c.DataKey, "LLM_GATEWAY_DATA_KEY")
	setStringFromEnv(&c.Moderation.APIKey, "LLM_GATEWAY_MODERATION_API_KEY")
	setStringFromEnv(&c.Trace.Endpoint, "LLM_GATEWAY_OTEL_ENDPOINT")

	if v := os.Getenv("LLM_GATEWAY_KAFKA_BROKERS"); v != "" {
		c.UsageEvents.Kafka.Brokers = splitNonEmpty(v)
	}
}

func setStringFromEnv(dst *string, key string) {
	if v := os.Getenv(key); v != "" {
		*dst = v
	}
}

func splitNonEmpty(value string) []string {
	parts := strings.Split(value, ",")

	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}

	return out
}

// Validate does fail-fast validation (docs/07 §5). **It runs after
// ApplyDefaults** — so it only validates constraints that "can't be defaulted
// and must be given correctly by a human":
//   - data_key is 64 hex characters (32 bytes)
//   - when trace.driver=otel, endpoint is required (slog doesn't need it);
//     driver only accepts slog|otel
//   - usage_events.driver and each driver's required sub-fields
//   - content_log.driver / backpressure constraints
//
// **Note**: database.dsn / redis.addr / cooldown have local-dev defaults
// (filled in by ApplyDefaults) and are therefore never empty — their
// misconfiguration is surfaced by real connection attempts failing fast at
// startup (OpenDB / OpenRedis ping), not by string checks here (which would be
// dead code anyway — the old dsn-empty / cooldown-all-zero checks could never
// fire after defaults ran, so they've been removed).
//
// A startup-time failure exits immediately, so a config error never reaches runtime.
func (c *Config) Validate() error {
	if c.DataKey != "" && len(c.DataKey) != 64 {
		return fmt.Errorf("data_key must be 64 hex chars (32 bytes); got %d", len(c.DataKey))
	}

	switch c.Trace.Driver {
	case "", "slog":
		// ok; endpoint is ignored
	case "otel":
		if c.Trace.Endpoint == "" {
			return errors.New("trace.driver=otel requires trace.endpoint (OTLP gRPC collector address)")
		}
	default:
		return fmt.Errorf("trace.driver=%q not supported (use slog|otel)", c.Trace.Driver)
	}

	switch c.UsageEvents.Driver {
	case "", DriverFile:
		// the file driver doesn't need the kafka section; file.path is backstopped by ApplyDefaults
	case DriverKafka:
		if len(c.UsageEvents.Kafka.Brokers) == 0 {
			return errors.New("usage_events.driver=kafka requires kafka.brokers non-empty")
		}

		if c.UsageEvents.Kafka.Topic == "" {
			return errors.New("usage_events.driver=kafka requires kafka.topic")
		}
	default:
		return fmt.Errorf("usage_events.driver=%q not supported (use file|kafka)", c.UsageEvents.Driver)
	}

	switch c.ContentLog.Driver {
	case "", DriverNone, DriverFile:
		// ok
	default:
		// kafka is deliberately no longer supported: Content Log is a
		// logging/audit channel, gateway only writes local JSONL, and
		// downstream fan-out is left to fluent-bit / vector (see docs/05 §2 + docs/07 §2).
		return fmt.Errorf("content_log.driver=%q not supported (use none|file; kafka handling has moved to fluent-bit/vector)", c.ContentLog.Driver)
	}

	switch c.Budget.Driver {
	case "", "alwayspass", DriverInMemory:
	default:
		return fmt.Errorf("budget.driver=%q not supported (use alwayspass|inmemory)", c.Budget.Driver)
	}

	switch c.RateLimit.Driver {
	case "", DriverRedis, DriverInMemory:
	default:
		return fmt.Errorf("rate_limit.driver=%q not supported (use redis|inmemory)", c.RateLimit.Driver)
	}

	seenVendor := make(map[string]bool, len(c.Vendors.OpenAICompatible))
	for _, v := range c.Vendors.OpenAICompatible {
		if v == "" || strings.ContainsAny(v, " \t") {
			return fmt.Errorf("vendors.openai_compatible contains invalid name %q (must be non-empty, no whitespace)", v)
		}

		if seenVendor[v] {
			return fmt.Errorf("vendors.openai_compatible lists %q twice", v)
		}

		seenVendor[v] = true
	}

	switch c.Moderation.Driver {
	case "", DriverNone:
	case DriverOpenAI:
		if c.Moderation.APIKey == "" {
			return errors.New("moderation.driver=openai requires moderation.api_key or LLM_GATEWAY_MODERATION_API_KEY")
		}
	default:
		return fmt.Errorf("moderation.driver=%q not supported (use none|openai)", c.Moderation.Driver)
	}

	switch c.Scoring.Driver {
	case "", DriverInMemory, DriverRedis:
	default:
		return fmt.Errorf("scoring.driver=%q not supported (use inmemory|redis)", c.Scoring.Driver)
	}

	switch c.Selector.Picker {
	case "", selectorFilterWeightedRandom, "p2c":
	default:
		return fmt.Errorf("selector.picker=%q not supported (use weighted_random|p2c)", c.Selector.Picker)
	}

	for _, filter := range c.Selector.Filters {
		if !validSelectorFilters[filter] {
			return fmt.Errorf("selector.filters contains unsupported value %q", filter)
		}
	}

	if c.Cache.Semantic.Enabled {
		if c.Cache.Semantic.Embedder.Driver != DriverOpenAI {
			return fmt.Errorf("cache.semantic.embedder.driver=%q not supported (use openai)", c.Cache.Semantic.Embedder.Driver)
		}

		if c.Cache.Semantic.Embedder.APIKey == "" {
			return errors.New("cache.semantic embedder requires api_key")
		}
	}

	if c.ContentLog.Driver == DriverFile && c.ContentLog.File.Path == "" {
		return errors.New("content_log.driver=file requires file.path non-empty")
	}

	if c.ContentLog.Backpressure == "block" && c.ContentLog.BlockTimeout <= 0 {
		return errors.New("content_log.backpressure=block requires block_timeout > 0")
	}

	switch c.ContentLog.Backpressure {
	case "", "drop_oldest", "drop_newest", "block":
	default:
		return fmt.Errorf("content_log.backpressure=%q not supported", c.ContentLog.Backpressure)
	}

	return nil
}

// ApplyDefaults fills in defaults for every field that isn't set.
//
// A caller can construct a zero-value Config and then call ApplyDefaults to
// get a "ready to use" configuration.
func (c *Config) ApplyDefaults() {
	if c.Server.Addr == "" {
		c.Server.Addr = ":8080"
	}

	if c.Server.ReadHeaderTimeout == 0 {
		c.Server.ReadHeaderTimeout = 10 * time.Second
	}

	if c.Server.ShutdownTimeout == 0 {
		c.Server.ShutdownTimeout = 30 * time.Second
	}

	if c.Request.BodyLimitBytes == 0 {
		c.Request.BodyLimitBytes = 10 << 20 // 10 MiB
	}

	if c.Request.Timeout == 0 {
		c.Request.Timeout = 60 * time.Second
	}

	if c.Database.Driver == "" {
		c.Database.Driver = infra.DriverMySQL
	}

	if c.Database.DSN == "" {
		c.Database.DSN = "root:@tcp(localhost:3306)/llm_gateway?parseTime=true&charset=utf8mb4"
	}

	if c.Redis.Addr == "" {
		c.Redis.Addr = "localhost:6379"
	}

	if c.RateLimit.Driver == "" {
		c.RateLimit.Driver = DriverRedis
	}

	if c.UsageEvents.Driver == "" {
		c.UsageEvents.Driver = DriverFile
	}

	if c.UsageEvents.File.Path == "" {
		c.UsageEvents.File.Path = "/tmp/llm-gateway-usage.log"
	}
	// UsageEvents.Kafka gets no default (must be explicitly configured when driver=kafka)

	// Scheduler defaults
	if len(c.Selector.Filters) == 0 {
		c.Selector.Filters = []string{"cooldown", "limit_read", selectorFilterWeightedRandom}
	}

	if c.Selector.MaxAttempts == 0 {
		c.Selector.MaxAttempts = 3
	}

	if c.Selector.Cooldown.Transient == 0 {
		c.Selector.Cooldown.Transient = 30 * time.Second
	}

	if c.Selector.Cooldown.Capacity == 0 {
		c.Selector.Cooldown.Capacity = 60 * time.Second
	}

	if c.Selector.Cooldown.Permanent == 0 {
		c.Selector.Cooldown.Permanent = 5 * time.Minute
	}

	if c.Selector.Cooldown.Unknown == 0 {
		c.Selector.Cooldown.Unknown = 10 * time.Second
	}
	// Cooldown.Invalid defaults to 0 (a client error shouldn't cool down the endpoint)

	// Budget defaults
	if c.Budget.Driver == "" {
		c.Budget.Driver = "alwayspass"
	}
	// Moderation defaults
	if c.Moderation.Driver == "" {
		c.Moderation.Driver = DriverNone
	}
	// Trace defaults
	if c.Trace.Driver == "" {
		c.Trace.Driver = "slog"
	}

	if c.Trace.ServiceName == "" {
		c.Trace.ServiceName = "llm-gateway"
	}
}

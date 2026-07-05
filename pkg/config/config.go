// Package config loads the gateway's startup configuration (the root of
// gateway.yaml = the Config type).
//
// **The Config types for infra subsystems live in pkg/infra** (infra.DBConfig /
// infra.KafkaConfig etc.); pkg/config references them via import — this way
// ownership of schema evolution for new infra stays concentrated in infra,
// and pkg/config is just the yaml orchestration layer.
//
// This is distinct from pkg/repo — pkg/repo holds "business records"
// (ModelService / Endpoint etc., maintained directly via SQL by the deployer);
// pkg/config is "the process's own settings" read once at startup
// (listen port, timeouts, DB connection, log paths, etc.).
//
// Example: configs/local/gateway.yaml.
package config

import (
	"errors"
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/zereker/llm-gateway/pkg/infra"
)

// Config is the root of gateway.yaml.
//
// Unset fields are filled with sensible defaults by ApplyDefaults; the user's
// YAML only needs to declare the fields it wants to override.
type Config struct {
	Server      ServerConfig      `yaml:"server"`
	Request     RequestConfig     `yaml:"request"`
	Paths       PathsConfig       `yaml:"paths"`
	Database    infra.DBConfig    `yaml:"database"` // schema lives in pkg/infra
	Redis       infra.RedisConfig `yaml:"redis"`    // shared by M6 RateLimit + future cache layer
	UsageEvents UsageEventsConfig `yaml:"usage_events"`
	Selector    SelectorConfig    `yaml:"selector"`    // M7 endpoint scheduling + cooldown config
	Budget      BudgetConfig      `yaml:"budget"`      // M4 Budget driver
	Moderation  ModerationConfig  `yaml:"moderation"`  // M8 content moderation driver
	Trace       TraceConfig       `yaml:"trace"`       // M10 Tracer driver (slog / otel)
	ContentLog  ContentLogConfig  `yaml:"content_log"` // content logging channel (docs/05 §2 + docs/08 §6)
	Health      HealthConfig      `yaml:"health"`      // Health Probing (docs/03 §10)
	Scoring     ScoringConfig     `yaml:"scoring"`     // Runtime Scoring (docs/03 §8)
	Cache       CacheConfig       `yaml:"cache"`       // response cache (after M6, before M7)

	// DataKey is the KEK for AES-256-GCM (hex-encoded 32 bytes = 64 chars).
	// Loaded at gateway startup via repo.SetDataKey; used to decrypt the
	// endpoints.auth column.
	// The deployer must use the same KEK when encrypting the endpoints.auth column.
	// In production this should be injected from a secret manager — **do not
	// commit the real key**.
	DataKey string `yaml:"data_key"`
}

// BudgetConfig selects the M4 Budget Gate implementation.
//
//	driver:
//	  alwayspass — default; always allows (development / no billing system)
//	  inmemory   — in-process balance tracking (suitable for single-instance
//	               demos / a single primary account); resets on restart
//
// With inmemory, default_balance is the balance (USD) assigned the first time
// a new user shows up.
// 0 = safe-by-default rejection (using inmemory budget requires an explicit SetBalance).
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
// production); leave base_url empty to use the official OpenAI endpoint.
type ModerationConfig struct {
	Driver  string `yaml:"driver"`
	APIKey  string `yaml:"api_key"`
	BaseURL string `yaml:"base_url"`
}

// ContentLogConfig is the content-logging configuration
// (docs/architecture/05-metering-billing.md §2, docs/08 §6).
//
//	driver:
//	  none — default; fully disabled, zero overhead
//	  file — appends JSONL to a local file, shipped downstream to various
//	         sinks by fluent-bit / vector (archival / retrieval / content
//	         safety post-review / training data feedback loop)
//
// The gateway deliberately does **not** embed a Kafka producer. A Content Log
// is a logging/audit concern in nature, not a business event; downstream is
// often multiple sinks (Loki + S3 + Kafka + ES), and having the gateway also
// own fan-out to those sinks would couple their availability into the main
// request path. Using a file as the sole output lets the fluent-bit layer own
// fan-out + retry, so the gateway process only has to care that "writing
// never affects the response." File rotation / compression / cleanup is
// handled externally by logrotate or the log collector (fluent-bit's tail
// input supports inode following).
//
// sample_rate:        [0,1], 1.0 = sample everything, 0 = drop everything
// backpressure:       drop_oldest (default) / drop_newest / block; block requires block_timeout
// max_body_bytes:     truncates the body when > 0
// buffer_size:        async queue capacity; default 1024
// file.path:          JSONL append path when driver=file; use an absolute path
type ContentLogConfig struct {
	Driver       string            `yaml:"driver"`
	SampleRate   float64           `yaml:"sample_rate"`
	Backpressure string            `yaml:"backpressure"`
	BlockTimeout time.Duration     `yaml:"block_timeout"`
	MaxBodyBytes int               `yaml:"max_body_bytes"`
	BufferSize   int               `yaml:"buffer_size"`
	File         FileOutboxSection `yaml:"file"`
}

// HealthConfig is the active health-probing configuration
// (docs/architecture/03-endpoint-scheduling.md §10).
//
//	enabled:     default false; true starts the periodic prober
//	interval:    probe period (default 30s)
//	timeout:     per-probe timeout (default 5s)
//	concurrent:  concurrency cap (default 8)
type HealthConfig struct {
	Enabled    bool          `yaml:"enabled"`
	Interval   time.Duration `yaml:"interval"`
	Timeout    time.Duration `yaml:"timeout"`
	Concurrent int           `yaml:"concurrent"`
}

// ScoringConfig is the Runtime Scoring configuration
// (docs/architecture/03-endpoint-scheduling.md §8).
//
//	enabled:           default false; true lets the Scorer adjust weights
//	driver:            stats storage: inmemory (default, per-replica) | redis (shared across replicas)
//	min_samples:       sample count < min_samples gets a neutral factor=1 (default 5)
//	latency_baseline:  baseline used to normalize latency (default 200ms)
//	ema_decay:         EMA decay (0..1, default 0.2)
//	stats_ttl:         TTL for per-endpoint stats under the redis driver (default 1h)
type ScoringConfig struct {
	Enabled         bool          `yaml:"enabled"`
	Driver          string        `yaml:"driver"`
	MinSamples      uint32        `yaml:"min_samples"`
	LatencyBaseline time.Duration `yaml:"latency_baseline"`
	EMADecay        float64       `yaml:"ema_decay"`
	StatsTTL        time.Duration `yaml:"stats_ttl"`
}

// ServerConfig is the HTTP server layer configuration.
type ServerConfig struct {
	Addr              string        `yaml:"addr"`
	ReadHeaderTimeout time.Duration `yaml:"read_header_timeout"`
	ShutdownTimeout   time.Duration `yaml:"shutdown_timeout"`
}

// RequestConfig holds global default limits for every inbound HTTP request.
//
// Historically these two fields were named `middleware:`, which is misleading:
//   - `body_limit_bytes` must reject oversized bodies at the router / server
//     layer, before M1 even runs;
//   - `timeout` wraps the entire M1-M10 chain via gin's TimeoutMiddleware, it
//     is not any single M_n's own timeout.
//
// The real per-middleware configuration (M4 budget driver / M7 scheduler /
// M8 moderation / M10 trace, etc.) is already distributed across its own
// top-level sections; this section is just the per-request defaults, hence
// the name "request".
type RequestConfig struct {
	BodyLimitBytes int64         `yaml:"body_limit_bytes"`
	Timeout        time.Duration `yaml:"timeout"`
}

// PathsConfig holds file-based data paths.
//
// v0.1: apikeys / model_services / endpoints have all moved to the DB
// (maintained directly via SQL), and usage output has moved to the outbox
// section. This struct is currently empty but kept around — if a resource
// that "must be a file" comes up in the future (e.g. TLS certificates), add
// it here.
type PathsConfig struct{}

// UsageEventsConfig selects the downstream channel M10 Tracing uses to
// output usage events.
//
// The yaml section name `usage_events:` is named by "purpose", consistent
// with `content_log:` / `trace:`; the implementation layer uses the Outbox
// Pattern (the pkg/usage.OutboxPublisher interface), but that's an internal
// pattern name and isn't exposed on the yaml surface.
//
//	driver:
//	  file            — writes local JSONL only, no downstream broadcast (dev / fallback)
//	  kafka           — writes Kafka only, no local copy (not recommended: broker down = data lost)
//	  async_kafka     — Kafka + in-memory buffer + retry + DLQ (survives brief broker blips, still loses data on prolonged outage)
//	  file_and_kafka  — **recommended for production**: file is the source of
//	                    truth (sync commit), Kafka is a best-effort async
//	                    broadcast; no data loss if the broker goes down,
//	                    an external replay tool reads the file to backfill Kafka
//
// Field usage:
//
//	driver=file               → uses file.path
//	driver=kafka|async_kafka  → uses kafka.{brokers, topic, ...}
//	driver=file_and_kafka     → uses both file.path and kafka.{brokers, topic, ...}
//
// Fields for other branches are ignored.
type UsageEventsConfig struct {
	Driver string             `yaml:"driver"`
	File   FileOutboxSection  `yaml:"file"`
	Kafka  KafkaOutboxSection `yaml:"kafka"`
}

// FileOutboxSection holds the fields used when driver=file.
type FileOutboxSection struct {
	Path string `yaml:"path"` // JSONL append path; use an absolute path, no relative resolution
}

// KafkaOutboxSection holds the fields used when driver=kafka: embeds
// infra.KafkaConfig (brokers and other connection fields) + Topic (a
// business-side concern, not part of infra) + async / DLQ options.
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
	Async             bool          `yaml:"async"`        // true = use AsyncKafkaOutbox (recommended for production)
	BufferSize        int           `yaml:"buffer_size"`  // channel capacity in async mode; 0 = default 1024
	MaxRetries        int           `yaml:"max_retries"`  // max retries per event in async mode; 0 = default 3
	BackoffBase       time.Duration `yaml:"backoff_base"` // exponential backoff starting point; 0 = default 200ms
	DLQTopic          string        `yaml:"dlq_topic"`    // DLQ topic once retries are exhausted; empty = drop
}

// SelectorConfig holds M7 endpoint routing + cooldown + retry configuration.
//
// **filters**: execution order is array order; available values (v0.5):
//   - `cooldown`         excludes endpoints currently in cooldown
//   - `limit_read`       excludes endpoints over their quota
//   - `weighted_random`  makes the final pick (must be last; runs after the other filters)
//
// **cooldown.<class>**: cooldown duration after an endpoint failure, keyed by
// ErrorClass. 0 = no cooldown.
//
// **max_attempts**: the M7 global attempt cap (includes L1 in-endpoint
// retries); the client's X-Gateway-Max-Attempts header can override it.
//
// **max_per_endpoint**: max attempts on the same endpoint (including the
// first); default 1 = no L1 retry, switch endpoints immediately on failure.
// Setting 2-3 can absorb occasional upstream network jitter.
type SelectorConfig struct {
	Filters         []string              `yaml:"filters"`
	Cooldown        CooldownConfig        `yaml:"cooldown"`
	MaxAttempts     int                   `yaml:"max_attempts"`
	MaxPerEndpoint  int                   `yaml:"max_per_endpoint"`
	SessionAffinity SessionAffinityConfig `yaml:"session_affinity"`
}

// CacheConfig is the response cache: a hit returns directly and skips the
// upstream. Redis-backed (shared across replicas).
// By default only caches non-streaming + temperature=0 deterministic
// requests; the client's X-Gateway-Cache header can override this.
type CacheConfig struct {
	Enabled bool          `yaml:"enabled"`
	TTL     time.Duration `yaml:"ttl"` // default 5m
}

// SessionAffinityConfig is session affinity (sticky routing): the client's
// X-Gateway-Session header carries a session id, and the gateway pins it to
// the same upstream endpoint (for prefix/KV cache hits). Redis-backed
// (shared across replicas); has no effect at all when enabled=false.
type SessionAffinityConfig struct {
	Enabled bool          `yaml:"enabled"`
	TTL     time.Duration `yaml:"ttl"` // TTL of the session→endpoint mapping; default 10m
}

// CooldownConfig holds the cooldown duration for each ErrorClass.
//
// Once a cooldown marker is hit, the candidate is excluded by the
// CooldownFilter within the TTL.
type CooldownConfig struct {
	Transient time.Duration `yaml:"transient"` // transient conditions: upstream 5xx / network error / timeout, etc.
	Capacity  time.Duration `yaml:"capacity"`  // upstream 429 / quota exhausted / overloaded
	Permanent time.Duration `yaml:"permanent"` // upstream 401 / misconfiguration / endpoint itself broken
	Invalid   time.Duration `yaml:"invalid"`   // client 400-class (generally not cooled down)
	Unknown   time.Duration `yaml:"unknown"`   // fallback when it can't be classified
}

// Load reads Config in from a YAML file and applies defaults.
//
// The MySQL DSN is a connection string, and Outbox.File.Path is conventionally
// an absolute path; both are kept as-is, literally.
func Load(path string) (*Config, error) {
	if path == "" {
		return nil, errors.New("config: empty path")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: read %q: %w", path, err)
	}
	var c Config
	if err := yaml.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("config: parse %q: %w", path, err)
	}
	c.ApplyDefaults()
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("config: validate %q: %w", path, err)
	}
	return &c, nil
}

// Validate performs fail-fast validation (docs/07 §5). **Runs after
// ApplyDefaults** — so it only validates constraints that "can't be defaulted,
// must be supplied correctly by a human":
//   - data_key is 64 hex chars (32 bytes)
//   - endpoint is required when trace.driver=otel (not needed for slog); driver only accepts slog|otel
//   - usage_events.driver / each driver's required sub-fields
//   - content_log.driver / backpressure constraints
//
// **Note**: database.dsn / redis.addr / cooldown have local dev defaults
// (filled in by ApplyDefaults) and are always non-empty — their
// "misconfiguration" is exposed fail-fast by the real connection at startup
// (OpenDB / OpenRedis ping), so no string checks are done here (doing so
// would be dead code anyway — the old dsn-empty / cooldown-all-zero checks
// could never trigger after defaults were applied, and have been removed).
//
// A failure at startup exits immediately, so a bad config never reaches runtime.
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
	case "", "file":
		// the file driver doesn't need the kafka section; file.path falls back via ApplyDefaults
	case "kafka", "async_kafka":
		if len(c.UsageEvents.Kafka.Brokers) == 0 {
			return errors.New("usage_events.driver=" + c.UsageEvents.Driver + " requires kafka.brokers non-empty")
		}
		if c.UsageEvents.Kafka.Topic == "" {
			return errors.New("usage_events.driver=" + c.UsageEvents.Driver + " requires kafka.topic")
		}
	case "file_and_kafka":
		// dual-write: requires both file and kafka configuration
		if c.UsageEvents.File.Path == "" {
			return errors.New("usage_events.driver=file_and_kafka requires file.path non-empty (source of truth)")
		}
		if len(c.UsageEvents.Kafka.Brokers) == 0 {
			return errors.New("usage_events.driver=file_and_kafka requires kafka.brokers non-empty")
		}
		if c.UsageEvents.Kafka.Topic == "" {
			return errors.New("usage_events.driver=file_and_kafka requires kafka.topic")
		}
	default:
		return fmt.Errorf("usage_events.driver=%q not supported (use file|kafka|async_kafka|file_and_kafka)", c.UsageEvents.Driver)
	}
	switch c.ContentLog.Driver {
	case "", "none", "file":
		// ok
	default:
		// kafka is deliberately unsupported here: Content Log is a
		// logging/audit channel, the gateway only writes local JSONL, and
		// downstream fan-out is left to fluent-bit / vector
		// (see docs/05 §2 + docs/07 §2).
		return fmt.Errorf("content_log.driver=%q not supported (use none|file; kafka handling has moved to fluent-bit/vector)", c.ContentLog.Driver)
	}
	if c.ContentLog.Driver == "file" && c.ContentLog.File.Path == "" {
		return errors.New("content_log.driver=file requires file.path non-empty")
	}
	if c.ContentLog.Backpressure == "block" && c.ContentLog.BlockTimeout <= 0 {
		return errors.New("content_log.backpressure=block requires block_timeout > 0")
	}
	return nil
}

// ApplyDefaults fills in defaults for every unset field.
//
// Callers can construct a zero-value Config and then call ApplyDefaults to
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
	if c.UsageEvents.Driver == "" {
		c.UsageEvents.Driver = "file"
	}
	if c.UsageEvents.File.Path == "" {
		c.UsageEvents.File.Path = "/tmp/llm-gateway-usage.log"
	}
	// UsageEvents.Kafka has no default (must be configured explicitly when driver=kafka)

	// Scheduler defaults
	if len(c.Selector.Filters) == 0 {
		c.Selector.Filters = []string{"cooldown", "limit_read", "weighted_random"}
	}
	if c.Selector.MaxAttempts == 0 {
		c.Selector.MaxAttempts = 3
	}
	if c.Selector.MaxPerEndpoint == 0 {
		c.Selector.MaxPerEndpoint = 1 // default: no L1 retry; must be configured explicitly to enable
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
		c.Moderation.Driver = "none"
	}
	// Trace defaults
	if c.Trace.Driver == "" {
		c.Trace.Driver = "slog"
	}
	if c.Trace.ServiceName == "" {
		c.Trace.ServiceName = "llm-gateway"
	}
}

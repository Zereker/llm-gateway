// Package config 加载 gateway 启动配置（gateway.yaml 的根 = Config 类型）。
//
// **infra 子系统的 Config 类型住在 pkg/infra**（infra.DBConfig / infra.KafkaConfig
// 等），pkg/config 通过 import 引用——这样新增 infra 时 schema 演进的所有权
// 集中在 infra 那边，pkg/config 只是 yaml 编排层。
//
// 区分于 pkg/repo —— pkg/repo 是"业务记录"（ModelService / Endpoint 等，由
// deployer 直接 SQL 维护）；pkg/config 是启动时一次性读入的"进程本身的设置"
// （监听端口、超时、DB 连接、日志路径等）。
//
// 示例：configs/local/gateway.yaml。
package config

import (
	"errors"
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/zereker/llm-gateway/pkg/infra"
)

// Config 是 gateway.yaml 的根。
//
// 缺省字段会被 ApplyDefaults 填上合理默认值；用户的 YAML 可只声明需要 override 的字段。
type Config struct {
	Server     ServerConfig      `yaml:"server"`
	Request    RequestConfig     `yaml:"request"`
	Paths      PathsConfig       `yaml:"paths"`
	Database   infra.DBConfig    `yaml:"database"` // schema 在 pkg/infra
	Redis      infra.RedisConfig `yaml:"redis"`    // M6 RateLimit + 未来 cache layer 共享
	UsageEvents UsageEventsConfig `yaml:"usage_events"`
	Selector  SelectorConfig   `yaml:"selector"`  // M7 端点调度 + cooldown 配置
	Budget     BudgetConfig      `yaml:"budget"`     // M4 Budget driver
	Moderation ModerationConfig  `yaml:"moderation"` // M8 内容审核 driver
	Trace      TraceConfig       `yaml:"trace"`      // M10 Tracer driver（slog / otel）
	ContentLog ContentLogConfig  `yaml:"content_log"` // 内容记录通道（docs/05 §2 + docs/08 §6）
	Health     HealthConfig      `yaml:"health"`      // Health Probing（docs/03 §10）
	Scoring    ScoringConfig     `yaml:"scoring"`     // Runtime Scoring（docs/03 §8）
	Cache      CacheConfig       `yaml:"cache"`       // 响应缓存（M6 之后、M7 之前）

	// DataKey 是 AES-256-GCM 的 KEK（hex-encoded 32 字节 = 64 字符）。
	// gateway 启动期调 repo.SetDataKey 装载；用于解密 endpoints.auth 列。
	// deployer 加密 endpoints.auth 列时必须用同一个 KEK。
	// 生产应从 secret manager 注入，**不要 commit 真实 key**。
	DataKey string `yaml:"data_key"`
}

// BudgetConfig M4 Budget Gate 实现选择。
//
//	driver:
//	  alwayspass — 默认；永远放行（开发 / 无付费体系）
//	  inmemory   — 进程内余额跟踪（适合单实例 demo / 单主账号）；丢内存重启清零
//
// inmemory 时 default_balance 是新 user 首次出现时分配的余额（USD）。
// 0 = safe-by-default 拒绝（要用 inmemory budget 必须显式 SetBalance）。
type BudgetConfig struct {
	Driver         string  `yaml:"driver"`
	DefaultBalance float64 `yaml:"default_balance"`
}

// TraceConfig M10 Tracer 实现选择。
//
//	driver:
//	  slog — 默认；本地结构化日志（log/slog）
//	  otel — OpenTelemetry OTLP gRPC export
//
// otel 时 endpoint 是 collector 地址（如 "otel-collector:4317"）；
// service_name 写到 OTel resource（默认 "llm-gateway"）。
type TraceConfig struct {
	Driver      string `yaml:"driver"`
	Endpoint    string `yaml:"endpoint"`
	ServiceName string `yaml:"service_name"`
}

// ModerationConfig M8 Moderator 实现选择。
//
//	driver:
//	  none   — 默认；不审核（pass-through）
//	  openai — 调 OpenAI /v1/moderations endpoint
//
// openai 时需要 api_key（生产从 secret manager 注入）；base_url 留空走 OpenAI 官方。
type ModerationConfig struct {
	Driver  string `yaml:"driver"`
	APIKey  string `yaml:"api_key"`
	BaseURL string `yaml:"base_url"`
}

// ContentLogConfig 内容记录配置（docs/architecture/05-metering-billing.md §2、docs/08 §6）。
//
//	driver:
//	  none — 默认；完全关闭，零开销
//	  file — JSONL append 写本地文件，由 fluent-bit / vector 投递到下游各 sink
//	         （归档 / 检索 / 内容安全后审 / 训练数据回流）
//
// gateway 故意**不**内嵌 Kafka producer。Content Log 在性质上是日志/审计而非业务事件，
// 下游往往是多 sink（Loki + S3 + Kafka + ES），让 gateway 同时承担投递分流会把
// 多个下游的可用性耦合到主链路。把文件作为唯一出口，fluent-bit 这一层负责扇出 +
// 重试，gateway 主进程只关心"写不影响响应"。文件本身的轮转 / 压缩 / 清理由外部
// logrotate 或日志收集器（fluent-bit tail 输入支持 inode 跟随）负责。
//
// sample_rate:        [0,1]，1.0=全采，0=全丢
// backpressure:       drop_oldest（默认） / drop_newest / block；block 必须配 block_timeout
// max_body_bytes:     >0 时截断 body
// buffer_size:        异步队列容量；默认 1024
// file.path:          driver=file 时 JSONL 追加路径；约定绝对路径
type ContentLogConfig struct {
	Driver       string            `yaml:"driver"`
	SampleRate   float64           `yaml:"sample_rate"`
	Backpressure string            `yaml:"backpressure"`
	BlockTimeout time.Duration     `yaml:"block_timeout"`
	MaxBodyBytes int               `yaml:"max_body_bytes"`
	BufferSize   int               `yaml:"buffer_size"`
	File         FileOutboxSection `yaml:"file"`
}

// HealthConfig 主动健康探测配置（docs/architecture/03-endpoint-scheduling.md §10）。
//
//	enabled:     默认 false；true 启动周期 prober
//	interval:    探测周期（默认 30s）
//	timeout:     单次 probe 超时（默认 5s）
//	concurrent:  并发上限（默认 8）
type HealthConfig struct {
	Enabled    bool          `yaml:"enabled"`
	Interval   time.Duration `yaml:"interval"`
	Timeout    time.Duration `yaml:"timeout"`
	Concurrent int           `yaml:"concurrent"`
}

// ScoringConfig Runtime Scoring 配置（docs/architecture/03-endpoint-scheduling.md §8）。
//
//	enabled:           默认 false；true 时 Scorer 调权
//	driver:            stats 存储 inmemory（默认，每副本独立）| redis（多副本共享）
//	min_samples:       样本数 < min_samples 给中性 factor=1（默认 5）
//	latency_baseline:  归一 latency 用的 baseline（默认 200ms）
//	ema_decay:         EMA 衰减（0..1，默认 0.2）
//	stats_ttl:         redis driver 下单 endpoint 统计的 TTL（默认 1h）
type ScoringConfig struct {
	Enabled         bool          `yaml:"enabled"`
	Driver          string        `yaml:"driver"`
	MinSamples      uint32        `yaml:"min_samples"`
	LatencyBaseline time.Duration `yaml:"latency_baseline"`
	EMADecay        float64       `yaml:"ema_decay"`
	StatsTTL        time.Duration `yaml:"stats_ttl"`
}

// ServerConfig HTTP 服务器层配置。
type ServerConfig struct {
	Addr              string        `yaml:"addr"`
	ReadHeaderTimeout time.Duration `yaml:"read_header_timeout"`
	ShutdownTimeout   time.Duration `yaml:"shutdown_timeout"`
}

// RequestConfig 每条入站 HTTP 请求的全局默认限制。
//
// 这两个字段历史上叫 `middleware:` 是误导：
//   - `body_limit_bytes` 在 M1 之前就要在 router / server 层拒掉超大 body；
//   - `timeout` 用 gin TimeoutMiddleware 包整条 M1-M10 链，不是任一 M_n 自己的 timeout。
//
// 真正的 per-middleware 配置（M4 budget driver / M7 scheduler / M8 moderation /
// M10 trace 等）已经各自分布在顶级段里；这一段只是 per-request 默认值，故名 request。
type RequestConfig struct {
	BodyLimitBytes int64         `yaml:"body_limit_bytes"`
	Timeout        time.Duration `yaml:"timeout"`
}

// PathsConfig 文件型数据路径。
//
// v0.1：apikeys / model_services / endpoints 全部迁到 DB（直接 SQL 维护），
// usage 输出迁到 outbox 段。本结构体当前为空但保留——未来如果有"必须是文件"
// 的资源（例如 TLS 证书），加在这里。
type PathsConfig struct{}

// UsageEventsConfig M10 Tracing 输出 usage 事件的下游通道选择。
//
// yaml 段名 `usage_events:`——按"用途"命名跟 `content_log:` / `trace:` 一致；
// 实现层用的是 Outbox Pattern（pkg/usage.OutboxPublisher 接口），但这是内部模式名，
// 不暴露到 yaml 操作面。
//
//	driver:
//	  file            — 仅写本地 JSONL，无下游广播（dev / 兜底）
//	  kafka           — 仅写 Kafka，无本地副本（不推荐：broker 挂 = 数据丢）
//	  async_kafka     — Kafka + 内存 buffer + retry + DLQ（broker 短抖动可救，长时挂仍丢）
//	  file_and_kafka  — **生产推荐**：file 是 source of truth（sync commit），
//	                    Kafka 是 best-effort 异步广播；broker 挂不丢数据，
//	                    由外部 replay 工具读 file 补发到 Kafka
//
// 字段使用：
//
//	driver=file               → 取 file.path
//	driver=kafka|async_kafka  → 取 kafka.{brokers, topic, ...}
//	driver=file_and_kafka     → 同时取 file.path 和 kafka.{brokers, topic, ...}
//
// 其余分支字段被忽略。
type UsageEventsConfig struct {
	Driver string             `yaml:"driver"`
	File   FileOutboxSection  `yaml:"file"`
	Kafka  KafkaOutboxSection `yaml:"kafka"`
}

// FileOutboxSection driver=file 时的字段。
type FileOutboxSection struct {
	Path string `yaml:"path"` // JSONL 追加路径；约定绝对路径，不做相对解析
}

// KafkaOutboxSection driver=kafka 时的字段：嵌入 infra.KafkaConfig（brokers
// 等连接字段）+ Topic（业务侧关切，不属于 infra）+ 异步 / DLQ 选项。
//
// yaml `,inline` 让嵌入字段直接出现在 usage_events.kafka 这一级，不嵌套：
//
//	usage_events:
//	  kafka:
//	    brokers: [...]   # 来自 infra.KafkaConfig
//	    topic: ...       # 本类型独有
//	    async: true      # 用 AsyncKafkaOutbox（生产推荐）
//	    buffer_size: 1024
//	    max_retries: 3
//	    dlq_topic: billing.usage.recorded.v1.dlq
type KafkaOutboxSection struct {
	infra.KafkaConfig `yaml:",inline"`
	Topic             string        `yaml:"topic"`
	Async             bool          `yaml:"async"`        // true = 用 AsyncKafkaOutbox（生产推荐）
	BufferSize        int           `yaml:"buffer_size"`  // async 模式 channel 容量；0 = 默认 1024
	MaxRetries        int           `yaml:"max_retries"`  // async 单事件最多 retry 次数；0 = 默认 3
	BackoffBase       time.Duration `yaml:"backoff_base"` // 指数退避起始；0 = 默认 200ms
	DLQTopic          string        `yaml:"dlq_topic"`    // 重试耗尽后的 DLQ topic；空 = 直接丢
}

// SelectorConfig M7 端点选路 + cooldown + 重试配置。
//
// **filters**：执行顺序就是数组顺序；可用值（v0.5）：
//   - `cooldown`         排除冷却中 endpoint
//   - `limit_read`       排除 endpoint quota 超限
//   - `weighted_random`  最终选一个（必须是最后一个；其它 filter 跑完再这个）
//
// **cooldown.<class>**：endpoint 失败后冷却时长，按 ErrorClass 分。0 = 不冷却。
//
// **max_attempts**：M7 全局尝试上限（含 L1 同 ep 内部重试）；客户端 X-Gateway-Max-Attempts 可覆盖。
//
// **max_per_endpoint**：同 endpoint 最大尝试次数（含首次）；默认 1 = 无 L1 retry，
// 失败立刻换 ep。设 2-3 可吸收上游网络偶发抖动。
type SelectorConfig struct {
	Filters         []string              `yaml:"filters"`
	Cooldown        CooldownConfig        `yaml:"cooldown"`
	MaxAttempts     int                   `yaml:"max_attempts"`
	MaxPerEndpoint  int                   `yaml:"max_per_endpoint"`
	SessionAffinity SessionAffinityConfig `yaml:"session_affinity"`
}

// CacheConfig 响应缓存：命中直接返回、跳过上游。Redis-backed（多副本共享）。
// 默认只缓存非流式 + temperature=0 的确定性请求；客户端 X-Gateway-Cache 头可覆盖。
type CacheConfig struct {
	Enabled bool          `yaml:"enabled"`
	TTL     time.Duration `yaml:"ttl"` // 默认 5m
}

// SessionAffinityConfig 会话亲和（sticky routing）：客户端 X-Gateway-Session 头带
// session id，网关把它粘到同一上游 endpoint（prefix/KV cache 命中）。Redis-backed
// （多副本共享）；enabled=false 时完全不生效。
type SessionAffinityConfig struct {
	Enabled bool          `yaml:"enabled"`
	TTL     time.Duration `yaml:"ttl"` // session→endpoint 映射 TTL；默认 10m
}

// CooldownConfig 各 ErrorClass 对应的冷却时长。
//
// 命中冷却标记后，candidate 在 TTL 内被 CooldownFilter 排除。
type CooldownConfig struct {
	Transient time.Duration `yaml:"transient"` // 上游 5xx / 网络错 / timeout 等暂时性
	Capacity  time.Duration `yaml:"capacity"`  // 上游 429 / quota 满 / overloaded
	Permanent time.Duration `yaml:"permanent"` // 上游 401 / 配置错 / endpoint 本身坏
	Invalid   time.Duration `yaml:"invalid"`   // 客户端 400-class（一般不冷却）
	Unknown   time.Duration `yaml:"unknown"`   // 分类不出来时的兜底
}

// Load 从 YAML 文件读入 Config 并应用默认值。
//
// MySQL DSN 是连接字符串、Outbox.File.Path 约定绝对路径，都按字面量保留。
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

// Validate fail-fast 校验（docs/07 §5）。**在 ApplyDefaults 之后跑**——所以这里
// 只校验"默认值填不了、必须人工给对"的约束：
//   - data_key 是 hex 64 字符（32 字节）
//   - trace.driver=otel 时 endpoint 必填（slog 不需要）；driver 只认 slog|otel
//   - usage_events.driver / 各 driver 的必填子字段
//   - content_log.driver / backpressure 约束
//
// **注意**：database.dsn / redis.addr / cooldown 有本地开发默认值（ApplyDefaults
// 填充），永远非空——它们的"配错"由启动期真实连接 fail-fast 暴露（OpenDB /
// OpenRedis ping），不在这里做字符串检查（做了也是死代码——旧版的 dsn-empty /
// cooldown-all-zero 检查在 defaults 之后永远打不中，已删）。
//
// 启动期失败即退出，不让配置错走到运行期。
func (c *Config) Validate() error {
	if c.DataKey != "" && len(c.DataKey) != 64 {
		return fmt.Errorf("data_key must be 64 hex chars (32 bytes); got %d", len(c.DataKey))
	}
	switch c.Trace.Driver {
	case "", "slog":
		// ok；endpoint 忽略
	case "otel":
		if c.Trace.Endpoint == "" {
			return errors.New("trace.driver=otel requires trace.endpoint (OTLP gRPC collector 地址)")
		}
	default:
		return fmt.Errorf("trace.driver=%q not supported (use slog|otel)", c.Trace.Driver)
	}
	switch c.UsageEvents.Driver {
	case "", "file":
		// file driver 不需要 kafka 段；file.path 由 ApplyDefaults 兜底
	case "kafka", "async_kafka":
		if len(c.UsageEvents.Kafka.Brokers) == 0 {
			return errors.New("usage_events.driver=" + c.UsageEvents.Driver + " requires kafka.brokers non-empty")
		}
		if c.UsageEvents.Kafka.Topic == "" {
			return errors.New("usage_events.driver=" + c.UsageEvents.Driver + " requires kafka.topic")
		}
	case "file_and_kafka":
		// dual-write：同时需要 file 和 kafka 配置
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
		// kafka 故意不再支持：Content Log 是日志/审计通道，gateway 只写本地 JSONL，
		// 下游分流交给 fluent-bit / vector（见 docs/05 §2 + docs/07 §2）。
		return fmt.Errorf("content_log.driver=%q not supported (use none|file; kafka 已下沉到 fluent-bit/vector)", c.ContentLog.Driver)
	}
	if c.ContentLog.Driver == "file" && c.ContentLog.File.Path == "" {
		return errors.New("content_log.driver=file requires file.path non-empty")
	}
	if c.ContentLog.Backpressure == "block" && c.ContentLog.BlockTimeout <= 0 {
		return errors.New("content_log.backpressure=block requires block_timeout > 0")
	}
	return nil
}

// ApplyDefaults 给所有未设置的字段填默认值。
//
// 调用方可以 zero-value 构造 Config 然后 ApplyDefaults，得到一份"可直接用"的配置。
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
	// UsageEvents.Kafka 不给默认（driver=kafka 时必须显式配置）

	// Scheduler defaults
	if len(c.Selector.Filters) == 0 {
		c.Selector.Filters = []string{"cooldown", "limit_read", "weighted_random"}
	}
	if c.Selector.MaxAttempts == 0 {
		c.Selector.MaxAttempts = 3
	}
	if c.Selector.MaxPerEndpoint == 0 {
		c.Selector.MaxPerEndpoint = 1 // 默认无 L1 retry；显式开启需配置
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
	// Cooldown.Invalid 默认 0（客户端错误不该冷却 endpoint）

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

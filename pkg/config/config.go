// Package config 加载两个独立服务的启动配置：
//
//   - config.go  →  Config  + Load        （gateway / 数据平面 → gateway.yaml）
//   - admin.go   →  AdminConfig + LoadAdmin（admin / 控制平面 → admin.yaml）
//
// 两个 *Config 完全独立、各自独立 yaml。
//
// **infra 子系统的 Config 类型住在 pkg/infra**（infra.DBConfig / infra.KafkaConfig
// 等），pkg/config 通过 import 引用——这样新增 infra 时 schema 演进的所有权
// 集中在 infra 那边，pkg/config 只是 yaml 编排层。
//
// 区分于 pkg/repo —— pkg/repo 是 admin 可增删改的"业务记录"
// （ModelService / Endpoint）；pkg/config 是启动时一次性读入的"进程本身的设置"
// （监听端口、超时、apikeys 文件、DB 连接、日志路径、admin token 等）。
//
// 示例：configs/local/gateway.yaml + configs/local/admin.yaml。
package config

import (
	"errors"
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/zereker-labs/ai-gateway/pkg/infra"
)

// Config 是 gateway.yaml 的根。
//
// 缺省字段会被 ApplyDefaults 填上合理默认值；用户的 YAML 可只声明需要 override 的字段。
type Config struct {
	Server     ServerConfig      `yaml:"server"`
	Middleware MiddlewareConfig  `yaml:"middleware"`
	Paths      PathsConfig       `yaml:"paths"`
	Database   infra.DBConfig    `yaml:"database"` // schema 在 pkg/infra
	Redis      infra.RedisConfig `yaml:"redis"`    // M6 RateLimit + 未来 cache layer 共享
	Outbox     OutboxConfig      `yaml:"outbox"`
	Scheduler  SchedulerConfig   `yaml:"scheduler"`  // M7 端点调度 + cooldown 配置
	Budget     BudgetConfig      `yaml:"budget"`     // M4 Budget driver
	Moderation ModerationConfig  `yaml:"moderation"` // M8 内容审核 driver
	Trace      TraceConfig       `yaml:"trace"`      // M10 Tracer driver（slog / otel）

	// DataKey 是 AES-256-GCM 的 KEK（hex-encoded 32 字节 = 64 字符）。
	// gateway 启动期调 repo.SetDataKey 装载；用于解密 endpoints.auth 列。
	// 必须跟 admin.yaml 的 data_key 字面相同（共享同一份密文存储）。
	// 生产应从 secret manager 注入，**不要 commit 真实 key**。
	DataKey string `yaml:"data_key"`
}

// BudgetConfig M4 Budget Gate 实现选择。
//
//	driver:
//	  alwayspass — 默认；永远放行（开发 / 无付费体系）
//	  inmemory   — 进程内余额跟踪（适合单实例 demo / 单租户）；丢内存重启清零
//
// inmemory 时 default_balance 是新 user 首次出现时分配的余额（USD）。
// 0 = safe-by-default 拒绝（必须 admin 显式 SetBalance 才能用）。
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
// service_name 写到 OTel resource（默认 "ai-gateway"）。
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

// ServerConfig HTTP 服务器层配置。
type ServerConfig struct {
	Addr              string        `yaml:"addr"`
	ReadHeaderTimeout time.Duration `yaml:"read_header_timeout"`
	ShutdownTimeout   time.Duration `yaml:"shutdown_timeout"`
}

// MiddlewareConfig 主 middleware 链上的全局参数。
type MiddlewareConfig struct {
	BodyLimitBytes int64         `yaml:"body_limit_bytes"`
	Timeout        time.Duration `yaml:"timeout"`
}

// PathsConfig 文件型数据路径。
//
// v0.1：apikeys / model_services / endpoints 全部迁到 DB（admin 管理），
// usage 输出迁到 outbox 段。本结构体当前为空但保留——未来如果有"必须是文件"
// 的资源（例如 TLS 证书），加在这里。
type PathsConfig struct{}

// OutboxConfig M10 Tracing 输出 usage 事件的下游通道选择。
//
//	driver: file | kafka
//	driver=file 时取 file.path；driver=kafka 时取 kafka.{brokers, topic}；
//	另一分支字段被忽略。
type OutboxConfig struct {
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
// yaml `,inline` 让嵌入字段直接出现在 outbox.kafka 这一级，不嵌套：
//
//	outbox:
//	  kafka:
//	    brokers: [...]   # 来自 infra.KafkaConfig
//	    topic: ...       # 本类型独有
//	    async: true      # 用 AsyncKafkaOutbox（生产推荐）
//	    buffer_size: 1024
//	    max_retries: 3
//	    dlq_topic: ai-gateway.usage.dlq
type KafkaOutboxSection struct {
	infra.KafkaConfig `yaml:",inline"`
	Topic             string        `yaml:"topic"`
	Async             bool          `yaml:"async"`        // true = 用 AsyncKafkaOutbox（生产推荐）
	BufferSize        int           `yaml:"buffer_size"`  // async 模式 channel 容量；0 = 默认 1024
	MaxRetries        int           `yaml:"max_retries"`  // async 单事件最多 retry 次数；0 = 默认 3
	BackoffBase       time.Duration `yaml:"backoff_base"` // 指数退避起始；0 = 默认 200ms
	DLQTopic          string        `yaml:"dlq_topic"`    // 重试耗尽后的 DLQ topic；空 = 直接丢
}

// SchedulerConfig M7 端点选路 + cooldown + 重试配置。
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
type SchedulerConfig struct {
	Filters        []string       `yaml:"filters"`
	Cooldown       CooldownConfig `yaml:"cooldown"`
	MaxAttempts    int            `yaml:"max_attempts"`
	MaxPerEndpoint int            `yaml:"max_per_endpoint"`
}

// CooldownConfig 各 ErrorClass 对应的冷却时长。
//
// 命中 admin 标记后，candidate 在 TTL 内被 CooldownFilter 排除。
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
	return &c, nil
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
	if c.Middleware.BodyLimitBytes == 0 {
		c.Middleware.BodyLimitBytes = 10 << 20 // 10 MiB
	}
	if c.Middleware.Timeout == 0 {
		c.Middleware.Timeout = 60 * time.Second
	}
	if c.Database.Driver == "" {
		c.Database.Driver = infra.DriverMySQL
	}
	if c.Database.DSN == "" {
		c.Database.DSN = "root:@tcp(localhost:3306)/ai_gateway?parseTime=true&charset=utf8mb4"
	}
	if c.Redis.Addr == "" {
		c.Redis.Addr = "localhost:6379"
	}
	if c.Outbox.Driver == "" {
		c.Outbox.Driver = "file"
	}
	if c.Outbox.File.Path == "" {
		c.Outbox.File.Path = "/tmp/ai-gateway-usage.log"
	}
	// Outbox.Kafka 不给默认（driver=kafka 时必须显式配置）

	// Scheduler defaults
	if len(c.Scheduler.Filters) == 0 {
		c.Scheduler.Filters = []string{"cooldown", "limit_read", "weighted_random"}
	}
	if c.Scheduler.MaxAttempts == 0 {
		c.Scheduler.MaxAttempts = 3
	}
	if c.Scheduler.MaxPerEndpoint == 0 {
		c.Scheduler.MaxPerEndpoint = 1 // 默认无 L1 retry；显式开启需配置
	}
	if c.Scheduler.Cooldown.Transient == 0 {
		c.Scheduler.Cooldown.Transient = 30 * time.Second
	}
	if c.Scheduler.Cooldown.Capacity == 0 {
		c.Scheduler.Cooldown.Capacity = 60 * time.Second
	}
	if c.Scheduler.Cooldown.Permanent == 0 {
		c.Scheduler.Cooldown.Permanent = 5 * time.Minute
	}
	if c.Scheduler.Cooldown.Unknown == 0 {
		c.Scheduler.Cooldown.Unknown = 10 * time.Second
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
		c.Trace.ServiceName = "ai-gateway"
	}
}

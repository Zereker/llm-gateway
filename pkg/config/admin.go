package config

import (
	"errors"
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/zereker/llm-gateway/pkg/infra"
)

// AdminConfig admin（控制平面）服务的启动配置（admin.yaml 的根）。
//
// admin 跟 gateway 完全独立——独立 binary、独立 yaml、独立端口。
// 两个服务唯一的物理共享是数据库（database 段必须跟 gateway.yaml 写一致，
// deployer 自行保证）；schema 演进由 admin 拥有，gateway 只读。
type AdminConfig struct {
	Server   ServerConfig      `yaml:"server"`
	Admin    AdminSection      `yaml:"admin"`
	Database infra.DBConfig    `yaml:"database"` // schema 在 pkg/infra
	Redis    infra.RedisConfig `yaml:"redis"`    // CDC outbox relay 推送目标
	CDC      CDCConfig         `yaml:"cdc"`      // outbox relay 行为

	// DataKey AES-256-GCM 加密 endpoints.auth 列；admin 写、gateway 读必须一致。
	// hex-encoded 32 字节 = 64 字符。生产从 secret manager 注入。
	DataKey string `yaml:"data_key"`
}

// CDCConfig 控制 admin 的 outbox relay（数据变更 → Redis 同步）。
//
//	enabled: 默认 true（admin 写数据库时同事务追加 outbox + 后台 relay 推 Redis）。
//	         false 时退化为纯 MySQL 共享方案（gateway 直读 MySQL，无 Redis 缓存）。
//	channel: Redis PUBSUB 通道；默认 llm-gateway.invalidate
//	cache_key_prefix: Redis 缓存 key 前缀；默认 llm:cache
//	cache_ttl: Redis 缓存 key TTL；默认 1h（防 Redis 数据永驻；relay 出错时缓存自然过期）
//	poll_interval: relay 轮询间隔；默认 200ms
//	batch_size: 单次轮询取多少未发送行；默认 100
type CDCConfig struct {
	Enabled        bool          `yaml:"enabled"`
	Channel        string        `yaml:"channel"`
	CacheKeyPrefix string        `yaml:"cache_key_prefix"`
	CacheTTL       time.Duration `yaml:"cache_ttl"`
	PollInterval   time.Duration `yaml:"poll_interval"`
	BatchSize      int           `yaml:"batch_size"`
}

// AdminSection admin 服务专属字段（不与 gateway 共享）。
type AdminSection struct {
	Token string `yaml:"token"` // X-Admin-Token header 校验值；空时 admin 拒所有请求
}

// LoadAdmin 加载 admin.yaml；行为与 Load 一致（应用默认值），schema 是 AdminConfig。
//
// MySQL DSN 是连接字符串，不做相对解析。
func LoadAdmin(path string) (*AdminConfig, error) {
	if path == "" {
		return nil, errors.New("config: empty path")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: read %q: %w", path, err)
	}
	var c AdminConfig
	if err := yaml.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("config: parse %q: %w", path, err)
	}
	c.ApplyDefaults()
	return &c, nil
}

// ApplyDefaults 给 AdminConfig 未设置字段填默认值。
//
// **Admin.Token 故意不给默认**——必须在 yaml 里显式配置；
// 缺失时 adminAuthMW 拒所有请求（防止误把无 token 的服务上线）。
func (c *AdminConfig) ApplyDefaults() {
	if c.Server.Addr == "" {
		c.Server.Addr = ":8081" // gateway 是 :8080，差 1 好记
	}
	if c.Server.ReadHeaderTimeout == 0 {
		c.Server.ReadHeaderTimeout = 10 * time.Second
	}
	if c.Server.ShutdownTimeout == 0 {
		c.Server.ShutdownTimeout = 30 * time.Second
	}
	if c.Database.Driver == "" {
		c.Database.Driver = infra.DriverMySQL
	}
	if c.Database.DSN == "" {
		c.Database.DSN = "root:@tcp(localhost:3306)/llm_gateway?parseTime=true&charset=utf8mb4"
	}
	// CDC defaults
	if !c.CDC.Enabled {
		c.CDC.Enabled = true // 默认开启（控制面变更准实时传 gateway）
	}
	if c.CDC.Channel == "" {
		c.CDC.Channel = "llm-gateway.invalidate"
	}
	if c.CDC.CacheKeyPrefix == "" {
		c.CDC.CacheKeyPrefix = "llm:cache"
	}
	if c.CDC.CacheTTL <= 0 {
		c.CDC.CacheTTL = 1 * time.Hour
	}
	if c.CDC.PollInterval <= 0 {
		c.CDC.PollInterval = 200 * time.Millisecond
	}
	if c.CDC.BatchSize <= 0 {
		c.CDC.BatchSize = 100
	}
}

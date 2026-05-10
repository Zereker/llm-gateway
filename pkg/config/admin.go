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
	Server   ServerConfig   `yaml:"server"`
	Admin    AdminSection   `yaml:"admin"`
	Database infra.DBConfig `yaml:"database"` // schema 在 pkg/infra

	// DataKey AES-256-GCM 加密 endpoints.auth 列；admin 写、gateway 读必须一致。
	// hex-encoded 32 字节 = 64 字符。生产从 secret manager 注入。
	DataKey string `yaml:"data_key"`
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
}

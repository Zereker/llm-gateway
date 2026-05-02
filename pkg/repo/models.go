package repo

import (
	"database/sql/driver"
	"encoding/json"
	"errors"
	"time"

	"gorm.io/datatypes"
)

// models.go 定义"业务实体"——同时被 gateway（sqlx Reader）和 admin（gorm CRUD）使用。
//
// **schema 真相在 pkg/infra/schema.sql**；这里的 `db:` / `gorm:` tag 只描述列名，
// 不开 gorm AutoMigrate（gorm 不能改 schema，所有 DDL 演进只走 SQL）。
//
// JSON 列两种处理方式：
//   - **已知结构**（EndpointCapabilities）：typed struct + 自定义 Scanner/Valuer，
//     代码端直接 `.SelfHosted` 读字段，DB 端透明 JSON 序列化
//   - **未知 / 可扩展结构**（SpecDetail / Extra）：用 datatypes.JSON（gorm 提供），
//     等同 json.RawMessage 但带 Scanner/Valuer

// ModelService 对应表 model_services。
//
// gateway 通过 sqlx Reader 读取（middleware 用），admin 通过 gorm CRUD 写入。
type ModelService struct {
	ID         int64          `db:"id"          gorm:"column:id;primaryKey;autoIncrement"`
	ServiceID  string         `db:"service_id"  gorm:"column:service_id;uniqueIndex;size:191;not null"`
	Model      string         `db:"model"       gorm:"column:model;uniqueIndex;size:191;not null"`
	UpdateTime time.Time      `db:"update_time" gorm:"column:update_time;not null;default:CURRENT_TIMESTAMP(6)"`
	SpecDetail datatypes.JSON `db:"spec_detail" gorm:"column:spec_detail"`
	Group      string         `db:"group_name"  gorm:"column:group_name;size:64;not null;default:default"`
	Tpm        int64          `db:"tpm"         gorm:"column:tpm;not null;default:0"`
	Rpm        int64          `db:"rpm"         gorm:"column:rpm;not null;default:0"`
}

// TableName 显式指定（避免 gorm 复数化推断与 schema 不一致）。
func (ModelService) TableName() string { return "model_services" }

// Endpoint 对应表 endpoints。
type Endpoint struct {
	ID           string               `db:"id"           gorm:"column:id;primaryKey;size:128"`
	Vendor       string               `db:"vendor"       gorm:"column:vendor;size:64;not null"`
	URL          string               `db:"url"          gorm:"column:url;size:512;not null"`
	APIKey       Secret               `db:"api_key"      gorm:"column:api_key;size:512;not null;default:''"`
	Group        string               `db:"group_name"   gorm:"column:group_name;size:64;not null;default:default"`
	Model        string               `db:"model"        gorm:"column:model;size:191;not null;index:idx_endpoints_model_group,priority:1"`
	Weight       int                  `db:"weight"       gorm:"column:weight;not null;default:100"`
	RPM          int64                `db:"rpm"          gorm:"column:rpm;not null;default:0"`
	TPM          int64                `db:"tpm"          gorm:"column:tpm;not null;default:0"`
	RPS          int64                `db:"rps"          gorm:"column:rps;not null;default:0"`
	Capabilities EndpointCapabilities `db:"capabilities" gorm:"column:capabilities;serializer:json"`
	Extra        datatypes.JSON       `db:"extra"        gorm:"column:extra"`
}

func (Endpoint) TableName() string { return "endpoints" }

// EndpointCapabilities 已知结构的能力标记；JSON 序列化进 endpoints.capabilities 列。
//
// gorm 用 `serializer:json` 自动 marshal；sqlx 通过本类型实现的 Scanner/Valuer 接管。
type EndpointCapabilities struct {
	SelfHosted          bool   `json:"self_hosted,omitempty"`
	KVMetricEndpoint    string `json:"kv_metric_endpoint,omitempty"`
	HealthProbeEndpoint string `json:"health_probe_endpoint,omitempty"`
	PrefixCacheEnabled  bool   `json:"prefix_cache_enabled,omitempty"`
}

// Scan 实现 sql.Scanner：从 DB JSON 字节反序列化。
func (c *EndpointCapabilities) Scan(value any) error {
	if value == nil {
		*c = EndpointCapabilities{}
		return nil
	}
	var b []byte
	switch v := value.(type) {
	case []byte:
		b = v
	case string:
		b = []byte(v)
	default:
		return errors.New("EndpointCapabilities: unsupported scan source")
	}
	if len(b) == 0 {
		*c = EndpointCapabilities{}
		return nil
	}
	return json.Unmarshal(b, c)
}

// Value 实现 driver.Valuer：marshal 到 JSON；零值写 NULL。
func (c EndpointCapabilities) Value() (driver.Value, error) {
	if (c == EndpointCapabilities{}) {
		return nil, nil
	}
	return json.Marshal(c)
}

// EndpointForm 由 Capabilities 派生（保留原 domain 的 helper）。
type EndpointForm int

const (
	FormVendor     EndpointForm = iota // 第三方厂商（OpenAI、Anthropic、AWS Bedrock 等）
	FormSelfHosted                     // 自部署（vLLM、Ollama、SGLang 等内部可观测的部署）
)

func (f EndpointForm) String() string {
	if f == FormSelfHosted {
		return "self_hosted"
	}
	return "vendor"
}

// Form 派生方法。
func (e *Endpoint) Form() EndpointForm {
	if e.Capabilities.SelfHosted {
		return FormSelfHosted
	}
	return FormVendor
}

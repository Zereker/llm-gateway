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
//
// 多租户：TenantID 是逻辑租户分区；唯一约束是 (tenant_id, service_id) 和 (tenant_id, model)。
type ModelService struct {
	ID         int64          `db:"id"          gorm:"column:id;primaryKey;autoIncrement"`
	TenantID   string         `db:"tenant_id"   gorm:"column:tenant_id;size:64;not null;default:default;uniqueIndex:uk_tenant_service_id;uniqueIndex:uk_tenant_model"`
	ServiceID  string         `db:"service_id"  gorm:"column:service_id;size:191;not null;uniqueIndex:uk_tenant_service_id"`
	Model      string         `db:"model"       gorm:"column:model;size:191;not null;uniqueIndex:uk_tenant_model"`
	UpdateTime time.Time      `db:"update_time" gorm:"column:update_time;not null;default:CURRENT_TIMESTAMP(6)"`
	SpecDetail datatypes.JSON `db:"spec_detail" gorm:"column:spec_detail"`
	Group      string         `db:"group_name"  gorm:"column:group_name;size:64;not null;default:default"`
	Tpm        int64          `db:"tpm"         gorm:"column:tpm;not null;default:0"`
	Rpm        int64          `db:"rpm"         gorm:"column:rpm;not null;default:0"`
}

// TableName 显式指定（避免 gorm 复数化推断与 schema 不一致）。
func (ModelService) TableName() string { return "model_services" }

// Endpoint 对应表 endpoints。
//
// 多租户：复合主键 (tenant_id, id)；不同租户可以重名 endpoint id。
type Endpoint struct {
	TenantID     string               `db:"tenant_id"    gorm:"column:tenant_id;size:64;not null;default:default;primaryKey;index:idx_endpoints_tenant_model_group,priority:1"`
	ID           string               `db:"id"           gorm:"column:id;primaryKey;size:128"`
	Vendor       string               `db:"vendor"       gorm:"column:vendor;size:64;not null"`
	URL          string               `db:"url"          gorm:"column:url;size:512;not null"`
	APIKey       Secret               `db:"api_key"      gorm:"column:api_key;size:512;not null;default:''"`
	Group        string               `db:"group_name"   gorm:"column:group_name;size:64;not null;default:default"`
	Model        string               `db:"model"        gorm:"column:model;size:191;not null;index:idx_endpoints_tenant_model_group,priority:2"`
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

// APIKey 对应表 api_keys。
//
// 多租户：tenant_id 是逻辑分区；api_key 全局唯一（gateway 拿到 key 没有 tenant
// 上下文，要从 key 反查 tenant），(tenant_id, api_key_id) 联合唯一。
//
// **APIKey.Key 在 DB 层用 plain string**（v0.1 不 hash）；通过 admin DTO 边界
// 才看到明文（创建时返回一次），后续 GET 只显 api_key_id。
//
// 注意：本类型字段名 Key 而不是 APIKey/Value，避免跟 APIKey 类型自身重名。
type APIKey struct {
	ID           int64      `db:"id"            gorm:"column:id;primaryKey;autoIncrement"`
	TenantID     string     `db:"tenant_id"     gorm:"column:tenant_id;size:64;not null;default:default;uniqueIndex:uk_tenant_api_key_id;index:idx_tenant_user_id,priority:1"`
	Key          string     `db:"api_key"       gorm:"column:api_key;size:255;not null;uniqueIndex:uk_api_key"`
	APIKeyID     string     `db:"api_key_id"    gorm:"column:api_key_id;size:64;not null;uniqueIndex:uk_tenant_api_key_id"`
	UserID       string     `db:"user_id"       gorm:"column:user_id;size:64;not null;index:idx_tenant_user_id,priority:2"`
	Group        string     `db:"group_name"    gorm:"column:group_name;size:64;not null;default:default"`
	ExternalUser bool       `db:"external_user" gorm:"column:external_user;not null;default:false"`
	Enabled      bool       `db:"enabled"       gorm:"column:enabled;not null;default:true;index:idx_enabled_expires,priority:1"`
	ExpiresAt    *time.Time `db:"expires_at"    gorm:"column:expires_at;index:idx_enabled_expires,priority:2"`
	CreatedAt    time.Time  `db:"created_at"    gorm:"column:created_at;not null;default:CURRENT_TIMESTAMP(6)"`
}

func (APIKey) TableName() string { return "api_keys" }

// ToUserIdentity 把 DB 行映射成 M2 Auth 给后续 middleware 用的 UserIdentity。
func (a APIKey) ToUserIdentity() UserIdentity {
	return UserIdentity{
		TenantID:     a.TenantID,
		UserID:       a.UserID,
		APIKeyID:     a.APIKeyID,
		Group:        a.Group,
		ExternalUser: a.ExternalUser,
	}
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

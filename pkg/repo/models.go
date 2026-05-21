package repo

import (
	"database/sql/driver"
	"encoding/json"
	"errors"
	"time"

	"gorm.io/datatypes"
)

// models.go 定义"业务实体"——gateway（sqlx Reader）使用；写入由 deployer 走 SQL 维护。
//
// **schema 真相在 pkg/infra/schema.sql**；这里的 `db:` / `gorm:` tag 只描述列名，
// 不开 gorm AutoMigrate（gorm 不能改 schema，所有 DDL 演进只走 SQL）。
//
// JSON 列两种处理方式：
//   - **已知结构**（EndpointCapabilities / AuthConfig / RoutingConfig / QuotaConfig）：
//     typed struct + 自定义 Scanner/Valuer，调用方直接读字段，DB 端透明 JSON 序列化
//   - **未知 / 可扩展结构**（rule_json / Extra）：用 datatypes.JSON
//
// **三件套审计字段**：
//   - CreatedAt / UpdatedAt / DeletedAt 软删除指针
//   - 软删后同 UNIQUE 键不能直接复用，需 hard-delete

// =============================================================================
// Account：主账号 pin / 计费主体元信息
// =============================================================================

// Account 对应表 accounts，业务语义是主账号 / 计费主体。
//
// **pin 直接做主键**：业务键 = 身份键，不引入 BIGINT 中转。其它表的 account_id
// VARCHAR(64) 列就是这个 pin，FK → accounts.pin。
//
// QuotaPolicyID NULL = 主账号层不限流（M6 跳过主账号层检查）。
type Account struct {
	Pin           string `db:"pin"               gorm:"column:pin;size:64;primaryKey"`
	Name          string `db:"name"              gorm:"column:name;size:128;not null"`
	Enabled       bool   `db:"enabled"           gorm:"column:enabled;not null;default:true"`
	QuotaPolicyID *int64 `db:"quota_policy_id"   gorm:"column:quota_policy_id"`

	CreatedAt time.Time  `db:"created_at" gorm:"column:created_at;not null;default:CURRENT_TIMESTAMP(6)"`
	UpdatedAt time.Time  `db:"updated_at" gorm:"column:updated_at;not null;default:CURRENT_TIMESTAMP(6)"`
	DeletedAt *time.Time `db:"deleted_at" gorm:"column:deleted_at"`
}

func (Account) TableName() string { return "accounts" }

// =============================================================================
// QuotaPolicy：限流策略库（被主账号 / api_keys 引用，N:M 共享）
// =============================================================================

// QuotaPolicy 对应表 quota_policies。
//
// rule_json 形态：
//
//	{
//	  "default":   {"rpm":60, "tpm":100000, "rps":null, "concurrent_requests":null},
//	  "per_model": {"gpt-4o":{"rpm":10}, "gpt-4o-mini":{"rpm":100}}
//	}
//
// gateway 不解析 rule_json；M6 RateLimit 是唯一消费者：
// 先 per_model[currentModel]，没有 fallback default，都没就该层不限。
//
// 可改（不像 pricing_versions 是 append-only；改 quota 不影响计费历史）。
type QuotaPolicy struct {
	ID          int64          `db:"id"          gorm:"column:id;primaryKey;autoIncrement"`
	Name        string         `db:"name"        gorm:"column:name;size:64;not null;uniqueIndex:uk_name"`
	Description string         `db:"description" gorm:"column:description;size:512;not null;default:''"`
	RuleJSON    datatypes.JSON `db:"rule_json"   gorm:"column:rule_json;type:json;not null"`
	Enabled     bool           `db:"enabled"     gorm:"column:enabled;not null;default:true"`

	CreatedAt time.Time  `db:"created_at" gorm:"column:created_at;not null;default:CURRENT_TIMESTAMP(6)"`
	UpdatedAt time.Time  `db:"updated_at" gorm:"column:updated_at;not null;default:CURRENT_TIMESTAMP(6)"`
	DeletedAt *time.Time `db:"deleted_at" gorm:"column:deleted_at"`
}

func (QuotaPolicy) TableName() string { return "quota_policies" }

// =============================================================================
// ModelService：全局模型 catalog
// =============================================================================

// ModelService 对应表 model_services。
//
// **v0.3 改动**：删 account_id（改为全局 catalog）/ group_name / spec_detail。
// 模型可见性走 account_model_subscriptions；group 是 endpoint 维度。
type ModelService struct {
	ID        int64  `db:"id"          gorm:"column:id;primaryKey;autoIncrement"`
	ServiceID string `db:"service_id"  gorm:"column:service_id;size:191;not null;uniqueIndex:uk_service_id"`
	Model     string `db:"model"       gorm:"column:model;size:191;not null;uniqueIndex:uk_model"`

	CreatedAt time.Time  `db:"created_at" gorm:"column:created_at;not null;default:CURRENT_TIMESTAMP(6)"`
	UpdatedAt time.Time  `db:"updated_at" gorm:"column:updated_at;not null;default:CURRENT_TIMESTAMP(6)"`
	DeletedAt *time.Time `db:"deleted_at" gorm:"column:deleted_at;index:idx_deleted_at"`
}

func (ModelService) TableName() string { return "model_services" }

// =============================================================================
// AccountModelSubscription：主账号 × model 的可见性 N:M
// =============================================================================

// AccountModelSubscription 对应表 account_model_subscriptions。
//
// M5 在确认 model 在 catalog 后，按 (主账号 pin, model_service_id) 查这张表；
// 没找到 → 403 "model not subscribed"。
type AccountModelSubscription struct {
	ID             int64  `db:"id"                gorm:"column:id;primaryKey;autoIncrement"`
	AccountID      string `db:"account_id"         gorm:"column:account_id;size:64;not null;uniqueIndex:uk_account_model;index:idx_account"`
	ModelServiceID int64  `db:"model_service_id"  gorm:"column:model_service_id;not null;uniqueIndex:uk_account_model"`
	Enabled        bool   `db:"enabled"           gorm:"column:enabled;not null;default:true"`

	CreatedAt time.Time  `db:"created_at" gorm:"column:created_at;not null;default:CURRENT_TIMESTAMP(6)"`
	UpdatedAt time.Time  `db:"updated_at" gorm:"column:updated_at;not null;default:CURRENT_TIMESTAMP(6)"`
	DeletedAt *time.Time `db:"deleted_at" gorm:"column:deleted_at"`
}

func (AccountModelSubscription) TableName() string { return "account_model_subscriptions" }

// =============================================================================
// Endpoint：全局上游接入点
// =============================================================================

// Endpoint 对应表 endpoints。
//
// **v0.3 改动**：删 account_id（改为全局；BYOK 等真要做时加 nullable account_id）。
//
// 核心列只放调度选路 hot path 用得到的；vendor-specific 全进 typed JSON。
type Endpoint struct {
	ID       int64  `db:"id"         gorm:"column:id;primaryKey;autoIncrement"`
	Name     string `db:"name"       gorm:"column:name;size:128;not null;uniqueIndex:uk_name"`
	Vendor   string `db:"vendor"     gorm:"column:vendor;size:32;not null"`
	Protocol string `db:"protocol"   gorm:"column:protocol;size:32;not null"` // domain.Protocol.String()——mappers 来回转
	Model    string `db:"model"      gorm:"column:model;size:191;not null;index:idx_model_group,priority:1"`
	Group    string `db:"group_name" gorm:"column:group_name;size:64;not null;default:default;index:idx_model_group,priority:2"`
	Weight   uint32 `db:"weight"     gorm:"column:weight;not null;default:100"`
	Enabled  bool   `db:"enabled"    gorm:"column:enabled;not null;default:true"`

	// typed JSON 三段；Scanner/Valuer 在各自的文件里
	Auth         AuthConfig           `db:"auth"         gorm:"column:auth;type:varchar(2048);not null"`
	Routing      RoutingConfig        `db:"routing"      gorm:"column:routing;type:json;not null"`
	Quota        QuotaConfig          `db:"quota"        gorm:"column:quota;type:json"`
	Capabilities EndpointCapabilities `db:"capabilities" gorm:"column:capabilities;type:json"`
	Extra        datatypes.JSON       `db:"extra"        gorm:"column:extra;type:json"`

	CreatedAt time.Time  `db:"created_at" gorm:"column:created_at;not null;default:CURRENT_TIMESTAMP(6)"`
	UpdatedAt time.Time  `db:"updated_at" gorm:"column:updated_at;not null;default:CURRENT_TIMESTAMP(6)"`
	DeletedAt *time.Time `db:"deleted_at" gorm:"column:deleted_at;index:idx_deleted_at"`
}

func (Endpoint) TableName() string { return "endpoints" }

// EndpointCapabilities 已知结构的能力标记；JSON 序列化进 endpoints.capabilities 列。
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
	b, err := bytesFromScan(value, "EndpointCapabilities")
	if err != nil {
		return err
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

// =============================================================================
// APIKey
// =============================================================================

// APIKey 对应表 api_keys。
//
// **v0.3 改动**：加 quota_policy_id 列（API key 级限流；与主账号级 quota 叠加）。
//
// DB 不存明文：服务端生成 sk-XXX → SHA-256 → api_key_hash 入库。
type APIKey struct {
	ID            int64      `db:"id"               gorm:"column:id;primaryKey;autoIncrement"`
	AccountID     string     `db:"account_id"        gorm:"column:account_id;size:64;not null;default:default;uniqueIndex:uk_account_api_key_id;index:idx_account_sub_account_id,priority:1"` // 主账号 pin / 计费主体
	APIKeyHash    string     `db:"api_key_hash"     gorm:"column:api_key_hash;size:64;not null;uniqueIndex:uk_api_key_hash"`
	APIKeyPrefix  string     `db:"api_key_prefix"   gorm:"column:api_key_prefix;size:16;not null"`
	APIKeyID      string     `db:"api_key_id"       gorm:"column:api_key_id;size:64;not null;uniqueIndex:uk_account_api_key_id"`
	Name          string     `db:"name"             gorm:"column:name;size:64;not null;default:''"`
	SubAccountID  string     `db:"sub_account_id"   gorm:"column:sub_account_id;size:64;not null;index:idx_account_sub_account_id,priority:2"` // 子账户 / 操作者
	Group         string     `db:"group_name"       gorm:"column:group_name;size:64;not null;default:default"`
	ExternalUser  bool       `db:"external_user"    gorm:"column:external_user;not null;default:false"`
	Enabled       bool       `db:"enabled"          gorm:"column:enabled;not null;default:true"`
	ExpiresAt     *time.Time `db:"expires_at"       gorm:"column:expires_at;index:idx_expires_at"`
	LastUsedAt    *time.Time `db:"last_used_at"     gorm:"column:last_used_at"`
	RevokedAt     *time.Time `db:"revoked_at"       gorm:"column:revoked_at"`
	QuotaPolicyID *int64     `db:"quota_policy_id"  gorm:"column:quota_policy_id"`

	CreatedAt time.Time  `db:"created_at" gorm:"column:created_at;not null;default:CURRENT_TIMESTAMP(6)"`
	UpdatedAt time.Time  `db:"updated_at" gorm:"column:updated_at;not null;default:CURRENT_TIMESTAMP(6)"`
	DeletedAt *time.Time `db:"deleted_at" gorm:"column:deleted_at;index:idx_deleted_at"`
}

func (APIKey) TableName() string { return "api_keys" }

// ToUserIdentity 把 DB 行映射成 M2 Auth 给后续 middleware 用的 UserIdentity。
//
// **不含 AccountQuotaPolicyID**：那个字段只能从 JOIN accounts 拿，APIKey 单行没有。
// SQLAPIKeyProvider.Resolve 直接构造完整 UserIdentity（带 AccountQuotaPolicyID），
// 不走这个方法。
func (a APIKey) ToUserIdentity() UserIdentity {
	return UserIdentity{
		AccountID:           a.AccountID,
		SubAccountID:        a.SubAccountID,
		APIKeyID:            a.APIKeyID,
		Group:               a.Group,
		ExternalUser:        a.ExternalUser,
		APIKeyQuotaPolicyID: a.QuotaPolicyID,
	}
}

// =============================================================================
// PricingVersion：append-only 价格版本
// =============================================================================

// PricingVersion 对应表 pricing_versions。
//
// **append-only**：rule_json 一旦发布永不 UPDATE；改价 = 一次事务里
//
//  1. 给当前 active 行 UPDATE effective_to = NOW()
//  2. INSERT 新行 effective_from = NOW(), effective_to = NULL
//
// gateway 只读：M5 GetActive 拿当前版本，rc.Pricing 快照引用 ID。
// gateway 不 unmarshal rule_json，billing engine 自己定义 schema。
type PricingVersion struct {
	ID             int64          `db:"id"               gorm:"column:id;primaryKey;autoIncrement"`
	AccountID      string         `db:"account_id"        gorm:"column:account_id;size:64;not null;default:default;index:idx_active_lookup,priority:1"`
	ModelServiceID int64          `db:"model_service_id" gorm:"column:model_service_id;not null;index:idx_active_lookup,priority:2"`
	RuleClass      string         `db:"rule_class"       gorm:"column:rule_class;size:64;not null;default:standard;index:idx_active_lookup,priority:3"`
	EffectiveFrom  time.Time      `db:"effective_from"   gorm:"column:effective_from;not null;index:idx_active_lookup,priority:4"`
	EffectiveTo    *time.Time     `db:"effective_to"     gorm:"column:effective_to;index:idx_effective_to"`
	RuleJSON       datatypes.JSON `db:"rule_json"        gorm:"column:rule_json;type:json;not null"`
	CreatedAt      time.Time      `db:"created_at"       gorm:"column:created_at;not null;default:CURRENT_TIMESTAMP(6)"`
	CreatedBy      string         `db:"created_by"       gorm:"column:created_by;size:128;not null;default:''"`
	Notes          string         `db:"notes"            gorm:"column:notes;size:512;not null;default:''"`
}

func (PricingVersion) TableName() string { return "pricing_versions" }

// =============================================================================
// EndpointForm helper
// =============================================================================

// EndpointForm 由 Capabilities 派生（保留原 domain 的 helper）。
type EndpointForm int

const (
	FormVendor EndpointForm = iota
	FormSelfHosted
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

// bytesFromScan 把 driver.Value 标准化成 []byte；JSON 列 Scanner 复用。
func bytesFromScan(value any, typeName string) ([]byte, error) {
	switch v := value.(type) {
	case []byte:
		return v, nil
	case string:
		return []byte(v), nil
	default:
		return nil, errors.New(typeName + ": unsupported scan source")
	}
}

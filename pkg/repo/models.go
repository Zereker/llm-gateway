package repo

import (
	"database/sql/driver"
	"encoding/json"
	"errors"
	"time"
)

// models.go 定义"业务实体"——gateway（sqlx Reader）使用；写入由 deployer 走 SQL 维护。
//
// **schema 真相在 pkg/infra/schema.sql**；这里的 `db:` tag 只描述列名给 sqlx 用。
// 不再带 gorm tag——gateway 是只读数据面，不用 AutoMigrate，DDL 演进只走 SQL。
//
// JSON 列两种处理方式：
//   - **已知结构**（EndpointCapabilities / AuthConfig / RoutingConfig / QuotaConfig）：
//     typed struct + 自定义 Scanner/Valuer，调用方直接读字段，DB 端透明 JSON 序列化
//   - **未知 / 可扩展结构**（rule_json / Extra）：用 json.RawMessage——
//     字节透传，gateway 不解析
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
	Pin           string `db:"pin"`
	Name          string `db:"name"`
	Enabled       bool   `db:"enabled"`
	QuotaPolicyID *int64 `db:"quota_policy_id"`

	CreatedAt time.Time  `db:"created_at"`
	UpdatedAt time.Time  `db:"updated_at"`
	DeletedAt *time.Time `db:"deleted_at"`
}

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
type QuotaPolicy struct {
	ID          int64           `db:"id"`
	Name        string          `db:"name"`
	Description string          `db:"description"`
	RuleJSON    json.RawMessage `db:"rule_json"`
	Enabled     bool            `db:"enabled"`

	CreatedAt time.Time  `db:"created_at"`
	UpdatedAt time.Time  `db:"updated_at"`
	DeletedAt *time.Time `db:"deleted_at"`
}

// =============================================================================
// ModelService：全局模型 catalog
// =============================================================================

// ModelService 对应表 model_services。
//
// **v0.3 改动**：删 account_id（改为全局 catalog）/ group_name / spec_detail。
// 模型可见性走 account_model_subscriptions；group 是 endpoint 维度。
type ModelService struct {
	ID        int64  `db:"id"`
	ServiceID string `db:"service_id"`
	Model     string `db:"model"`

	CreatedAt time.Time  `db:"created_at"`
	UpdatedAt time.Time  `db:"updated_at"`
	DeletedAt *time.Time `db:"deleted_at"`
}

// =============================================================================
// AccountModelSubscription：主账号 × model 的可见性 N:M
// =============================================================================

// AccountModelSubscription 对应表 account_model_subscriptions。
//
// M5 在确认 model 在 catalog 后，按 (主账号 pin, model_service_id) 查这张表；
// 没找到 → 403 "model not subscribed"。
type AccountModelSubscription struct {
	ID             int64  `db:"id"`
	AccountID      string `db:"account_id"`
	ModelServiceID int64  `db:"model_service_id"`
	Enabled        bool   `db:"enabled"`

	CreatedAt time.Time  `db:"created_at"`
	UpdatedAt time.Time  `db:"updated_at"`
	DeletedAt *time.Time `db:"deleted_at"`
}

// =============================================================================
// Endpoint：全局上游接入点
// =============================================================================

// Endpoint 对应表 endpoints。
//
// **v0.3 改动**：删 account_id（改为全局；BYOK 等真要做时加 nullable account_id）。
//
// 核心列只放调度选路 hot path 用得到的；vendor-specific 全进 typed JSON。
type Endpoint struct {
	ID       int64  `db:"id"`
	Name     string `db:"name"`
	Vendor   string `db:"vendor"`
	Protocol string `db:"protocol"` // domain.Protocol.String()——mappers 来回转
	Model    string `db:"model"`
	Group    string `db:"group_name"`
	Weight   uint32 `db:"weight"`
	Enabled  bool   `db:"enabled"`

	// typed JSON 三段；Scanner/Valuer 在各自的文件里
	Auth         AuthConfig           `db:"auth"`
	Routing      RoutingConfig        `db:"routing"`
	Quota        QuotaConfig          `db:"quota"`
	Capabilities EndpointCapabilities `db:"capabilities"`
	Extra        json.RawMessage      `db:"extra"`

	CreatedAt time.Time  `db:"created_at"`
	UpdatedAt time.Time  `db:"updated_at"`
	DeletedAt *time.Time `db:"deleted_at"`
}

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
	ID            int64      `db:"id"`
	AccountID     string     `db:"account_id"` // 主账号 pin / 计费主体
	APIKeyHash    string     `db:"api_key_hash"`
	APIKeyPrefix  string     `db:"api_key_prefix"`
	APIKeyID      string     `db:"api_key_id"`
	Name          string     `db:"name"`
	SubAccountID  string     `db:"sub_account_id"` // 子账户 / 操作者
	Group         string     `db:"group_name"`
	ExternalUser  bool       `db:"external_user"`
	Enabled       bool       `db:"enabled"`
	ExpiresAt     *time.Time `db:"expires_at"`
	LastUsedAt    *time.Time `db:"last_used_at"`
	RevokedAt     *time.Time `db:"revoked_at"`
	QuotaPolicyID *int64     `db:"quota_policy_id"`

	CreatedAt time.Time  `db:"created_at"`
	UpdatedAt time.Time  `db:"updated_at"`
	DeletedAt *time.Time `db:"deleted_at"`
}

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
	ID             int64           `db:"id"`
	AccountID      string          `db:"account_id"`
	ModelServiceID int64           `db:"model_service_id"`
	RuleClass      string          `db:"rule_class"`
	EffectiveFrom  time.Time       `db:"effective_from"`
	EffectiveTo    *time.Time      `db:"effective_to"`
	RuleJSON       json.RawMessage `db:"rule_json"`
	CreatedAt      time.Time       `db:"created_at"`
	CreatedBy      string          `db:"created_by"`
	Notes          string          `db:"notes"`
}

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

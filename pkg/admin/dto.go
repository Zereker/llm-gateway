package admin

import (
	"encoding/json"
	"time"

	"gorm.io/datatypes"

	"github.com/zereker-labs/ai-gateway/pkg/repo"
)

// dto.go 提供 admin REST API 边界用的传输结构体。
//
// 跟 repo.X（DB Model）分开的原因：
//
//  1. JSON 命名风格：DTO 用 snake_case（REST 行业惯例），repo.X 字段是 PascalCase
//  2. APIKey 明文：admin 创建 api_key 时一次性返回明文（apiKeyCreateResponse），
//     普通 GET / List 只返 prefix（"sk-abc1de2f..."）
//  3. 字段裁剪 / 计算字段 / 版本演进：API 演化不污染 DB 模型
//
// **v0.3 改动**：
//   - modelServiceDTO 删 tenant_id/group/spec_detail
//   - endpointDTO 删 tenant_id
//   - apiKeyDTO 加 quota_policy_id
//   - 新增 tenantDTO / quotaPolicyDTO / subscriptionDTO

// =============================================================================
// Tenant
// =============================================================================

type tenantDTO struct {
	Pin             string  `json:"pin"`
	Name            string  `json:"name"`
	Enabled         bool    `json:"enabled"`
	QuotaPolicyID   *int64  `json:"quota_policy_id,omitempty"`

	CreatedAt time.Time  `json:"created_at,omitempty"`
	UpdatedAt time.Time  `json:"updated_at,omitempty"`
	DeletedAt *time.Time `json:"deleted_at,omitempty"`
}

func tenantToDTO(t *repo.Tenant) tenantDTO {
	return tenantDTO{
		Pin:           t.Pin,
		Name:          t.Name,
		Enabled:       t.Enabled,
		QuotaPolicyID: t.QuotaPolicyID,
		CreatedAt:     t.CreatedAt,
		UpdatedAt:     t.UpdatedAt,
		DeletedAt:     t.DeletedAt,
	}
}

func dtoToTenant(d tenantDTO) *repo.Tenant {
	return &repo.Tenant{
		Pin:           d.Pin,
		Name:          d.Name,
		Enabled:       d.Enabled,
		QuotaPolicyID: d.QuotaPolicyID,
	}
}

// =============================================================================
// QuotaPolicy
// =============================================================================

type quotaPolicyDTO struct {
	ID          int64           `json:"id,omitempty"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	RuleJSON    json.RawMessage `json:"rule_json"`
	Enabled     bool            `json:"enabled"`

	CreatedAt time.Time  `json:"created_at,omitempty"`
	UpdatedAt time.Time  `json:"updated_at,omitempty"`
	DeletedAt *time.Time `json:"deleted_at,omitempty"`
}

func quotaPolicyToDTO(p *repo.QuotaPolicy) quotaPolicyDTO {
	return quotaPolicyDTO{
		ID:          p.ID,
		Name:        p.Name,
		Description: p.Description,
		RuleJSON:    jsonRawFromDatatype(p.RuleJSON),
		Enabled:     p.Enabled,
		CreatedAt:   p.CreatedAt,
		UpdatedAt:   p.UpdatedAt,
		DeletedAt:   p.DeletedAt,
	}
}

func dtoToQuotaPolicy(d quotaPolicyDTO) *repo.QuotaPolicy {
	return &repo.QuotaPolicy{
		ID:          d.ID,
		Name:        d.Name,
		Description: d.Description,
		RuleJSON:    datatypeFromJSONRaw(d.RuleJSON),
		Enabled:     d.Enabled,
	}
}

// =============================================================================
// Subscription
// =============================================================================

type subscriptionDTO struct {
	ID               int64  `json:"id,omitempty"`
	TenantID         string `json:"tenant_id"`
	ModelServiceID   int64  `json:"model_service_id"`
	Enabled          bool   `json:"enabled"`

	CreatedAt time.Time  `json:"created_at,omitempty"`
	UpdatedAt time.Time  `json:"updated_at,omitempty"`
	DeletedAt *time.Time `json:"deleted_at,omitempty"`
}

func subscriptionToDTO(s *repo.TenantModelSubscription) subscriptionDTO {
	return subscriptionDTO{
		ID:             s.ID,
		TenantID:       s.TenantID,
		ModelServiceID: s.ModelServiceID,
		Enabled:        s.Enabled,
		CreatedAt:      s.CreatedAt,
		UpdatedAt:      s.UpdatedAt,
		DeletedAt:      s.DeletedAt,
	}
}

// =============================================================================
// ModelService
// =============================================================================

type modelServiceDTO struct {
	ID         int64  `json:"id,omitempty"`
	ServiceID  string `json:"service_id"`
	Model      string `json:"model"`

	CreatedAt time.Time  `json:"created_at,omitempty"`
	UpdatedAt time.Time  `json:"updated_at,omitempty"`
	DeletedAt *time.Time `json:"deleted_at,omitempty"`
}

func msToDTO(m *repo.ModelService) modelServiceDTO {
	return modelServiceDTO{
		ID:        m.ID,
		ServiceID: m.ServiceID,
		Model:     m.Model,
		CreatedAt: m.CreatedAt,
		UpdatedAt: m.UpdatedAt,
		DeletedAt: m.DeletedAt,
	}
}

func dtoToMS(d modelServiceDTO) *repo.ModelService {
	return &repo.ModelService{
		ID:        d.ID,
		ServiceID: d.ServiceID,
		Model:     d.Model,
	}
}

// =============================================================================
// Endpoint
// =============================================================================

// endpointDTO admin REST 形态。
//
// **auth 字段语义**：
//   - 入站（POST/PUT body）：明文 {"type":"bearer", "payload":{"api_key":"sk-..."}}
//   - 出站（GET 响应）：repo.AuthConfig.MarshalJSON 屏蔽成 {"type":"bearer","payload":"***"}
type endpointDTO struct {
	ID       int64  `json:"id,omitempty"`
	Name     string `json:"name"`
	Vendor   string `json:"vendor"`
	Model    string `json:"model"`
	Group    string `json:"group"`
	Weight   uint32 `json:"weight"`
	Enabled  bool   `json:"enabled"`

	Auth         repo.AuthConfig           `json:"auth"`
	Routing      repo.RoutingConfig        `json:"routing"`
	Quota        repo.QuotaConfig          `json:"quota,omitempty"`
	Capabilities repo.EndpointCapabilities `json:"capabilities,omitempty"`
	Extra        json.RawMessage           `json:"extra,omitempty"`

	CreatedAt time.Time  `json:"created_at,omitempty"`
	UpdatedAt time.Time  `json:"updated_at,omitempty"`
	DeletedAt *time.Time `json:"deleted_at,omitempty"`
}

func epToDTO(e *repo.Endpoint) endpointDTO {
	return endpointDTO{
		ID:           e.ID,
		Name:         e.Name,
		Vendor:       e.Vendor,
		Model:        e.Model,
		Group:        e.Group,
		Weight:       e.Weight,
		Enabled:      e.Enabled,
		Auth:         e.Auth,
		Routing:      e.Routing,
		Quota:        e.Quota,
		Capabilities: e.Capabilities,
		Extra:        jsonRawFromDatatype(e.Extra),
		CreatedAt:    e.CreatedAt,
		UpdatedAt:    e.UpdatedAt,
		DeletedAt:    e.DeletedAt,
	}
}

func dtoToEp(d endpointDTO) *repo.Endpoint {
	return &repo.Endpoint{
		ID:           d.ID,
		Name:         d.Name,
		Vendor:       d.Vendor,
		Model:        d.Model,
		Group:        d.Group,
		Weight:       d.Weight,
		Enabled:      d.Enabled,
		Auth:         d.Auth,
		Routing:      d.Routing,
		Quota:        d.Quota,
		Capabilities: d.Capabilities,
		Extra:        datatypeFromJSONRaw(d.Extra),
	}
}

// =============================================================================
// APIKey
// =============================================================================

// apiKeyDTO 普通 GET / List 用：**不返明文 api_key**（已 hash），只返 prefix。
type apiKeyDTO struct {
	ID            int64      `json:"id,omitempty"`
	TenantID      string     `json:"tenant_id"`
	APIKeyID      string     `json:"api_key_id"`
	APIKeyPrefix  string     `json:"api_key_prefix"`
	Name          string     `json:"name,omitempty"`
	UserID        string     `json:"user_id"`
	Group         string     `json:"group"`
	ExternalUser  bool       `json:"external_user"`
	Enabled       bool       `json:"enabled"`
	ExpiresAt     *time.Time `json:"expires_at,omitempty"`
	LastUsedAt    *time.Time `json:"last_used_at,omitempty"`
	RevokedAt     *time.Time `json:"revoked_at,omitempty"`
	QuotaPolicyID *int64     `json:"quota_policy_id,omitempty"`

	CreatedAt time.Time  `json:"created_at,omitempty"`
	UpdatedAt time.Time  `json:"updated_at,omitempty"`
	DeletedAt *time.Time `json:"deleted_at,omitempty"`
}

func apiKeyToDTO(k *repo.APIKey) apiKeyDTO {
	return apiKeyDTO{
		ID:            k.ID,
		TenantID:      k.TenantID,
		APIKeyID:      k.APIKeyID,
		APIKeyPrefix:  k.APIKeyPrefix,
		Name:          k.Name,
		UserID:        k.UserID,
		Group:         k.Group,
		ExternalUser:  k.ExternalUser,
		Enabled:       k.Enabled,
		ExpiresAt:     k.ExpiresAt,
		LastUsedAt:    k.LastUsedAt,
		RevokedAt:     k.RevokedAt,
		QuotaPolicyID: k.QuotaPolicyID,
		CreatedAt:     k.CreatedAt,
		UpdatedAt:     k.UpdatedAt,
		DeletedAt:     k.DeletedAt,
	}
}

// apiKeyCreateResponse POST /apikeys 的响应：包含一次性明文 api_key。
type apiKeyCreateResponse struct {
	apiKeyDTO
	APIKey string `json:"api_key"` // 明文 sk-XXX；只在 Create 响应出现
}

// =============================================================================
// helpers
// =============================================================================

func jsonRawFromDatatype(j datatypes.JSON) json.RawMessage {
	if len(j) == 0 {
		return nil
	}
	return json.RawMessage(j)
}

func datatypeFromJSONRaw(r json.RawMessage) datatypes.JSON {
	if len(r) == 0 {
		return nil
	}
	return datatypes.JSON(r)
}

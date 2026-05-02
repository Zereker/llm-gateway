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
//     普通 GET / List 只返 api_key_id（apiKeyDTO）；明文不进 dto base
//  3. 字段裁剪 / 计算字段 / 版本演进：API 演化不污染 DB 模型

type modelServiceDTO struct {
	ID         int64           `json:"id,omitempty"`
	TenantID   string          `json:"tenant_id"`
	ServiceID  string          `json:"service_id"`
	Model      string          `json:"model"`
	UpdateTime time.Time       `json:"update_time,omitempty"`
	SpecDetail json.RawMessage `json:"spec_detail,omitempty"`
	Group      string          `json:"group"`
	Tpm        int64           `json:"tpm"`
	Rpm        int64           `json:"rpm"`
}

func msToDTO(m *repo.ModelService) modelServiceDTO {
	return modelServiceDTO{
		ID:         m.ID,
		TenantID:   m.TenantID,
		ServiceID:  m.ServiceID,
		Model:      m.Model,
		UpdateTime: m.UpdateTime,
		SpecDetail: jsonRawFromDatatype(m.SpecDetail),
		Group:      m.Group,
		Tpm:        m.Tpm,
		Rpm:        m.Rpm,
	}
}

func dtoToMS(d modelServiceDTO) *repo.ModelService {
	return &repo.ModelService{
		ID:         d.ID,
		TenantID:   d.TenantID,
		ServiceID:  d.ServiceID,
		Model:      d.Model,
		UpdateTime: d.UpdateTime,
		SpecDetail: datatypeFromJSONRaw(d.SpecDetail),
		Group:      d.Group,
		Tpm:        d.Tpm,
		Rpm:        d.Rpm,
	}
}

type endpointDTO struct {
	TenantID     string                    `json:"tenant_id"`
	ID           string                    `json:"id"`
	Vendor       string                    `json:"vendor"`
	URL          string                    `json:"url"`
	APIKey       string                    `json:"api_key"` // plain string；admin 边界不走 repo.Secret 屏蔽
	Group        string                    `json:"group"`
	Model        string                    `json:"model"`
	Weight       int                       `json:"weight"`
	RPM          int64                     `json:"rpm"`
	TPM          int64                     `json:"tpm"`
	RPS          int64                     `json:"rps"`
	Capabilities repo.EndpointCapabilities `json:"capabilities"`
	Extra        json.RawMessage           `json:"extra,omitempty"`
}

func epToDTO(e *repo.Endpoint) endpointDTO {
	return endpointDTO{
		TenantID:     e.TenantID,
		ID:           e.ID,
		Vendor:       e.Vendor,
		URL:          e.URL,
		APIKey:       e.APIKey.Reveal(),
		Group:        e.Group,
		Model:        e.Model,
		Weight:       e.Weight,
		RPM:          e.RPM,
		TPM:          e.TPM,
		RPS:          e.RPS,
		Capabilities: e.Capabilities,
		Extra:        jsonRawFromDatatype(e.Extra),
	}
}

func dtoToEp(d endpointDTO) *repo.Endpoint {
	return &repo.Endpoint{
		TenantID:     d.TenantID,
		ID:           d.ID,
		Vendor:       d.Vendor,
		URL:          d.URL,
		APIKey:       repo.Secret(d.APIKey),
		Group:        d.Group,
		Model:        d.Model,
		Weight:       d.Weight,
		RPM:          d.RPM,
		TPM:          d.TPM,
		RPS:          d.RPS,
		Capabilities: d.Capabilities,
		Extra:        datatypeFromJSONRaw(d.Extra),
	}
}

// apiKeyDTO 普通 GET / List 用：**不返明文 api_key**。
type apiKeyDTO struct {
	ID           int64      `json:"id,omitempty"`
	TenantID     string     `json:"tenant_id"`
	APIKeyID     string     `json:"api_key_id"`
	UserID       string     `json:"user_id"`
	Group        string     `json:"group"`
	ExternalUser bool       `json:"external_user"`
	Enabled      bool       `json:"enabled"`
	ExpiresAt    *time.Time `json:"expires_at,omitempty"`
	CreatedAt    time.Time  `json:"created_at,omitempty"`
}

func apiKeyToDTO(k *repo.APIKey) apiKeyDTO {
	return apiKeyDTO{
		ID:           k.ID,
		TenantID:     k.TenantID,
		APIKeyID:     k.APIKeyID,
		UserID:       k.UserID,
		Group:        k.Group,
		ExternalUser: k.ExternalUser,
		Enabled:      k.Enabled,
		ExpiresAt:    k.ExpiresAt,
		CreatedAt:    k.CreatedAt,
	}
}

// apiKeyCreateResponse POST /apikeys 的响应：包含一次性明文 api_key。
//
// 客户端**只在这一次响应里**能拿到明文；后续 GET 只返 apiKeyDTO。
type apiKeyCreateResponse struct {
	apiKeyDTO
	APIKey string `json:"api_key"` // 明文 sk-xxx；只在 Create 响应出现
}

// jsonRawFromDatatype 把 gorm datatypes.JSON 转回标准 json.RawMessage。
// 空值（未设置 / NULL）返回 nil，避免输出 "null" 字面量。
func jsonRawFromDatatype(j datatypes.JSON) json.RawMessage {
	if len(j) == 0 {
		return nil
	}
	return json.RawMessage(j)
}

// datatypeFromJSONRaw 把 json.RawMessage 转成 gorm datatypes.JSON。
// 空 RawMessage 返回 nil（gorm 会写 NULL 而不是 ""）。
func datatypeFromJSONRaw(r json.RawMessage) datatypes.JSON {
	if len(r) == 0 {
		return nil
	}
	return datatypes.JSON(r)
}

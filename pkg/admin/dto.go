package admin

import (
	"encoding/json"
	"time"

	"gorm.io/datatypes"

	"github.com/zereker-labs/ai-gateway/pkg/repo"
)

// dto.go 提供 admin REST API 边界用的传输结构体。
//
// 跟 repo.X（gorm Model）分开的原因：
//
//  1. JSON 命名风格：DTO 用 snake_case（REST 行业惯例），repo.X 字段是 PascalCase
//  2. APIKey：admin 边界明文 string，绕开 repo.Secret 的屏蔽（gateway 那边用 repo.Secret）
//  3. 字段裁剪 / 计算字段 / 版本演进：API 演化不污染 DB 模型

type modelServiceDTO struct {
	ID         int64           `json:"id,omitempty"`
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

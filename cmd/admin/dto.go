package main

import (
	"encoding/json"
	"time"

	"github.com/zereker-labs/ai-gateway/pkg/domain"
)

// dto.go 提供 admin REST API 边界用的传输结构体。
//
// 为什么要 DTO 而不直接用 domain：
//
//  1. domain.Secret.MarshalJSON 强制输出 "***"——admin 写完 endpoint 后
//     GET 回来不能看到 key、PUT 时还会把 *** 当新 key 存回去。
//     DTO 里 APIKey 用 plain string，绕开 Secret 的屏蔽。
//  2. JSON 字段统一 snake_case（REST 行业惯例）。domain 字段是 PascalCase。
//  3. 未来 API 演进可以加版本字段、计算字段、隐藏 internal 字段，不污染 domain。

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

func msToDTO(s *domain.ModelServiceSnapshot) modelServiceDTO {
	return modelServiceDTO{
		ID:         s.ID,
		ServiceID:  s.ServiceID,
		Model:      s.Model,
		UpdateTime: s.UpdateTime,
		SpecDetail: s.SpecDetail,
		Group:      s.Group,
		Tpm:        s.Tpm,
		Rpm:        s.Rpm,
	}
}

func dtoToMS(d modelServiceDTO) *domain.ModelServiceSnapshot {
	return &domain.ModelServiceSnapshot{
		ID:         d.ID,
		ServiceID:  d.ServiceID,
		Model:      d.Model,
		UpdateTime: d.UpdateTime,
		SpecDetail: d.SpecDetail,
		Group:      d.Group,
		Tpm:        d.Tpm,
		Rpm:        d.Rpm,
	}
}

type endpointDTO struct {
	ID           string                      `json:"id"`
	Vendor       string                      `json:"vendor"`
	URL          string                      `json:"url"`
	APIKey       string                      `json:"api_key"` // plain string；admin 边界明文，不走 domain.Secret 屏蔽
	Group        string                      `json:"group"`
	Model        string                      `json:"model"`
	Weight       int                         `json:"weight"`
	RPM          int64                       `json:"rpm"`
	TPM          int64                       `json:"tpm"`
	RPS          int64                       `json:"rps"`
	Capabilities domain.EndpointCapabilities `json:"capabilities"`
	Extra        json.RawMessage             `json:"extra,omitempty"`
}

func epToDTO(ep *domain.Endpoint) endpointDTO {
	return endpointDTO{
		ID:           ep.ID,
		Vendor:       ep.Vendor,
		URL:          ep.URL,
		APIKey:       ep.APIKey.Reveal(),
		Group:        ep.Group,
		Model:        ep.Model,
		Weight:       ep.Weight,
		RPM:          ep.RPM,
		TPM:          ep.TPM,
		RPS:          ep.RPS,
		Capabilities: ep.Capabilities,
		Extra:        ep.Extra,
	}
}

func dtoToEp(d endpointDTO) *domain.Endpoint {
	return &domain.Endpoint{
		ID:           d.ID,
		Vendor:       d.Vendor,
		URL:          d.URL,
		APIKey:       domain.Secret(d.APIKey),
		Group:        d.Group,
		Model:        d.Model,
		Weight:       d.Weight,
		RPM:          d.RPM,
		TPM:          d.TPM,
		RPS:          d.RPS,
		Capabilities: d.Capabilities,
		Extra:        d.Extra,
	}
}

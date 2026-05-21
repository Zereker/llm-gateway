package repo

import (
	"github.com/zereker/llm-gateway/pkg/domain"
)

// mappers.go: 把 SQL-layer struct（带 db/gorm tag + Scanner/Valuer）映射为 domain
// 业务结构（docs/06 §3 — domain 不引用 repo，repo 在边界做转换）。
//
// 所有 repo 接口返回 *domain.X，consumer 不再看到 SQL tag / 加密细节。

// ToDomainEndpoint 把 repo.Endpoint (SQL row) → *domain.Endpoint。
//
// **Protocol 映射**：repo 用 VARCHAR(32) 字符串列；domain 用 typed Protocol。
// 未知字符串 → ProtoUnknown（DefaultLookup 看到会返 nil → eligibility 剔除该 ep）。
func ToDomainEndpoint(e *Endpoint) *domain.Endpoint {
	if e == nil {
		return nil
	}
	return &domain.Endpoint{
		ID:           e.ID,
		Name:         e.Name,
		Vendor:       e.Vendor,
		Protocol:     domain.ParseProtocol(e.Protocol),
		Model:        e.Model,
		Group:        e.Group,
		Weight:       e.Weight,
		Enabled:      e.Enabled,
		Auth:         domain.AuthConfig{Type: e.Auth.Type, Payload: e.Auth.Payload},
		Routing:      domain.RoutingConfig(e.Routing),
		Quota:        domain.QuotaConfig(e.Quota),
		Capabilities: domain.EndpointCapabilities(e.Capabilities),
		Extra:        []byte(e.Extra),
		CreatedAt:    e.CreatedAt,
		UpdatedAt:    e.UpdatedAt,
		DeletedAt:    e.DeletedAt,
	}
}

// ToDomainEndpoints 批量映射。
func ToDomainEndpoints(rows []*Endpoint) []*domain.Endpoint {
	if rows == nil {
		return nil
	}
	out := make([]*domain.Endpoint, 0, len(rows))
	for _, r := range rows {
		out = append(out, ToDomainEndpoint(r))
	}
	return out
}

// ToDomainModelService 把 SQL row → domain.ModelService。
func ToDomainModelService(m *ModelService) *domain.ModelService {
	if m == nil {
		return nil
	}
	return &domain.ModelService{
		ID:        m.ID,
		ServiceID: m.ServiceID,
		Model:     m.Model,
		CreatedAt: m.CreatedAt,
		UpdatedAt: m.UpdatedAt,
		DeletedAt: m.DeletedAt,
	}
}


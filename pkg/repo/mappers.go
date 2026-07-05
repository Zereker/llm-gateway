package repo

import (
	"github.com/zereker/llm-gateway/pkg/domain"
)

// mappers.go: maps SQL-layer structs (with db tags + Scanner/Valuer) into
// domain business structs (docs/06 §3 — domain does not import repo; repo
// does the conversion at the boundary).
//
// All repo interfaces return *domain.X, so consumers never see SQL tags /
// encryption details.

// ToDomainEndpoint maps repo.Endpoint (SQL row) -> *domain.Endpoint.
//
// **Protocol mapping**: repo uses a VARCHAR(32) string column; domain uses a
// typed Protocol. An unknown string maps to ProtoUnknown (DefaultLookup sees
// this and returns nil -> eligibility excludes this endpoint).
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
		Quirks:       []byte(e.Quirks),
		Extra:        []byte(e.Extra),
		CreatedAt:    e.CreatedAt,
		UpdatedAt:    e.UpdatedAt,
		DeletedAt:    e.DeletedAt,
	}
}

// ToDomainEndpoints maps a batch.
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

// ToDomainModelService maps a SQL row -> domain.ModelService.
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

package console

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/zereker/llm-gateway/pkg/endpointcheck"
	"github.com/zereker/llm-gateway/pkg/repo"
)

type EndpointInput struct {
	Name         string             `json:"name"`
	Vendor       string             `json:"vendor"`
	Protocol     string             `json:"protocol"`
	Model        string             `json:"model"`
	Group        string             `json:"group,omitempty"`
	Weight       uint32             `json:"weight,omitempty"`
	Enabled      *bool              `json:"enabled,omitempty"`
	Auth         AuthInput          `json:"auth"`
	Routing      repo.RoutingConfig `json:"routing"`
	Capabilities json.RawMessage    `json:"capabilities,omitempty"`
	Quota        json.RawMessage    `json:"quota,omitempty"`
	Quirks       json.RawMessage    `json:"quirks,omitempty"`
	Extra        json.RawMessage    `json:"extra,omitempty"`
}

type AuthInput struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

type InvalidEndpointError struct{ Reasons []string }

func (e *InvalidEndpointError) Error() string {
	return fmt.Sprintf("endpoint invalid: %v", e.Reasons)
}

func (s *Store) CreateEndpoint(ctx context.Context, in EndpointInput) (int64, error) {
	auth, err := repo.EncodePayload(in.Auth.Type, in.Auth.Payload)
	if err != nil {
		return 0, &InvalidEndpointError{Reasons: []string{"invalid_auth: " + err.Error()}}
	}
	ep := &repo.Endpoint{
		Name: in.Name, Vendor: in.Vendor, Protocol: in.Protocol, Model: in.Model,
		Group: orDefault(in.Group, "default"), Weight: orWeight(in.Weight, 100),
		Enabled: in.Enabled == nil || *in.Enabled, Auth: auth, Routing: in.Routing,
		Quirks: rawOrNil(in.Quirks), Extra: rawOrNil(in.Extra),
	}
	if len(in.Capabilities) > 0 {
		if err := json.Unmarshal(in.Capabilities, &ep.Capabilities); err != nil {
			return 0, &InvalidEndpointError{Reasons: []string{"invalid_capabilities: " + err.Error()}}
		}
	}
	if len(in.Quota) > 0 {
		if err := json.Unmarshal(in.Quota, &ep.Quota); err != nil {
			return 0, &InvalidEndpointError{Reasons: []string{"invalid_quota: " + err.Error()}}
		}
	}
	if reasons := endpointcheck.Validate(repo.ToDomainEndpoint(ep)); len(reasons) > 0 {
		return 0, &InvalidEndpointError{Reasons: reasons}
	}
	res, err := s.db.NamedExecContext(ctx,
		`INSERT INTO endpoints
		 (name, vendor, protocol, model, group_name, weight, enabled,
		  auth, routing, quota, capabilities, quirks, extra)
		 VALUES
		 (:name, :vendor, :protocol, :model, :group_name, :weight, :enabled,
		  :auth, :routing, :quota, :capabilities, :quirks, :extra)`, ep)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

type EndpointView struct {
	ID         int64     `json:"id"`
	Name       string    `json:"name"`
	Vendor     string    `json:"vendor"`
	Protocol   string    `json:"protocol"`
	Model      string    `json:"model"`
	Group      string    `json:"group"`
	Weight     uint32    `json:"weight"`
	Enabled    bool      `json:"enabled"`
	AuthType   string    `json:"auth_type"`
	RoutingURL string    `json:"routing_url,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
}

func endpointToView(e *repo.Endpoint) EndpointView {
	return EndpointView{
		ID: e.ID, Name: e.Name, Vendor: e.Vendor, Protocol: e.Protocol,
		Model: e.Model, Group: e.Group, Weight: e.Weight, Enabled: e.Enabled,
		AuthType: e.Auth.Type, RoutingURL: e.Routing.URL, CreatedAt: e.CreatedAt,
	}
}

const epSelectColumns = `id, name, vendor, protocol, model, group_name, weight, enabled,
	auth, routing, quota, capabilities, quirks, extra, created_at, updated_at, deleted_at`

func (s *Store) ListEndpoints(ctx context.Context) ([]EndpointView, error) {
	var rows []repo.Endpoint
	if err := s.db.SelectContext(ctx, &rows,
		`SELECT `+epSelectColumns+` FROM endpoints WHERE deleted_at IS NULL ORDER BY id`); err != nil {
		return nil, err
	}
	out := make([]EndpointView, len(rows))
	for i := range rows {
		out[i] = endpointToView(&rows[i])
	}
	return out, nil
}

func (s *Store) GetEndpoint(ctx context.Context, id int64) (*EndpointView, error) {
	var endpoint repo.Endpoint
	err := s.db.GetContext(ctx, &endpoint,
		`SELECT `+epSelectColumns+` FROM endpoints WHERE id = ? AND deleted_at IS NULL`, id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	view := endpointToView(&endpoint)
	return &view, nil
}

func (s *Store) DeleteEndpoint(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE endpoints SET deleted_at = NOW(6) WHERE id = ? AND deleted_at IS NULL`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

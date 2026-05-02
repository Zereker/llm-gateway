package repo

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jmoiron/sqlx"

	"github.com/zereker-labs/ai-gateway/pkg/domain"
)

// SQLEndpointRepo 是 EndpointReader + Writer 的 sqlx 实现。
//
// Capabilities 序列化为 JSON TEXT；Extra (json.RawMessage) 直接当 TEXT 存。
type SQLEndpointRepo struct {
	db *sqlx.DB
}

// NewSQLEndpointRepo 用现成的 *sqlx.DB 构造。
func NewSQLEndpointRepo(db *sqlx.DB) *SQLEndpointRepo {
	return &SQLEndpointRepo{db: db}
}

// dbEndpoint 是行级表示。
type dbEndpoint struct {
	ID           string `db:"id"`
	Vendor       string `db:"vendor"`
	URL          string `db:"url"`
	APIKey       string `db:"api_key"`
	GroupName    string `db:"group_name"`
	Model        string `db:"model"`
	Weight       int    `db:"weight"`
	RPM          int64  `db:"rpm"`
	TPM          int64  `db:"tpm"`
	RPS          int64  `db:"rps"`
	Capabilities string `db:"capabilities"`
	Extra        string `db:"extra"`
}

const epColumns = `id, vendor, url, api_key, group_name, model, weight, rpm, tpm, rps, capabilities, extra`

// PickForModel 实现 EndpointReader.PickForModel。
//
// v0.1：按 (model, group_name) 取第一行。无加权 / 无 Filter / 无 Cooldown。
func (r *SQLEndpointRepo) PickForModel(ctx context.Context, model, group string) (*domain.Endpoint, error) {
	if model == "" {
		return nil, errors.New("endpoint: empty model name")
	}
	if group == "" {
		group = "default"
	}
	var row dbEndpoint
	err := r.db.GetContext(ctx, &row, r.db.Rebind(
		`SELECT `+epColumns+`
		 FROM endpoints
		 WHERE model = ? AND group_name = ?
		 ORDER BY id LIMIT 1`),
		model, group)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("endpoint: no endpoint for model %q in group %q", model, group)
		}
		return nil, fmt.Errorf("endpoint: pick: %w", err)
	}
	return rowToEndpoint(row)
}

// GetByID 实现 EndpointReader.GetByID。
func (r *SQLEndpointRepo) GetByID(ctx context.Context, id string) (*domain.Endpoint, error) {
	if id == "" {
		return nil, errors.New("endpoint: empty id")
	}
	var row dbEndpoint
	err := r.db.GetContext(ctx, &row, r.db.Rebind(
		`SELECT `+epColumns+` FROM endpoints WHERE id = ?`), id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("endpoint: not found: %s", id)
		}
		return nil, fmt.Errorf("endpoint: get by id: %w", err)
	}
	return rowToEndpoint(row)
}

// List 实现 EndpointReader.List。
func (r *SQLEndpointRepo) List(ctx context.Context) ([]*domain.Endpoint, error) {
	var rows []dbEndpoint
	if err := r.db.SelectContext(ctx, &rows,
		`SELECT `+epColumns+` FROM endpoints ORDER BY id`); err != nil {
		return nil, fmt.Errorf("endpoint: list: %w", err)
	}
	out := make([]*domain.Endpoint, 0, len(rows))
	for i := range rows {
		ep, err := rowToEndpoint(rows[i])
		if err != nil {
			return nil, err
		}
		out = append(out, ep)
	}
	return out, nil
}

// Create 实现 EndpointWriter.Create。
func (r *SQLEndpointRepo) Create(ctx context.Context, ep *domain.Endpoint) error {
	if ep == nil || ep.ID == "" || ep.Vendor == "" || ep.Model == "" {
		return errors.New("endpoint: id, vendor, model required")
	}
	groupName := ep.Group
	if groupName == "" {
		groupName = "default"
	}
	caps, err := json.Marshal(ep.Capabilities)
	if err != nil {
		return fmt.Errorf("endpoint: marshal capabilities: %w", err)
	}
	_, err = r.db.ExecContext(ctx, r.db.Rebind(
		`INSERT INTO endpoints (`+epColumns+`)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`),
		ep.ID, ep.Vendor, ep.URL, string(ep.APIKey), groupName, ep.Model,
		ep.Weight, ep.RPM, ep.TPM, ep.RPS, string(caps), string(ep.Extra),
	)
	if err != nil {
		return fmt.Errorf("endpoint: create: %w", err)
	}
	return nil
}

// Update 实现 EndpointWriter.Update（按 ep.ID）。
func (r *SQLEndpointRepo) Update(ctx context.Context, ep *domain.Endpoint) error {
	if ep == nil || ep.ID == "" {
		return errors.New("endpoint: id required for update")
	}
	groupName := ep.Group
	if groupName == "" {
		groupName = "default"
	}
	caps, err := json.Marshal(ep.Capabilities)
	if err != nil {
		return fmt.Errorf("endpoint: marshal capabilities: %w", err)
	}
	res, err := r.db.ExecContext(ctx, r.db.Rebind(
		`UPDATE endpoints SET
		 vendor = ?, url = ?, api_key = ?, group_name = ?, model = ?,
		 weight = ?, rpm = ?, tpm = ?, rps = ?, capabilities = ?, extra = ?
		 WHERE id = ?`),
		ep.Vendor, ep.URL, string(ep.APIKey), groupName, ep.Model,
		ep.Weight, ep.RPM, ep.TPM, ep.RPS, string(caps), string(ep.Extra),
		ep.ID,
	)
	if err != nil {
		return fmt.Errorf("endpoint: update: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("endpoint: not found: %s", ep.ID)
	}
	return nil
}

// Delete 实现 EndpointWriter.Delete。
func (r *SQLEndpointRepo) Delete(ctx context.Context, id string) error {
	if id == "" {
		return errors.New("endpoint: empty id")
	}
	res, err := r.db.ExecContext(ctx,
		r.db.Rebind(`DELETE FROM endpoints WHERE id = ?`), id)
	if err != nil {
		return fmt.Errorf("endpoint: delete: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("endpoint: not found: %s", id)
	}
	return nil
}

func rowToEndpoint(row dbEndpoint) (*domain.Endpoint, error) {
	ep := &domain.Endpoint{
		ID:        row.ID,
		Vendor:    row.Vendor,
		URL:       row.URL,
		APIKey:    domain.Secret(row.APIKey),
		Group:     row.GroupName,
		Model:     row.Model,
		Weight:    row.Weight,
		RPM:       row.RPM,
		TPM:       row.TPM,
		RPS:       row.RPS,
	}
	if row.Capabilities != "" {
		if err := json.Unmarshal([]byte(row.Capabilities), &ep.Capabilities); err != nil {
			return nil, fmt.Errorf("endpoint: parse capabilities for %q: %w", row.ID, err)
		}
	}
	if row.Extra != "" {
		ep.Extra = json.RawMessage(row.Extra)
	}
	return ep, nil
}

// 编译期断言。
var (
	_ EndpointReader     = (*SQLEndpointRepo)(nil)
	_ EndpointWriter     = (*SQLEndpointRepo)(nil)
	_ EndpointRepository = (*SQLEndpointRepo)(nil)
)

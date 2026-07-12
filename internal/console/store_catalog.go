package console

import (
	"context"
	"time"
)

type AccountInput struct {
	Pin           string `json:"pin"`
	Name          string `json:"name"`
	QuotaPolicyID *int64 `json:"quota_policy_id,omitempty"`
}

func (s *Store) CreateAccount(ctx context.Context, in AccountInput) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO accounts (pin, name, quota_policy_id) VALUES (?, ?, ?)`,
		in.Pin, in.Name, in.QuotaPolicyID)

	return err
}

type AccountView struct {
	Pin           string    `db:"pin" json:"pin"`
	Name          string    `db:"name" json:"name"`
	Enabled       bool      `db:"enabled" json:"enabled"`
	QuotaPolicyID *int64    `db:"quota_policy_id" json:"quota_policy_id,omitempty"`
	CreatedAt     time.Time `db:"created_at" json:"created_at"`
	UpdatedAt     time.Time `db:"updated_at" json:"updated_at"`
}

func (s *Store) ListAccounts(ctx context.Context) ([]AccountView, error) {
	var rows []AccountView

	err := s.db.SelectContext(ctx, &rows,
		`SELECT pin, name, enabled, quota_policy_id, created_at, updated_at
		 FROM accounts WHERE deleted_at IS NULL ORDER BY created_at`)

	return rows, err
}

type ModelServiceInput struct {
	ServiceID string `json:"service_id"`
	Model     string `json:"model"`
}

func (s *Store) CreateModelService(ctx context.Context, in ModelServiceInput) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO model_services (service_id, model) VALUES (?, ?)`,
		in.ServiceID, in.Model)
	if err != nil {
		return 0, err
	}

	return res.LastInsertId()
}

type ModelServiceView struct {
	ID        int64     `db:"id" json:"id"`
	ServiceID string    `db:"service_id" json:"service_id"`
	Model     string    `db:"model" json:"model"`
	CreatedAt time.Time `db:"created_at" json:"created_at"`
}

func (s *Store) ListModelServices(ctx context.Context) ([]ModelServiceView, error) {
	var rows []ModelServiceView

	err := s.db.SelectContext(ctx, &rows,
		`SELECT id, service_id, model, created_at
		 FROM model_services WHERE deleted_at IS NULL ORDER BY id`)

	return rows, err
}

type SubscriptionInput struct {
	AccountID      string `json:"account_id"`
	ModelServiceID int64  `json:"model_service_id"`
}

func (s *Store) Subscribe(ctx context.Context, in SubscriptionInput) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO account_model_subscriptions (account_id, model_service_id, enabled)
		 VALUES (?, ?, 1)
		 ON DUPLICATE KEY UPDATE enabled = 1, deleted_at = NULL`,
		in.AccountID, in.ModelServiceID)

	return err
}

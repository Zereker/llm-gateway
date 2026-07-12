package console

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"time"

	"github.com/zereker/llm-gateway/internal/cachebus"
	"github.com/zereker/llm-gateway/internal/repo"
)

type APIKeyInput struct {
	AccountID     string `json:"account_id"`
	SubAccountID  string `json:"sub_account_id"`
	Name          string `json:"name,omitempty"`
	Group         string `json:"group,omitempty"`
	ExternalUser  bool   `json:"external_user,omitempty"`
	QuotaPolicyID *int64 `json:"quota_policy_id,omitempty"`
}

type APIKeyCreated struct {
	APIKeyID  string `json:"api_key_id"`
	Plaintext string `json:"api_key"`
	Prefix    string `json:"api_key_prefix"`
}

func (s *Store) CreateAPIKey(ctx context.Context, in APIKeyInput) (*APIKeyCreated, error) {
	plain, prefix, err := generateAPIKey()
	if err != nil {
		return nil, err
	}

	keyID, err := generateID("ak_")
	if err != nil {
		return nil, err
	}

	_, err = s.db.ExecContext(ctx,
		`INSERT INTO api_keys
		 (account_id, api_key_hash, api_key_prefix, api_key_id, name,
		  sub_account_id, group_name, external_user, enabled, quota_policy_id)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, 1, ?)`,
		orDefault(in.AccountID, "default"), repo.HashAPIKey(plain), prefix, keyID, in.Name,
		in.SubAccountID, orDefault(in.Group, "default"), in.ExternalUser, in.QuotaPolicyID)
	if err != nil {
		return nil, err
	}

	return &APIKeyCreated{APIKeyID: keyID, Plaintext: plain, Prefix: prefix}, nil
}

type APIKeyView struct {
	APIKeyID     string     `db:"api_key_id" json:"api_key_id"`
	AccountID    string     `db:"account_id" json:"account_id"`
	Prefix       string     `db:"api_key_prefix" json:"api_key_prefix"`
	Name         string     `db:"name" json:"name"`
	SubAccountID string     `db:"sub_account_id" json:"sub_account_id"`
	Enabled      bool       `db:"enabled" json:"enabled"`
	RevokedAt    *time.Time `db:"revoked_at" json:"revoked_at,omitempty"`
	LastUsedAt   *time.Time `db:"last_used_at" json:"last_used_at,omitempty"`
	CreatedAt    time.Time  `db:"created_at" json:"created_at"`
}

func (s *Store) ListAPIKeys(ctx context.Context, accountID string) ([]APIKeyView, error) {
	var rows []APIKeyView

	err := s.db.SelectContext(ctx, &rows,
		`SELECT api_key_id, account_id, api_key_prefix, name, sub_account_id,
		        enabled, revoked_at, last_used_at, created_at
		 FROM api_keys
		 WHERE account_id = ? AND deleted_at IS NULL ORDER BY created_at`,
		accountID)

	return rows, err
}

func (s *Store) RevokeAPIKey(ctx context.Context, accountID, apiKeyID string) error {
	var hash string

	err := s.db.GetContext(ctx, &hash,
		`SELECT api_key_hash FROM api_keys
		 WHERE account_id = ? AND api_key_id = ? AND deleted_at IS NULL`,
		accountID, apiKeyID)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}

	if err != nil {
		return err
	}

	res, err := s.db.ExecContext(ctx,
		`UPDATE api_keys SET revoked_at = NOW(6), enabled = 0
		 WHERE account_id = ? AND api_key_id = ? AND deleted_at IS NULL AND revoked_at IS NULL`,
		accountID, apiKeyID)
	if err != nil {
		return err
	}

	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}

	if s.pub != nil {
		if err := s.pub.Invalidate(ctx, cachebus.Invalidation{Kind: cachebus.KindAPIKey, Key: hash}); err != nil {
			slog.WarnContext(ctx, "cachebus invalidate failed; data plane will fall back to TTL", "err", err, "api_key_id", apiKeyID)
		}
	}

	return nil
}

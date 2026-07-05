package repo

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/zereker/llm-gateway/pkg/domain"
)

// SQLAPIKeyProvider is the MySQL implementation of IdentityProvider:
//
// **v0.3 change**: JOIN accounts to get both (api_keys.quota_policy_id,
// accounts.quota_policy_id) in one shot, so M6 RateLimit no longer needs
// 2 extra SELECTs.
//
// **v0.2 change**: the DB no longer stores the plaintext api_key — it stores
// the SHA-256 hash. Resolve hashes the input before looking it up.
//
// **v0.1 queries the DB directly, no cache** — every Resolve runs a SELECT.
// The api_key_hash column has a UNIQUE index, ~1ms.
type SQLAPIKeyProvider struct {
	db *sqlx.DB
}

// NewSQLAPIKeyProvider builds a provider that queries the DB directly; it does
// no startup-time load.
func NewSQLAPIKeyProvider(db *sqlx.DB) *SQLAPIKeyProvider {
	return &SQLAPIKeyProvider{db: db}
}

// resolveRow is the row shape after the JOIN; used only within this file.
type resolveRow struct {
	AccountID            string `db:"account_id"`
	SubAccountID         string `db:"sub_account_id"`
	APIKeyID             string `db:"api_key_id"`
	Group                string `db:"group_name"`
	ExternalUser         bool   `db:"external_user"`
	APIKeyQuotaPolicyID  *int64 `db:"apikey_quota_policy_id"`
	AccountQuotaPolicyID *int64 `db:"account_quota_policy_id"`
}

// Resolve implements IdentityProvider.Resolve.
//
// The SQL JOINs both quota_policy_id values in one go; M6 RateLimit consumes
// rc.Identity directly and doesn't need to query again.
//
// Query conditions:
//   - api_key_hash = SHA256(creds.APIKey) hex-encoded
//   - api_keys.enabled = 1, revoked_at IS NULL, deleted_at IS NULL
//   - expires_at IS NULL OR expires_at > NOW()
//   - accounts.enabled = 1, accounts.deleted_at IS NULL (an implicit pin-level disable)
//
// **Does not update last_used_at**: skipped in v0.3 (an INSERT/UPDATE on every
// request would double the write load); could later become an async batch
// update running in its own goroutine.
func (p *SQLAPIKeyProvider) Resolve(ctx context.Context, creds *Credentials) (*UserIdentity, error) {
	if creds == nil || creds.APIKey == "" {
		return nil, fmt.Errorf("apikey: missing api key: %w", domain.ErrInvalidCredentials)
	}

	hashed := HashAPIKey(creds.APIKey)

	var row resolveRow
	err := p.db.GetContext(ctx, &row, p.db.Rebind(
		`SELECT
		    a.account_id           AS account_id,
		    a.sub_account_id             AS sub_account_id,
		    a.api_key_id          AS api_key_id,
		    a.group_name          AS group_name,
		    a.external_user       AS external_user,
		    a.quota_policy_id     AS apikey_quota_policy_id,
		    t.quota_policy_id     AS account_quota_policy_id
		 FROM api_keys a
		 JOIN accounts t ON t.pin = a.account_id
		 WHERE a.api_key_hash = ?
		   AND a.enabled = 1
		   AND a.revoked_at IS NULL
		   AND a.deleted_at IS NULL
		   AND (a.expires_at IS NULL OR a.expires_at > ?)
		   AND t.enabled = 1
		   AND t.deleted_at IS NULL
		 LIMIT 1`),
		hashed, time.Now().UTC(),
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// unknown / disabled / expired / revoked / account disabled all map to
			// the ErrInvalidCredentials sentinel (M2 -> 401); kept coarse-grained
			// to avoid enabling enumeration.
			return nil, fmt.Errorf("apikey: %w", domain.ErrInvalidCredentials)
		}
		// SQL-layer failure (connection / timeout / schema, etc.) — not wrapped in
		// the sentinel; M2 fails closed -> 503.
		return nil, fmt.Errorf("apikey: lookup: %w", err)
	}

	return &UserIdentity{
		AccountID:            row.AccountID,
		SubAccountID:         row.SubAccountID,
		APIKeyID:             row.APIKeyID,
		Group:                row.Group,
		ExternalUser:         row.ExternalUser,
		AccountQuotaPolicyID: row.AccountQuotaPolicyID,
		APIKeyQuotaPolicyID:  row.APIKeyQuotaPolicyID,
	}, nil
}

// HashAPIKey SHA-256 hex-encodes the input; shared between the deployer's SQL
// INSERT hash computation and the gateway's Resolve.
func HashAPIKey(plain string) string {
	sum := sha256.Sum256([]byte(plain))
	return hex.EncodeToString(sum[:])
}

// Compile-time assertion.
var _ IdentityProvider = (*SQLAPIKeyProvider)(nil)

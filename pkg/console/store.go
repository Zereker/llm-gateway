package console

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/zereker/llm-gateway/pkg/cachebus"
	"github.com/zereker/llm-gateway/pkg/endpointcheck"
	"github.com/zereker/llm-gateway/pkg/ratelimit"
	"github.com/zereker/llm-gateway/pkg/repo"
)

// Store is the control plane's write/read layer, holding a *sqlx.DB directly.
//
// It reuses pkg/repo's typed structs + Scanner/Valuer (AuthConfig's
// transparent KEK encryption, NULL-safe rawJSON, HashAPIKey) — the bytes the
// control plane writes and the semantics the data plane reads are naturally
// aligned and cannot drift apart.
type Store struct {
	db  *sqlx.DB
	pub *cachebus.Publisher // optional; nil = rely solely on the data plane's TTL fallback
}

// NewStore constructs a Store.
func NewStore(db *sqlx.DB) *Store { return &Store{db: db} }

// WithPublisher attaches a cachebus Publisher so that revoking a key notifies
// the data plane precisely (bringing the <=TTL window down to sub-second).
// Falls back to plain TTL when nil.
func (s *Store) WithPublisher(p *cachebus.Publisher) *Store {
	s.pub = p
	return s
}

// ErrNotFound indicates the resource doesn't exist (translated to 404 by the handler).
var ErrNotFound = errors.New("not found")

// =============================================================================
// Accounts
// =============================================================================

// AccountInput is the input for creating a primary account.
type AccountInput struct {
	Pin           string `json:"pin"`
	Name          string `json:"name"`
	QuotaPolicyID *int64 `json:"quota_policy_id,omitempty"`
}

// CreateAccount creates a primary account.
func (s *Store) CreateAccount(ctx context.Context, in AccountInput) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO accounts (pin, name, quota_policy_id) VALUES (?, ?, ?)`,
		in.Pin, in.Name, in.QuotaPolicyID)
	return err
}

// AccountView is a read-only view of a primary account.
type AccountView struct {
	Pin           string     `db:"pin" json:"pin"`
	Name          string     `db:"name" json:"name"`
	Enabled       bool       `db:"enabled" json:"enabled"`
	QuotaPolicyID *int64     `db:"quota_policy_id" json:"quota_policy_id,omitempty"`
	CreatedAt     time.Time  `db:"created_at" json:"created_at"`
	UpdatedAt     time.Time  `db:"updated_at" json:"updated_at"`
}

// ListAccounts lists all non-deleted primary accounts.
func (s *Store) ListAccounts(ctx context.Context) ([]AccountView, error) {
	var rows []AccountView
	err := s.db.SelectContext(ctx, &rows,
		`SELECT pin, name, enabled, quota_policy_id, created_at, updated_at
		 FROM accounts WHERE deleted_at IS NULL ORDER BY created_at`)
	return rows, err
}

// =============================================================================
// Model services + subscriptions
// =============================================================================

// ModelServiceInput is the input for creating a model catalog entry.
type ModelServiceInput struct {
	ServiceID string `json:"service_id"`
	Model     string `json:"model"`
}

// CreateModelService creates a global model catalog entry, returning the auto-increment id.
func (s *Store) CreateModelService(ctx context.Context, in ModelServiceInput) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO model_services (service_id, model) VALUES (?, ?)`,
		in.ServiceID, in.Model)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// ModelServiceView is a read-only view of a model catalog entry.
type ModelServiceView struct {
	ID        int64     `db:"id" json:"id"`
	ServiceID string    `db:"service_id" json:"service_id"`
	Model     string    `db:"model" json:"model"`
	CreatedAt time.Time `db:"created_at" json:"created_at"`
}

// ListModelServices lists all non-deleted models.
func (s *Store) ListModelServices(ctx context.Context) ([]ModelServiceView, error) {
	var rows []ModelServiceView
	err := s.db.SelectContext(ctx, &rows,
		`SELECT id, service_id, model, created_at
		 FROM model_services WHERE deleted_at IS NULL ORDER BY id`)
	return rows, err
}

// SubscriptionInput is the input for a primary account subscribing to a model.
type SubscriptionInput struct {
	AccountID      string `json:"account_id"`
	ModelServiceID int64  `json:"model_service_id"`
}

// Subscribe makes a primary account subscribe to a model (M5 visibility).
// Idempotent: if it already exists, this just updates enabled=1.
func (s *Store) Subscribe(ctx context.Context, in SubscriptionInput) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO account_model_subscriptions (account_id, model_service_id, enabled)
		 VALUES (?, ?, 1)
		 ON DUPLICATE KEY UPDATE enabled = 1, deleted_at = NULL`,
		in.AccountID, in.ModelServiceID)
	return err
}

// =============================================================================
// Endpoints
// =============================================================================

// EndpointInput is the input for creating an upstream endpoint. auth.payload
// carries the secret — before being written it goes through
// repo.AuthConfig.Value() for AES-256-GCM encryption (the same KEK as the
// data plane).
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

// AuthInput is the endpoint credential input (plaintext; encrypted on write).
type AuthInput struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

// InvalidEndpointError is a pre-write validation failure (the reasons from endpointcheck.Validate).
type InvalidEndpointError struct {
	Reasons []string
}

func (e *InvalidEndpointError) Error() string {
	return fmt.Sprintf("endpoint invalid: %v", e.Reasons)
}

// CreateEndpoint validates + encrypts + inserts, returning the auto-increment id.
//
// It runs endpointcheck.Validate before writing (the same logic as the data
// plane's startup scan) — a protocol typo, an unregistered vendor, an
// unreachable translator, routing pointed at metadata, or a quirks compile
// failure gets rejected (400) right here at the API layer, instead of only
// warning at the data plane's startup scan.
func (s *Store) CreateEndpoint(ctx context.Context, in EndpointInput) (int64, error) {
	auth, err := repo.EncodePayload(in.Auth.Type, in.Auth.Payload)
	if err != nil {
		return 0, &InvalidEndpointError{Reasons: []string{"invalid_auth: " + err.Error()}}
	}

	ep := &repo.Endpoint{
		Name:     in.Name,
		Vendor:   in.Vendor,
		Protocol: in.Protocol,
		Model:    in.Model,
		Group:    orDefault(in.Group, "default"),
		Weight:   orWeight(in.Weight, 100),
		Enabled:  in.Enabled == nil || *in.Enabled,
		Auth:     auth,
		Routing:  in.Routing,
		Quirks:   rawOrNil(in.Quirks),
		Extra:    rawOrNil(in.Extra),
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

	// Pre-write business validation (domain view; auth is not part of validation).
	if reasons := endpointcheck.Validate(repo.ToDomainEndpoint(ep)); len(reasons) > 0 {
		return 0, &InvalidEndpointError{Reasons: reasons}
	}

	res, err := s.db.NamedExecContext(ctx,
		`INSERT INTO endpoints
		 (name, vendor, protocol, model, group_name, weight, enabled,
		  auth, routing, quota, capabilities, quirks, extra)
		 VALUES
		 (:name, :vendor, :protocol, :model, :group_name, :weight, :enabled,
		  :auth, :routing, :quota, :capabilities, :quirks, :extra)`,
		ep)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// EndpointView is a read-only view of an endpoint — **deliberately excludes
// the auth payload** (only auth.type is returned); the secret never leaves
// the API.
type EndpointView struct {
	ID        int64    `json:"id"`
	Name      string   `json:"name"`
	Vendor    string   `json:"vendor"`
	Protocol  string   `json:"protocol"`
	Model     string   `json:"model"`
	Group     string   `json:"group"`
	Weight    uint32   `json:"weight"`
	Enabled   bool     `json:"enabled"`
	AuthType  string   `json:"auth_type"`
	RoutingURL string  `json:"routing_url,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

func endpointToView(e *repo.Endpoint) EndpointView {
	return EndpointView{
		ID:         e.ID,
		Name:       e.Name,
		Vendor:     e.Vendor,
		Protocol:   e.Protocol,
		Model:      e.Model,
		Group:      e.Group,
		Weight:     e.Weight,
		Enabled:    e.Enabled,
		AuthType:   e.Auth.Type,
		RoutingURL: e.Routing.URL,
		CreatedAt:  e.CreatedAt,
	}
}

const epSelectColumns = `id, name, vendor, protocol, model, group_name, weight, enabled,
	auth, routing, quota, capabilities, quirks, extra, created_at, updated_at, deleted_at`

// ListEndpoints lists all non-deleted endpoints (redacted view).
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

// GetEndpoint fetches a single endpoint (redacted view).
func (s *Store) GetEndpoint(ctx context.Context, id int64) (*EndpointView, error) {
	var ep repo.Endpoint
	err := s.db.GetContext(ctx, &ep,
		`SELECT `+epSelectColumns+` FROM endpoints WHERE id = ? AND deleted_at IS NULL`, id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	v := endpointToView(&ep)
	return &v, nil
}

// DeleteEndpoint soft-deletes an endpoint (sets deleted_at).
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

// =============================================================================
// API keys
// =============================================================================

// APIKeyInput is the input for issuing a key. The plaintext is generated
// server-side and echoed back only once.
type APIKeyInput struct {
	AccountID     string `json:"account_id"`
	SubAccountID  string `json:"sub_account_id"`
	Name          string `json:"name,omitempty"`
	Group         string `json:"group,omitempty"`
	ExternalUser  bool   `json:"external_user,omitempty"`
	QuotaPolicyID *int64 `json:"quota_policy_id,omitempty"`
}

// APIKeyCreated is the result of issuing a key — **the plaintext is returned
// only this once**; afterwards the DB only stores the hash.
type APIKeyCreated struct {
	APIKeyID  string `json:"api_key_id"`
	Plaintext string `json:"api_key"`
	Prefix    string `json:"api_key_prefix"`
}

// CreateAPIKey generates a random key -> stores its SHA-256 hash -> returns the plaintext once.
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

// APIKeyView is a read-only view of a key — **never includes the hash or
// plaintext**, only the prefix and metadata.
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

// ListAPIKeys lists the keys under a primary account (redacted).
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

// RevokeAPIKey revokes a key: sets revoked_at + enabled=0, and notifies the
// data plane to evict precisely via cachebus (bringing the "still cached as
// valid after revoke" window down from <=30s TTL to sub-second).
//
// The hash is fetched before the UPDATE: the hash is the data plane's cache
// key, so the control plane can notify invalidation just by holding it, with
// no need for the plaintext. Publishing is best-effort — if Redis is down it
// only logs a warning; the DB write plus TTL fallback still gives eventual
// consistency.
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
		return ErrNotFound // already revoked
	}

	if s.pub != nil {
		if perr := s.pub.Invalidate(ctx, cachebus.Invalidation{Kind: cachebus.KindAPIKey, Key: hash}); perr != nil {
			slog.WarnContext(ctx, "cachebus invalidate failed; data plane will fall back to TTL", "err", perr, "api_key_id", apiKeyID)
		}
	}
	return nil
}

// =============================================================================
// Model aliases (alias -> canonical model redirection)
// =============================================================================

// ModelAliasInput is the input for creating an alias.
type ModelAliasInput struct {
	Alias string `json:"alias"`
	Model string `json:"model"` // canonical model_services.model
}

// InvalidAliasError indicates an invalid alias input (e.g. the canonical model doesn't exist).
type InvalidAliasError struct{ Reason string }

func (e *InvalidAliasError) Error() string { return "model alias invalid: " + e.Reason }

// CreateModelAlias creates an alias. Validates the canonical model exists
// before writing (to avoid creating a dead alias).
func (s *Store) CreateModelAlias(ctx context.Context, in ModelAliasInput) error {
	if in.Alias == "" || in.Model == "" {
		return &InvalidAliasError{Reason: "alias and model required"}
	}
	var n int
	if err := s.db.GetContext(ctx, &n,
		`SELECT COUNT(*) FROM model_services WHERE model = ? AND deleted_at IS NULL`, in.Model); err != nil {
		return err
	}
	if n == 0 {
		return &InvalidAliasError{Reason: "canonical model does not exist: " + in.Model}
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO model_aliases (alias, model) VALUES (?, ?)`, in.Alias, in.Model)
	return err
}

// ModelAliasView is a read-only view of an alias.
type ModelAliasView struct {
	Alias     string    `db:"alias" json:"alias"`
	Model     string    `db:"model" json:"model"`
	Enabled   bool      `db:"enabled" json:"enabled"`
	CreatedAt time.Time `db:"created_at" json:"created_at"`
}

// ListModelAliases lists all non-deleted aliases.
func (s *Store) ListModelAliases(ctx context.Context) ([]ModelAliasView, error) {
	var rows []ModelAliasView
	err := s.db.SelectContext(ctx, &rows,
		`SELECT alias, model, enabled, created_at
		 FROM model_aliases WHERE deleted_at IS NULL ORDER BY alias`)
	return rows, err
}

// DeleteModelAlias hard-deletes an alias (an alias is a pure redirect with no
// historical value; hard delete avoids the PK becoming unreusable).
func (s *Store) DeleteModelAlias(ctx context.Context, alias string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM model_aliases WHERE alias = ?`, alias)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// =============================================================================
// Quota policies (rate-limit policy library; referenced by accounts / api_keys)
// =============================================================================

// QuotaPolicyInput is the input for creating a rate-limit policy. Rule takes
// the shape of ratelimit.PolicyRule ({default:{rpm,tpm,rps,...},
// per_model:{...}}); it is validated for parseability before writing.
type QuotaPolicyInput struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Rule        json.RawMessage `json:"rule"`
}

// InvalidPolicyError indicates rule_json validation failed.
type InvalidPolicyError struct{ Reason string }

func (e *InvalidPolicyError) Error() string { return "quota policy invalid: " + e.Reason }

// CreateQuotaPolicy validates the shape of rule_json then inserts, returning the auto-increment id.
func (s *Store) CreateQuotaPolicy(ctx context.Context, in QuotaPolicyInput) (int64, error) {
	if in.Name == "" {
		return 0, &InvalidPolicyError{Reason: "name required"}
	}
	if len(in.Rule) == 0 {
		return 0, &InvalidPolicyError{Reason: "rule required"}
	}
	// Validate: must parse as a PolicyRule, and have at least default or
	// per_model (an empty policy is meaningless).
	var pr ratelimit.PolicyRule
	if err := json.Unmarshal(in.Rule, &pr); err != nil {
		return 0, &InvalidPolicyError{Reason: "rule not a valid PolicyRule: " + err.Error()}
	}
	if pr.Default == nil && len(pr.PerModel) == 0 {
		return 0, &InvalidPolicyError{Reason: "rule has neither default nor per_model"}
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO quota_policies (name, description, rule_json) VALUES (?, ?, ?)`,
		in.Name, in.Description, []byte(in.Rule))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// QuotaPolicyView is a read-only view of a rate-limit policy.
type QuotaPolicyView struct {
	ID          int64           `db:"id" json:"id"`
	Name        string          `db:"name" json:"name"`
	Description string          `db:"description" json:"description"`
	RuleJSON    json.RawMessage `db:"rule_json" json:"rule"`
	Enabled     bool            `db:"enabled" json:"enabled"`
	CreatedAt   time.Time       `db:"created_at" json:"created_at"`
}

// ListQuotaPolicies lists all non-deleted policies.
func (s *Store) ListQuotaPolicies(ctx context.Context) ([]QuotaPolicyView, error) {
	var rows []QuotaPolicyView
	err := s.db.SelectContext(ctx, &rows,
		`SELECT id, name, description, rule_json, enabled, created_at
		 FROM quota_policies WHERE deleted_at IS NULL ORDER BY id`)
	return rows, err
}

// DeleteQuotaPolicy soft-deletes a policy. Note: after soft-deleting a policy
// referenced by accounts/api_keys, the data plane can still read it by id
// (the row is still there) — to actually deactivate it, change the
// referencing side's quota_policy_id to NULL instead.
func (s *Store) DeleteQuotaPolicy(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE quota_policies SET deleted_at = NOW(6) WHERE id = ? AND deleted_at IS NULL`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// =============================================================================
// Pricing (append-only price versions; changing a price = closing out the
// old one + inserting a new one)
// =============================================================================

// PricingInput is the input for publishing a new price version. An empty
// RuleClass means "standard".
//
// **rule_json is opaque to the gateway**: the billing engine defines its own
// schema, which the gateway does not parse (docs/05 §6) — so this only
// validates that it is legal JSON, without imposing a pricing shape (to avoid
// pulling the billing domain into the gateway).
type PricingInput struct {
	AccountID      string          `json:"account_id"`
	ModelServiceID int64           `json:"model_service_id"`
	RuleClass      string          `json:"rule_class,omitempty"`
	Rule           json.RawMessage `json:"rule"`
	CreatedBy      string          `json:"created_by,omitempty"`
	Notes          string          `json:"notes,omitempty"`
}

// InvalidPricingError indicates an invalid pricing input.
type InvalidPricingError struct{ Reason string }

func (e *InvalidPricingError) Error() string { return "pricing invalid: " + e.Reason }

// PublishPrice publishes append-only: within a single transaction, it closes
// out the current active row (effective_to=NOW) and inserts a new row
// (effective_from=NOW, effective_to=NULL). It never UPDATEs rule_json.
func (s *Store) PublishPrice(ctx context.Context, in PricingInput) (int64, error) {
	if in.AccountID == "" || in.ModelServiceID == 0 {
		return 0, &InvalidPricingError{Reason: "account_id and model_service_id required"}
	}
	if !json.Valid(in.Rule) || len(in.Rule) == 0 {
		return 0, &InvalidPricingError{Reason: "rule must be non-empty valid JSON"}
	}
	class := orDefault(in.RuleClass, "standard")

	// **Concurrency serialization**: two concurrent PublishPrice calls
	// (especially on the first publish, when there's no existing active row
	// for FOR UPDATE to lock) would each "close out 0 rows + insert one row",
	// leaving two active rows (effective_to IS NULL) and making billing's
	// "current price" query nondeterministic. We use a dedicated connection +
	// a MySQL advisory lock to serialize publishes for the same
	// (account,model,class). The lock is held for the connection's lifetime —
	// Connx grabs a dedicated connection, and after commit we explicitly
	// RELEASE before returning the connection to the pool (otherwise the lock
	// would leak onto a reused connection).
	conn, err := s.db.Connx(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = conn.Close() }()

	lockName := fmt.Sprintf("llmgw:pricing:%s:%d:%s", in.AccountID, in.ModelServiceID, class)
	var locked int
	if err := conn.GetContext(ctx, &locked, "SELECT GET_LOCK(?, 10)", lockName); err != nil {
		return 0, fmt.Errorf("pricing: acquire lock: %w", err)
	}
	if locked != 1 {
		return 0, fmt.Errorf("pricing: could not acquire publish lock (timeout)")
	}
	// RELEASE runs before Close (defer is LIFO: registered later, runs
	// first) — released after commit, never earlier.
	defer func() { _, _ = conn.ExecContext(context.WithoutCancel(ctx), "DO RELEASE_LOCK(?)", lockName) }()

	tx, err := conn.BeginTxx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	// 1) Close out the current active row (same account+model+class, effective_to IS NULL)
	if _, err := tx.ExecContext(ctx,
		`UPDATE pricing_versions SET effective_to = NOW(6)
		 WHERE account_id = ? AND model_service_id = ? AND rule_class = ? AND effective_to IS NULL`,
		in.AccountID, in.ModelServiceID, class); err != nil {
		return 0, fmt.Errorf("pricing: close active: %w", err)
	}
	// 2) Insert the new active row
	res, err := tx.ExecContext(ctx,
		`INSERT INTO pricing_versions
		 (account_id, model_service_id, rule_class, effective_from, effective_to, rule_json, created_by, notes)
		 VALUES (?, ?, ?, NOW(6), NULL, ?, ?, ?)`,
		in.AccountID, in.ModelServiceID, class, []byte(in.Rule), in.CreatedBy, in.Notes)
	if err != nil {
		return 0, fmt.Errorf("pricing: insert: %w", err)
	}
	id, _ := res.LastInsertId()
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("pricing: commit: %w", err)
	}
	return id, nil
}

// PricingView is a read-only view of a price version.
type PricingView struct {
	ID             int64           `db:"id" json:"id"`
	AccountID      string          `db:"account_id" json:"account_id"`
	ModelServiceID int64           `db:"model_service_id" json:"model_service_id"`
	RuleClass      string          `db:"rule_class" json:"rule_class"`
	EffectiveFrom  time.Time       `db:"effective_from" json:"effective_from"`
	EffectiveTo    *time.Time      `db:"effective_to" json:"effective_to,omitempty"`
	RuleJSON       json.RawMessage `db:"rule_json" json:"rule"`
	CreatedBy      string          `db:"created_by" json:"created_by"`
	Notes          string          `db:"notes" json:"notes"`
}

// PricingQuery filters: an empty/0 AccountID / ModelServiceID means no
// filter; ActiveOnly means only rows that haven't been closed out.
type PricingQuery struct {
	AccountID      string
	ModelServiceID int64
	ActiveOnly     bool
}

// ListPricing lists price versions (active + historical; ordered by effective_from descending).
func (s *Store) ListPricing(ctx context.Context, q PricingQuery) ([]PricingView, error) {
	sqlStr := `SELECT id, account_id, model_service_id, rule_class, effective_from, effective_to,
	                  rule_json, created_by, notes
	           FROM pricing_versions WHERE 1=1`
	var args []any
	if q.AccountID != "" {
		sqlStr += ` AND account_id = ?`
		args = append(args, q.AccountID)
	}
	if q.ModelServiceID != 0 {
		sqlStr += ` AND model_service_id = ?`
		args = append(args, q.ModelServiceID)
	}
	if q.ActiveOnly {
		sqlStr += ` AND effective_to IS NULL`
	}
	sqlStr += ` ORDER BY effective_from DESC, id DESC`

	var rows []PricingView
	err := s.db.SelectContext(ctx, &rows, sqlStr, args...)
	return rows, err
}

// =============================================================================
// Audit log (control-plane write-operation audit)
// =============================================================================

// RecordAudit records an audit entry (best-effort; the caller swallows
// errors and only warns). The request body is deliberately excluded.
func (s *Store) RecordAudit(ctx context.Context, actor, role, method, path string, status int) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO audit_log (actor, role, method, path, status_code) VALUES (?, ?, ?, ?, ?)`,
		actor, role, method, path, status)
	return err
}

// AuditView is a read-only view of an audit entry.
type AuditView struct {
	ID         int64     `db:"id" json:"id"`
	Actor      string    `db:"actor" json:"actor"`
	Role       string    `db:"role" json:"role"`
	Method     string    `db:"method" json:"method"`
	Path       string    `db:"path" json:"path"`
	StatusCode int       `db:"status_code" json:"status_code"`
	CreatedAt  time.Time `db:"created_at" json:"created_at"`
}

// ListAudit lists the most recent limit audit entries (newest first).
func (s *Store) ListAudit(ctx context.Context, limit int) ([]AuditView, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	var rows []AuditView
	err := s.db.SelectContext(ctx, &rows,
		`SELECT id, actor, role, method, path, status_code, created_at
		 FROM audit_log ORDER BY id DESC LIMIT ?`, limit)
	return rows, err
}

// =============================================================================
// helpers
// =============================================================================

// generateAPIKey generates "sk-" + a 32-byte url-safe base64 random body, returning the plaintext + prefix.
func generateAPIKey() (plain, prefix string, err error) {
	buf := make([]byte, 24)
	if _, err = rand.Read(buf); err != nil {
		return "", "", fmt.Errorf("console: gen api key: %w", err)
	}
	plain = "sk-" + base64.RawURLEncoding.EncodeToString(buf)
	prefix = plain
	if len(prefix) > 12 {
		prefix = prefix[:12]
	}
	return plain, prefix, nil
}

// generateID generates prefix + 8 bytes of url-safe base64 (a stable ID for audit purposes).
func generateID(prefix string) (string, error) {
	buf := make([]byte, 6)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("console: gen id: %w", err)
	}
	return prefix + base64.RawURLEncoding.EncodeToString(buf), nil
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func orWeight(w, def uint32) uint32 {
	if w == 0 {
		return def
	}
	return w
}

func rawOrNil(r json.RawMessage) []byte {
	if len(r) == 0 {
		return nil
	}
	return []byte(r)
}

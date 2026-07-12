package console

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/zereker/llm-gateway/internal/cachebus"
	"github.com/zereker/llm-gateway/internal/endpointcheck"
)

// Store is the control plane's write/read layer, holding a *sqlx.DB directly.
//
// It reuses internal/repo's typed structs + Scanner/Valuer (AuthConfig's
// transparent KEK encryption, NULL-safe rawJSON, HashAPIKey) — the bytes the
// control plane writes and the semantics the data plane reads are naturally
// aligned and cannot drift apart.
type Store struct {
	db                *sqlx.DB
	pub               *cachebus.Publisher // optional; nil = rely solely on the data plane's TTL fallback
	endpointValidator endpointcheck.Validator
}

// NewStore constructs a Store.
func NewStore(db *sqlx.DB) *Store { return &Store{db: db} }

// WithEndpointValidator supplies the protocol capability catalog used before
// endpoint writes.
func (s *Store) WithEndpointValidator(v endpointcheck.Validator) *Store {
	s.endpointValidator = v
	return s
}

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

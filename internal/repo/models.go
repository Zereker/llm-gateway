package repo

import (
	"database/sql/driver"
	"encoding/json"
	"errors"
	"time"

	"github.com/zereker/llm-gateway/internal/domain"
)

// models.go defines the "business entities" — used by the gateway (sqlx
// Reader); writes are maintained by the deployer via SQL.
//
// **The schema of record lives in internal/infra/schema.sql**; the `db:` tags here
// only describe column names for sqlx. No gorm tags — the gateway is a
// read-only data plane, doesn't use AutoMigrate, and DDL evolves via SQL only.
//
// JSON columns are handled two ways:
//   - **known structure** (EndpointCapabilities / AuthConfig / RoutingConfig /
//     QuotaConfig): a typed struct + custom Scanner/Valuer, callers read
//     fields directly, JSON (de)serialization is transparent on the DB side
//   - **unknown / extensible structure** (rule_json / Extra): json.RawMessage —
//     bytes passed through, the gateway doesn't parse them
//
// **The standard trio of audit fields**:
//   - CreatedAt / UpdatedAt / DeletedAt (soft-delete pointer)
//   - After a soft delete, the same UNIQUE key can't be reused directly; needs a hard delete

// =============================================================================
// Account: primary account pin / billing entity metadata
// =============================================================================

// Account maps to table accounts; its business meaning is a primary account / billing entity.
//
// **pin is the primary key directly**: business key = identity key, no BIGINT
// surrogate is introduced. Other tables' account_id VARCHAR(64) column is
// exactly this pin, FK -> accounts.pin.
//
// QuotaPolicyID NULL = no rate limiting at the account level (M6 skips the
// account-level check).
type Account struct {
	Pin           string `db:"pin"`
	Name          string `db:"name"`
	Enabled       bool   `db:"enabled"`
	QuotaPolicyID *int64 `db:"quota_policy_id"`

	CreatedAt time.Time  `db:"created_at"`
	UpdatedAt time.Time  `db:"updated_at"`
	DeletedAt *time.Time `db:"deleted_at"`
}

// =============================================================================
// QuotaPolicy: the rate-limit policy library (referenced by accounts / api_keys, shared N:M)
// =============================================================================

// QuotaPolicy maps to table quota_policies.
//
// rule_json shape:
//
//	{
//	  "default":   {"rpm":60, "tpm":100000, "rps":null, "concurrent_requests":null},
//	  "per_model": {"gpt-4o":{"rpm":10}, "gpt-4o-mini":{"rpm":100}}
//	}
//
// The gateway doesn't parse rule_json; M6 RateLimit is the sole consumer:
// tries per_model[currentModel] first, falls back to default, and if neither
// exists that layer isn't limited.
type QuotaPolicy struct {
	ID          int64           `db:"id"`
	Name        string          `db:"name"`
	Description string          `db:"description"`
	RuleJSON    json.RawMessage `db:"rule_json"`
	Enabled     bool            `db:"enabled"`

	CreatedAt time.Time  `db:"created_at"`
	UpdatedAt time.Time  `db:"updated_at"`
	DeletedAt *time.Time `db:"deleted_at"`
}

// =============================================================================
// ModelService: the global model catalog
// =============================================================================

// ModelService maps to table model_services.
//
// **v0.3 change**: dropped account_id (became a global catalog) / group_name /
// spec_detail. Model visibility now goes through account_model_subscriptions;
// group is an endpoint-level dimension.
type ModelService struct {
	ID        int64  `db:"id"`
	ServiceID string `db:"service_id"`
	Model     string `db:"model"`

	CreatedAt time.Time  `db:"created_at"`
	UpdatedAt time.Time  `db:"updated_at"`
	DeletedAt *time.Time `db:"deleted_at"`
}

// =============================================================================
// AccountModelSubscription: account x model visibility N:M
// =============================================================================

// AccountModelSubscription maps to table account_model_subscriptions.
//
// After M5 confirms the model is in the catalog, it queries this table by
// (account pin, model_service_id); not found -> 403 "model not subscribed".
type AccountModelSubscription struct {
	ID             int64  `db:"id"`
	AccountID      string `db:"account_id"`
	ModelServiceID int64  `db:"model_service_id"`
	Enabled        bool   `db:"enabled"`

	CreatedAt time.Time  `db:"created_at"`
	UpdatedAt time.Time  `db:"updated_at"`
	DeletedAt *time.Time `db:"deleted_at"`
}

// =============================================================================
// Endpoint: global upstream access point
// =============================================================================

// Endpoint maps to table endpoints.
//
// **v0.3 change**: dropped account_id (became global; add a nullable
// account_id if BYOK etc. is actually needed later).
//
// Core columns hold only what the scheduling/routing hot path needs;
// everything vendor-specific goes into typed JSON.
type Endpoint struct {
	ID       int64  `db:"id"`
	Name     string `db:"name"`
	Vendor   string `db:"vendor"`
	Protocol string `db:"protocol"` // domain.Protocol.String() -- converted back and forth by mappers
	Model    string `db:"model"`
	Group    string `db:"group_name"`
	Weight   uint32 `db:"weight"`
	Enabled  bool   `db:"enabled"`

	// The three typed-JSON columns; Scanner/Valuer live in their own files
	Auth         AuthConfig           `db:"auth"`
	Routing      RoutingConfig        `db:"routing"`
	Quota        QuotaConfig          `db:"quota"`
	Capabilities EndpointCapabilities `db:"capabilities"`
	// quirks / extra are DEFAULT NULL columns, so they use rawJSON (a
	// NULL-safe Scanner) instead of bare json.RawMessage — database/sql can't
	// scan a SQL NULL into json.RawMessage, so an endpoint with no quirks
	// configured would fail with "unsupported Scan" on read.
	Quirks rawJSON `db:"quirks"` // v0.7: the internal/protocol/quirks DSL; NULL -> no-op
	Extra  rawJSON `db:"extra"`

	CreatedAt time.Time  `db:"created_at"`
	UpdatedAt time.Time  `db:"updated_at"`
	DeletedAt *time.Time `db:"deleted_at"`
}

// EndpointCapabilities is the known-structure capability flags; serialized as
// JSON into the endpoints.capabilities column.
//
// Shares its shape with domain.EndpointCapabilities (the mapper does a
// straight type conversion). Add new fields to both sides in sync.
type EndpointCapabilities struct {
	Modalities          []domain.Modality `json:"modalities,omitempty"`
	SelfHosted          bool              `json:"self_hosted,omitempty"`
	KVMetricEndpoint    string            `json:"kv_metric_endpoint,omitempty"`
	HealthProbeEndpoint string            `json:"health_probe_endpoint,omitempty"`
	PrefixCacheEnabled  bool              `json:"prefix_cache_enabled,omitempty"`
}

// isEmpty substitutes for == comparison (a struct with a slice field isn't comparable with ==).
func (c EndpointCapabilities) isEmpty() bool {
	return len(c.Modalities) == 0 && !c.SelfHosted &&
		c.KVMetricEndpoint == "" && c.HealthProbeEndpoint == "" && !c.PrefixCacheEnabled
}

// Scan implements sql.Scanner: deserializes from the DB's JSON bytes.
func (c *EndpointCapabilities) Scan(value any) error {
	if value == nil {
		*c = EndpointCapabilities{}
		return nil
	}
	b, err := bytesFromScan(value, "EndpointCapabilities")
	if err != nil {
		return err
	}
	if len(b) == 0 {
		*c = EndpointCapabilities{}
		return nil
	}
	return json.Unmarshal(b, c)
}

// Value implements driver.Valuer: marshals to JSON; the zero value writes NULL.
func (c EndpointCapabilities) Value() (driver.Value, error) {
	if c.isEmpty() {
		return nil, nil
	}
	return json.Marshal(c)
}

// =============================================================================
// APIKey
// =============================================================================

// APIKey maps to table api_keys.
//
// **v0.3 change**: added the quota_policy_id column (API-key-level rate
// limiting; stacks with the account-level quota).
//
// The DB never stores plaintext: the server generates sk-XXX -> SHA-256 ->
// stores api_key_hash.
type APIKey struct {
	ID            int64      `db:"id"`
	AccountID     string     `db:"account_id"` // primary account pin / billing entity
	APIKeyHash    string     `db:"api_key_hash"`
	APIKeyPrefix  string     `db:"api_key_prefix"`
	APIKeyID      string     `db:"api_key_id"`
	Name          string     `db:"name"`
	SubAccountID  string     `db:"sub_account_id"` // sub-account / operator
	Group         string     `db:"group_name"`
	ExternalUser  bool       `db:"external_user"`
	Enabled       bool       `db:"enabled"`
	ExpiresAt     *time.Time `db:"expires_at"`
	LastUsedAt    *time.Time `db:"last_used_at"`
	RevokedAt     *time.Time `db:"revoked_at"`
	QuotaPolicyID *int64     `db:"quota_policy_id"`

	CreatedAt time.Time  `db:"created_at"`
	UpdatedAt time.Time  `db:"updated_at"`
	DeletedAt *time.Time `db:"deleted_at"`
}

// ToUserIdentity maps a DB row into the UserIdentity that M2 Auth hands to
// subsequent middleware.
//
// **Does not include AccountQuotaPolicyID**: that field can only come from a
// JOIN with accounts; a single APIKey row doesn't have it.
// SQLAPIKeyProvider.Resolve constructs the full UserIdentity directly (with
// AccountQuotaPolicyID) and does not go through this method.
func (a APIKey) ToUserIdentity() UserIdentity {
	return UserIdentity{
		AccountID:           a.AccountID,
		SubAccountID:        a.SubAccountID,
		APIKeyID:            a.APIKeyID,
		Group:               a.Group,
		ExternalUser:        a.ExternalUser,
		APIKeyQuotaPolicyID: a.QuotaPolicyID,
	}
}

// =============================================================================
// PricingVersion: append-only pricing versions
// =============================================================================

// PricingVersion maps to table pricing_versions.
//
// **append-only**: once published, rule_json is never UPDATEd; changing a
// price = one transaction that
//
//  1. UPDATEs the current active row's effective_to = NOW()
//  2. INSERTs a new row with effective_from = NOW(), effective_to = NULL
//
// The gateway only reads: M5 GetActive fetches the current version, and
// rc.Pricing's snapshot references the ID. The gateway does not unmarshal
// rule_json — the billing engine defines its own schema.
type PricingVersion struct {
	ID             int64           `db:"id"`
	AccountID      string          `db:"account_id"`
	ModelServiceID int64           `db:"model_service_id"`
	RuleClass      string          `db:"rule_class"`
	EffectiveFrom  time.Time       `db:"effective_from"`
	EffectiveTo    *time.Time      `db:"effective_to"`
	RuleJSON       json.RawMessage `db:"rule_json"`
	CreatedAt      time.Time       `db:"created_at"`
	CreatedBy      string          `db:"created_by"`
	Notes          string          `db:"notes"`
}

// =============================================================================
// EndpointForm helper
// =============================================================================

// EndpointForm is derived from Capabilities (retained from the original domain helper).
type EndpointForm int

const (
	FormVendor EndpointForm = iota
	FormSelfHosted
)

func (f EndpointForm) String() string {
	if f == FormSelfHosted {
		return "self_hosted"
	}
	return "vendor"
}

// Form is a derived method.
func (e *Endpoint) Form() EndpointForm {
	if e.Capabilities.SelfHosted {
		return FormSelfHosted
	}
	return FormVendor
}

// rawJSON is the NULL-safe carrier type for nullable JSON columns (quirks / extra).
//
// **Why not bare json.RawMessage**: database/sql's convertAssign has no
// fallback branch for "SQL NULL -> json.RawMessage (a named []byte type)";
// the reflection fallback hits nil->slice and just returns "unsupported Scan,
// storing driver.Value type <nil> into type *json.RawMessage". The bare type
// only survives when the column is guaranteed non-NULL; quirks / extra are
// both DEFAULT NULL, so an endpoint the deployer didn't configure them for is
// NULL, and a single SELECT blows up — the whole endpoint can't be read,
// which effectively makes that endpoint unusable. Here NULL / empty is
// explicitly normalized to nil.
type rawJSON []byte

// Scan implements sql.Scanner: NULL / empty -> nil; otherwise copies the
// bytes (the driver's []byte buffer may be reused on the next rows.Next(), so
// it can't be held onto directly).
func (r *rawJSON) Scan(value any) error {
	if value == nil {
		*r = nil
		return nil
	}
	b, err := bytesFromScan(value, "rawJSON")
	if err != nil {
		return err
	}
	if len(b) == 0 {
		*r = nil
		return nil
	}
	cp := make([]byte, len(b))
	copy(cp, b)
	*r = cp
	return nil
}

// Value implements driver.Valuer: empty -> NULL; otherwise writes the bytes as-is.
func (r rawJSON) Value() (driver.Value, error) {
	if len(r) == 0 {
		return nil, nil
	}
	return []byte(r), nil
}

// bytesFromScan normalizes a driver.Value into []byte; reused by JSON-column Scanners.
func bytesFromScan(value any, typeName string) ([]byte, error) {
	switch v := value.(type) {
	case []byte:
		return v, nil
	case string:
		return []byte(v), nil
	default:
		return nil, errors.New(typeName + ": unsupported scan source")
	}
}

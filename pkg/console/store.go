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

// Store 是控制面的写/读层，直接持有 *sqlx.DB。
//
// 复用 pkg/repo 的 typed 结构 + Scanner/Valuer（AuthConfig 透明 KEK 加密、
// rawJSON NULL-safe、HashAPIKey）——控制面写进去的字节，数据面读出来的语义，
// 天然对齐，不可能漂移。
type Store struct {
	db  *sqlx.DB
	pub *cachebus.Publisher // 可选；nil = 只靠数据面 TTL 兜底
}

// NewStore 构造 Store。
func NewStore(db *sqlx.DB) *Store { return &Store{db: db} }

// WithPublisher 挂上 cachebus Publisher，让吊销 key 时精准通知数据面失效
// （把 ≤TTL 窗口收到亚秒级）。nil 时退化成纯 TTL。
func (s *Store) WithPublisher(p *cachebus.Publisher) *Store {
	s.pub = p
	return s
}

// ErrNotFound 资源不存在（handler 翻成 404）。
var ErrNotFound = errors.New("not found")

// =============================================================================
// Accounts
// =============================================================================

// AccountInput 建主账号入参。
type AccountInput struct {
	Pin           string `json:"pin"`
	Name          string `json:"name"`
	QuotaPolicyID *int64 `json:"quota_policy_id,omitempty"`
}

// CreateAccount 建主账号。
func (s *Store) CreateAccount(ctx context.Context, in AccountInput) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO accounts (pin, name, quota_policy_id) VALUES (?, ?, ?)`,
		in.Pin, in.Name, in.QuotaPolicyID)
	return err
}

// AccountView 主账号只读视图。
type AccountView struct {
	Pin           string     `db:"pin" json:"pin"`
	Name          string     `db:"name" json:"name"`
	Enabled       bool       `db:"enabled" json:"enabled"`
	QuotaPolicyID *int64     `db:"quota_policy_id" json:"quota_policy_id,omitempty"`
	CreatedAt     time.Time  `db:"created_at" json:"created_at"`
	UpdatedAt     time.Time  `db:"updated_at" json:"updated_at"`
}

// ListAccounts 列全部未删主账号。
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

// ModelServiceInput 建 model catalog 入参。
type ModelServiceInput struct {
	ServiceID string `json:"service_id"`
	Model     string `json:"model"`
}

// CreateModelService 建全局 model catalog，返回自增 id。
func (s *Store) CreateModelService(ctx context.Context, in ModelServiceInput) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO model_services (service_id, model) VALUES (?, ?)`,
		in.ServiceID, in.Model)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// ModelServiceView model catalog 只读视图。
type ModelServiceView struct {
	ID        int64     `db:"id" json:"id"`
	ServiceID string    `db:"service_id" json:"service_id"`
	Model     string    `db:"model" json:"model"`
	CreatedAt time.Time `db:"created_at" json:"created_at"`
}

// ListModelServices 列全部未删 model。
func (s *Store) ListModelServices(ctx context.Context) ([]ModelServiceView, error) {
	var rows []ModelServiceView
	err := s.db.SelectContext(ctx, &rows,
		`SELECT id, service_id, model, created_at
		 FROM model_services WHERE deleted_at IS NULL ORDER BY id`)
	return rows, err
}

// SubscriptionInput 主账号订阅 model 入参。
type SubscriptionInput struct {
	AccountID      string `json:"account_id"`
	ModelServiceID int64  `json:"model_service_id"`
}

// Subscribe 让主账号订阅一个 model（M5 可见性）。幂等：已存在则更新 enabled=1。
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

// EndpointInput 建上游 endpoint 入参。auth.payload 里带密钥——写入前经
// repo.AuthConfig.Value() 做 AES-256-GCM 加密（跟数据面同一个 KEK）。
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

// AuthInput endpoint 凭证入参（明文，写入即加密）。
type AuthInput struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

// InvalidEndpointError 写前校验失败（endpointcheck.Validate 的 reasons）。
type InvalidEndpointError struct {
	Reasons []string
}

func (e *InvalidEndpointError) Error() string {
	return fmt.Sprintf("endpoint invalid: %v", e.Reasons)
}

// CreateEndpoint 校验 + 加密 + 插入，返回自增 id。
//
// 写前跑 endpointcheck.Validate（跟数据面启动扫描同一份逻辑）——protocol typo /
// vendor 未注册 / translator 不可达 / routing 指 metadata / quirks 编译失败在 API
// 层就被拒（400），而不是等数据面启动扫描 warn。
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

	// 写前业务校验（domain 视图，auth 不参与校验）。
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

// EndpointView endpoint 只读视图——**故意不含 auth payload**（只回 auth.type），
// 密钥永不出 API。
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

// ListEndpoints 列全部未删 endpoint（脱敏视图）。
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

// GetEndpoint 取单个 endpoint（脱敏视图）。
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

// DeleteEndpoint 软删 endpoint（置 deleted_at）。
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

// APIKeyInput 发 key 入参。plaintext 由服务端生成，只回显一次。
type APIKeyInput struct {
	AccountID     string `json:"account_id"`
	SubAccountID  string `json:"sub_account_id"`
	Name          string `json:"name,omitempty"`
	Group         string `json:"group,omitempty"`
	ExternalUser  bool   `json:"external_user,omitempty"`
	QuotaPolicyID *int64 `json:"quota_policy_id,omitempty"`
}

// APIKeyCreated 发 key 结果——**Plaintext 只此一次**返回，之后 DB 只存 hash。
type APIKeyCreated struct {
	APIKeyID  string `json:"api_key_id"`
	Plaintext string `json:"api_key"`
	Prefix    string `json:"api_key_prefix"`
}

// CreateAPIKey 生成随机 key → 存 SHA-256 hash → 返回明文一次。
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

// APIKeyView key 只读视图——**永不含 hash / 明文**，只有前缀和元数据。
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

// ListAPIKeys 列某主账号下的 key（脱敏）。
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

// RevokeAPIKey 吊销 key：置 revoked_at + enabled=0，并经 cachebus 精准通知数据面
// evict（把"吊销后仍缓存有效"窗口从 ≤30s TTL 收到亚秒级）。
//
// 先取 hash 再 UPDATE：hash 是数据面缓存键，控制面持有它即可通知失效，无需明文。
// 发布 best-effort——Redis 挂了只 warn，DB 已落库 + TTL 兜底最终一致。
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
		return ErrNotFound // 已经吊销过
	}

	if s.pub != nil {
		if perr := s.pub.Invalidate(ctx, cachebus.Invalidation{Kind: cachebus.KindAPIKey, Key: hash}); perr != nil {
			slog.WarnContext(ctx, "cachebus invalidate failed; data plane will fall back to TTL", "err", perr, "api_key_id", apiKeyID)
		}
	}
	return nil
}

// =============================================================================
// Quota policies（限流策略库；被 accounts / api_keys 引用）
// =============================================================================

// QuotaPolicyInput 建限流策略入参。Rule 是 ratelimit.PolicyRule 形态
// （{default:{rpm,tpm,rps,...}, per_model:{...}}），写前校验能解析。
type QuotaPolicyInput struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Rule        json.RawMessage `json:"rule"`
}

// InvalidPolicyError rule_json 校验失败。
type InvalidPolicyError struct{ Reason string }

func (e *InvalidPolicyError) Error() string { return "quota policy invalid: " + e.Reason }

// CreateQuotaPolicy 校验 rule_json 形态后插入，返回自增 id。
func (s *Store) CreateQuotaPolicy(ctx context.Context, in QuotaPolicyInput) (int64, error) {
	if in.Name == "" {
		return 0, &InvalidPolicyError{Reason: "name required"}
	}
	if len(in.Rule) == 0 {
		return 0, &InvalidPolicyError{Reason: "rule required"}
	}
	// 校验：能解析成 PolicyRule，且至少有 default 或 per_model（空策略无意义）。
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

// QuotaPolicyView 限流策略只读视图。
type QuotaPolicyView struct {
	ID          int64           `db:"id" json:"id"`
	Name        string          `db:"name" json:"name"`
	Description string          `db:"description" json:"description"`
	RuleJSON    json.RawMessage `db:"rule_json" json:"rule"`
	Enabled     bool            `db:"enabled" json:"enabled"`
	CreatedAt   time.Time       `db:"created_at" json:"created_at"`
}

// ListQuotaPolicies 列全部未删策略。
func (s *Store) ListQuotaPolicies(ctx context.Context) ([]QuotaPolicyView, error) {
	var rows []QuotaPolicyView
	err := s.db.SelectContext(ctx, &rows,
		`SELECT id, name, description, rule_json, enabled, created_at
		 FROM quota_policies WHERE deleted_at IS NULL ORDER BY id`)
	return rows, err
}

// DeleteQuotaPolicy 软删策略。注意：被 accounts/api_keys 引用的策略软删后，数据面
// 仍能按 id 读到（行还在）——真要停用建议改引用方的 quota_policy_id 为 NULL。
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
// helpers
// =============================================================================

// generateAPIKey 生成 "sk-" + 32 字节 url-safe base64 随机体，返回明文 + 前缀。
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

// generateID 生成 prefix + 8 字节 url-safe base64（审计稳定 ID）。
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

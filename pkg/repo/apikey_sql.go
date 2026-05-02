package repo

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jmoiron/sqlx"
)

// SQLAPIKeyProvider 是 IdentityProvider 的 MySQL 实现：
//
// 启动期一次性把所有 enabled + 未过期的 api_keys 加载到内存 map（按 api_key 索引），
// Resolve 走内存查（M2 Auth 是 hot path，每请求查 DB 不可接受）。
//
// **v0.1 不支持热加载**：admin 改 api_keys 后需要重启 gateway 才能生效。
// 后续用 redis pub/sub 做失效通知（跟 model_services / endpoints 一起做）。
//
// Resolve 时再做一次 expires_at 校验——cache 里 enabled 但 expires 已到的 key
// 会在 Resolve 内拒绝（防止内存里旧值跨过期点继续放行）。
type SQLAPIKeyProvider struct {
	db *sqlx.DB

	mu      sync.RWMutex
	byKey   map[string]APIKey
	loadErr error
}

// NewSQLAPIKeyProvider 构造并立即拉一次全量。
//
// 失败时返回 error，cmd 装配方应 fail-fast。
func NewSQLAPIKeyProvider(ctx context.Context, db *sqlx.DB) (*SQLAPIKeyProvider, error) {
	p := &SQLAPIKeyProvider{db: db}
	if err := p.Reload(ctx); err != nil {
		return nil, err
	}
	return p, nil
}

// Reload 重新从 db 拉全量 enabled api_keys 进内存。
//
// admin 改完 keys 想立即生效可以暴露 /admin/v1/apikeys/reload 端点反向调 gateway，
// 或者用 redis pub/sub。v0.1 只在 boot 时调一次。
func (p *SQLAPIKeyProvider) Reload(ctx context.Context) error {
	var rows []APIKey
	// 只加载 enabled = true；过期判定在 Resolve 二次校验（避免 boot 时间 vs request 时间漂移）
	err := p.db.SelectContext(ctx, &rows,
		`SELECT id, tenant_id, api_key, api_key_id, user_id, group_name,
		        external_user, enabled, expires_at, created_at
		 FROM api_keys
		 WHERE enabled = 1`)
	if err != nil {
		return fmt.Errorf("apikey: load: %w", err)
	}

	next := make(map[string]APIKey, len(rows))
	for _, k := range rows {
		next[k.Key] = k
	}

	p.mu.Lock()
	p.byKey = next
	p.mu.Unlock()
	return nil
}

// Resolve 实现 IdentityProvider.Resolve：按 creds.APIKey 查 cache + 校验 expires_at。
func (p *SQLAPIKeyProvider) Resolve(_ context.Context, creds *Credentials) (*UserIdentity, error) {
	if creds == nil || creds.APIKey == "" {
		return nil, errors.New("apikey: missing api key")
	}
	p.mu.RLock()
	k, ok := p.byKey[creds.APIKey]
	p.mu.RUnlock()
	if !ok {
		return nil, errors.New("apikey: unknown api key")
	}
	if k.ExpiresAt != nil && k.ExpiresAt.Before(time.Now()) {
		return nil, errors.New("apikey: expired")
	}
	id := k.ToUserIdentity()
	return &id, nil
}

// 编译期断言。
var _ IdentityProvider = (*SQLAPIKeyProvider)(nil)

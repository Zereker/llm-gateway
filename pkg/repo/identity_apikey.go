package repo

import (
	"context"
	"errors"
	"sync"
)

// APIKeyProvider 是 IdentityProvider 的内存 / 文件默认实现：
// 把 Credentials.APIKey 字段直接对照内存表查 UserIdentity。
//
// 适合单机 / 配置驱动场景。生产可与 ConfigStore Watch 联动，
// 调用 Update 替换内存表实现热加载。
type APIKeyProvider struct {
	mu   sync.RWMutex
	keys map[string]UserIdentity
}

// NewAPIKeyProvider 用初始 key → identity 表构造。
//
// 入参 map 在内部被拷贝，不与外部共享。
func NewAPIKeyProvider(keys map[string]UserIdentity) *APIKeyProvider {
	p := &APIKeyProvider{keys: make(map[string]UserIdentity, len(keys))}
	for k, v := range keys {
		p.keys[k] = v
	}
	return p
}

// Update 整体替换内存表（适合配置 reload）。
func (p *APIKeyProvider) Update(keys map[string]UserIdentity) {
	next := make(map[string]UserIdentity, len(keys))
	for k, v := range keys {
		next[k] = v
	}
	p.mu.Lock()
	p.keys = next
	p.mu.Unlock()
}

// Resolve 实现 IdentityProvider.Resolve：按 creds.APIKey 查表。
func (p *APIKeyProvider) Resolve(_ context.Context, creds *Credentials) (*UserIdentity, error) {
	if creds == nil || creds.APIKey == "" {
		return nil, errors.New("apikey: missing api key")
	}
	p.mu.RLock()
	id, ok := p.keys[creds.APIKey]
	p.mu.RUnlock()
	if !ok {
		return nil, errors.New("apikey: unknown api key")
	}
	return &id, nil
}

// 编译期断言。
var _ IdentityProvider = (*APIKeyProvider)(nil)

package middleware

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/zereker-labs/ai-gateway/pkg/domain"
	"github.com/zereker-labs/ai-gateway/pkg/store"
)

// KVEndpointProvider 是 EndpointProvider 的默认实现：
// 启动期一次性从 store.KV 的指定 prefix 下加载所有 Endpoint 到内存。
//
// **v0.1：PickForModel 选第一个匹配的 endpoint，无加权 / 无 Filter**。
// 完整调度（Filter + Retry + Cooldown + Health）见 pkg/schedule（v0.5+ 接入）。
type KVEndpointProvider struct {
	kv     store.KV
	prefix string

	mu       sync.RWMutex
	all      []*domain.Endpoint
	byModel  map[string][]*domain.Endpoint
}

// NewKVEndpointProvider 构造并立即拉一次全量。
//
// prefix 推荐 "endpoint"；约定 store 中每个 key 的 value 是 JSON 序列化的 domain.Endpoint。
func NewKVEndpointProvider(c context.Context, kv store.KV, prefix string) (*KVEndpointProvider, error) {
	p := &KVEndpointProvider{kv: kv, prefix: prefix}
	if err := p.Reload(c); err != nil {
		return nil, err
	}
	return p, nil
}

// Reload 重新从 store 全量加载（适合手动触发热加载）。
func (p *KVEndpointProvider) Reload(c context.Context) error {
	raws, err := p.kv.List(c, p.prefix)
	if err != nil {
		return fmt.Errorf("endpoint: list %q: %w", p.prefix, err)
	}
	all := make([]*domain.Endpoint, 0, len(raws))
	byModel := make(map[string][]*domain.Endpoint, len(raws))
	for k, raw := range raws {
		var ep domain.Endpoint
		if err := json.Unmarshal(raw, &ep); err != nil {
			return fmt.Errorf("endpoint: parse %q: %w", k, err)
		}
		if ep.ID == "" {
			return fmt.Errorf("endpoint: %q has empty ID", k)
		}
		if ep.Model == "" {
			return fmt.Errorf("endpoint: %q has empty Model", k)
		}
		if ep.Vendor == "" {
			return fmt.Errorf("endpoint: %q has empty Vendor", k)
		}
		all = append(all, &ep)
		byModel[ep.Model] = append(byModel[ep.Model], &ep)
	}

	p.mu.Lock()
	p.all = all
	p.byModel = byModel
	p.mu.Unlock()
	return nil
}

// PickForModel 实现 EndpointProvider.PickForModel。
//
// v0.1：选第一个 endpoint.Group == group 的；都不匹配返回错误。
// 不做加权 / Filter / Cooldown / Health（v0.5+ 由 pkg/schedule 完整实现接管）。
func (p *KVEndpointProvider) PickForModel(_ context.Context, model, group string) (*domain.Endpoint, error) {
	if model == "" {
		return nil, errors.New("endpoint: empty model name")
	}
	if group == "" {
		group = "default"
	}

	p.mu.RLock()
	candidates := p.byModel[model]
	p.mu.RUnlock()

	if len(candidates) == 0 {
		return nil, fmt.Errorf("endpoint: no endpoint for model %q", model)
	}
	for _, ep := range candidates {
		epGroup := ep.Group
		if epGroup == "" {
			epGroup = "default"
		}
		if epGroup == group {
			return ep, nil
		}
	}
	return nil, fmt.Errorf("endpoint: no endpoint for model %q in group %q", model, group)
}

// List 实现 EndpointProvider.List。
func (p *KVEndpointProvider) List(_ context.Context) ([]*domain.Endpoint, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]*domain.Endpoint, len(p.all))
	copy(out, p.all)
	return out, nil
}

// 编译期断言。
var _ EndpointProvider = (*KVEndpointProvider)(nil)

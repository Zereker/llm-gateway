package repo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/zereker-labs/ai-gateway/pkg/domain"
	"github.com/zereker-labs/ai-gateway/pkg/store"
)

// KVModelServiceProvider 是 ModelServiceProvider 的默认实现：
// 启动期一次性从 store.KV 的指定 prefix 下加载所有 ModelServiceSnapshot 到内存，
// 之后请求路径只读内存（无 Watch）。
//
// **v0.1 不支持热加载**；变更需调用 Reload 或重启进程。
type KVModelServiceProvider struct {
	kv     store.KV
	prefix string

	mu      sync.RWMutex
	byModel map[string]*domain.ModelServiceSnapshot
}

// NewKVModelServiceProvider 构造并立即从 kv 拉一次全量。
//
// prefix 推荐 "modelservice"；约定 store 中每个 key 的 value 是 JSON 序列化的
// domain.ModelServiceSnapshot。
func NewKVModelServiceProvider(c context.Context, kv store.KV, prefix string) (*KVModelServiceProvider, error) {
	p := &KVModelServiceProvider{kv: kv, prefix: prefix}
	if err := p.Reload(c); err != nil {
		return nil, err
	}
	return p, nil
}

// Reload 重新从 store 全量加载（适合手动触发热加载）。
func (p *KVModelServiceProvider) Reload(c context.Context) error {
	raws, err := p.kv.List(c, p.prefix)
	if err != nil {
		return fmt.Errorf("model_service: list %q: %w", p.prefix, err)
	}
	next := make(map[string]*domain.ModelServiceSnapshot, len(raws))
	for k, raw := range raws {
		var snap domain.ModelServiceSnapshot
		if err := json.Unmarshal(raw, &snap); err != nil {
			return fmt.Errorf("model_service: parse %q: %w", k, err)
		}
		if snap.Model == "" {
			return fmt.Errorf("model_service: %q has empty Model field", k)
		}
		next[snap.Model] = &snap
	}
	p.mu.Lock()
	p.byModel = next
	p.mu.Unlock()
	return nil
}

// GetByModel 实现 ModelServiceProvider.GetByModel。
func (p *KVModelServiceProvider) GetByModel(_ context.Context, model string) (*domain.ModelServiceSnapshot, error) {
	if model == "" {
		return nil, errors.New("model_service: empty model name")
	}
	p.mu.RLock()
	snap, ok := p.byModel[model]
	p.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("model_service: not found: %s", model)
	}
	return snap, nil
}

// List 实现 ModelServiceProvider.List。
func (p *KVModelServiceProvider) List(_ context.Context) ([]*domain.ModelServiceSnapshot, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]*domain.ModelServiceSnapshot, 0, len(p.byModel))
	for _, s := range p.byModel {
		out = append(out, s)
	}
	return out, nil
}

// 编译期断言：KV 实现只覆盖 Reader（不提供写）。
var _ ModelServiceReader = (*KVModelServiceProvider)(nil)

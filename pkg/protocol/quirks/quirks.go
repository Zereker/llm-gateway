// Package quirks 定义"上游协议 body 翻译后的 vendor / 模型级最终微调"层。
//
// **架构定位**（v0.7）：
//
//	pkg/protocol/combine.go PrepareCall:
//	  client body
//	    → translator.TranslateRequest    （客户端协议 → 上游协议的 shape 转换）
//	    → quirks.Get(ep.Vendor).Rewrite  （vendor / 模型级最终微调）  ← 本包
//	    → adapter.BuildRequest           （HTTP 信封：URL / auth / headers）
//	    → upstream
//
// **为什么需要**：translator 只负责"客户端协议 → 上游协议"的形状转换；同一上游协议
// 内不同 vendor / 模型仍有细微差异：
//
//   - OpenAI o1 / o3 / o4 推理模型：max_tokens → max_completion_tokens；
//     拒绝 temperature / top_p / presence_penalty / frequency_penalty 等字段
//   - DeepSeek deepseek-reasoner：类似 o1 的字段限制
//   - Anthropic Claude 3.7+ extended_thinking：需要插 thinking 块 + 强制 temperature=1
//   - vLLM / Ollama 自部署：可能需要 strip 某些 OpenAI 特有字段
//
// 这些都属于"翻译后的最终修正"，不该污染 translator（translator 是协议层
// 抽象，不关心具体模型）也不该污染 adapter（adapter 是 HTTP 层抽象，不关心 body
// 形状）。quirks 是单独的一层。
//
// **使用模式**：每个 vendor 子包在 init() 注册自己的 Rewriter；不需要 quirks 的
// vendor 不注册，Get 返回 nil 时 combine 直接跳过该阶段。
//
//	func init() {
//	    quirks.Register("openai", quirks.RewriterFunc(reasoningModelRewrite))
//	}
//
// **不做**：
//   - 不在 quirks 里做协议形状转换（那是 translator 的事）
//   - 不在 quirks 里做 HTTP 鉴权 / URL 拼接（那是 adapter 的事）
//   - 不缓存解析结果（每次都 unmarshal body；quirks 应该足够小所以可忽略）
package quirks

import (
	"sync"
)

// Rewriter 上游协议 body 的 vendor / 模型级最终微调。
//
// 输入是 translator 输出的 upstream-shape body；返回 (possibly tweaked) body。
// 不匹配的 model 应该返回 body 原样（同一个 slice 即可，无需拷贝）。
//
// 实现 MUST be safe for concurrent use（无 per-call state；全局注册的单例）。
type Rewriter interface {
	Rewrite(body []byte) ([]byte, error)
}

// RewriterFunc 把普通函数适配为 Rewriter。
type RewriterFunc func(body []byte) ([]byte, error)

// Rewrite 实现 Rewriter。
func (f RewriterFunc) Rewrite(body []byte) ([]byte, error) { return f(body) }

// Chain 按顺序应用多个 Rewriter；任一个失败立即中止。
//
// 空 chain 或全 nil 元素 = no-op。同 vendor 多个 Rewriter 注册会自动 Chain 起来
// （详见 Register）。
type Chain []Rewriter

// Rewrite 实现 Rewriter。
func (c Chain) Rewrite(body []byte) ([]byte, error) {
	for _, r := range c {
		if r == nil {
			continue
		}
		out, err := r.Rewrite(body)
		if err != nil {
			return nil, err
		}
		body = out
	}
	return body, nil
}

// Registry vendor → Rewriter 注册表。
type Registry struct {
	mu sync.RWMutex
	m  map[string]Chain // vendor → 已注册的 rewriter 顺序
}

var defaultRegistry = &Registry{m: make(map[string]Chain)}

// Register 把 r 追加到 vendor 的 Rewriter chain 末尾。多次注册同 vendor 会
// 依次应用——给 vendor 子包提供"我有多个独立 quirks 规则"的能力。
//
// 包 init() 内调用。vendor 名要跟 endpoint.vendor 列对齐
// （e.g. "openai" / "anthropic" / "gemini" / "deepseek" / "ark" 等）。
func Register(vendor string, r Rewriter) {
	if r == nil {
		return
	}
	defaultRegistry.mu.Lock()
	defer defaultRegistry.mu.Unlock()
	defaultRegistry.m[vendor] = append(defaultRegistry.m[vendor], r)
}

// Get 返回 vendor 注册的 Rewriter chain；未注册时返回 nil。
//
// caller 应该 nil 检查再调 Rewrite——避免对没 quirks 的 vendor 做无谓的解包/打包。
func Get(vendor string) Rewriter {
	defaultRegistry.mu.RLock()
	defer defaultRegistry.mu.RUnlock()
	chain := defaultRegistry.m[vendor]
	if len(chain) == 0 {
		return nil
	}
	return chain
}

// Reset 清空注册表，仅供测试用。
func Reset() {
	defaultRegistry.mu.Lock()
	defer defaultRegistry.mu.Unlock()
	defaultRegistry.m = make(map[string]Chain)
}

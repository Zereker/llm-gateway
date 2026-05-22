// Package quirks 定义 endpoint 级 request 微调 DSL——译完上游协议后的最终修正。
//
// **架构定位**（v0.7）：
//
//	pkg/protocol/combine.go PrepareCall:
//	  client body
//	    → translator.TranslateRequest  （客户端协议 → 上游协议的 shape 转换）
//	    → ep.Quirks.RewriteBody        （endpoint 配置的 body 微调）  ← 本包
//	    → adapter.BuildRequest         （HTTP 信封：URL / auth / Content-Type）
//	    → ep.Quirks.RewriteHeader      （endpoint 配置的 header 微调）  ← 本包
//	    → upstream
//
// **为什么需要**：translator 只负责"客户端协议 → 上游协议"的形状转换；同一上游
// 协议内不同 vendor / 模型仍有细微差异。两类典型差异：
//
//   body 字段
//   - OpenAI o1/o3/o4 推理模型：max_tokens → max_completion_tokens；strip
//     temperature / top_p / presence_penalty / frequency_penalty
//   - DeepSeek deepseek-reasoner：类似限制
//   - Anthropic Claude 3.7+ extended_thinking：插 thinking 块 + 强制 temperature=1
//   - vLLM / Ollama：strip 某些 OpenAI 特有字段
//
//   header 字段
//   - 不同 vendor 的 trace-id header 名不一样（X-Request-Id / X-Trace-Id /
//     X-Ark-Request-Id / x-ds-request-id 等）——gateway 统一用 X-Request-Id 写入，
//     deployer 配置 rename 让上游收到自己认的 header 名
//   - vendor 私有 header（如 X-API-Version）需要在 endpoint 上配死
//
// **为什么不做 vendor init()-time 注册**：quirks 是 deployment 知识，不是代码
// 知识。同一 vendor 完全可能部署多个 endpoint 走不同 quirks（一个 o1 / 一个
// gpt-4o），vendor 代码不该硬编码哪些 model 走哪些规则。**deployer 在
// endpoints.quirks JSON 列里直接配，cmd 不需要重编译**。
//
// **DSL**：endpoints.quirks 列存如下 JSON：
//
//	{
//	  "body": {
//	    "rename":      {"max_tokens": "max_completion_tokens"},
//	    "strip":       ["temperature", "top_p"],
//	    "set":         {"reasoning_effort": "high"},
//	    "set_default": {"max_completion_tokens": 4096}
//	  },
//	  "headers": {
//	    "rename":      {"X-Request-Id": "X-Ark-Request-Id"},
//	    "strip":       ["X-Internal-Debug"],
//	    "set":         {"X-Custom-Tag": "prod"},
//	    "set_default": {"User-Agent": "llm-gateway/1.0"}
//	  }
//	}
//
// body / headers 子段任一可省略；全空 / 列 NULL = no-op。每子段内应用顺序固定：
// rename → strip → set → set_default（先腾位置、再清理、再覆写、最后兜底）。
//
// **strict mode**：CompileJSON 用 DisallowUnknownFields，typo 字段在 compile 阶段
// 直接报错（combine.go 翻成 PhaseQuirks PrepareError）。
package quirks

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
)

// Rewriter endpoint 上配置的"上游请求微调"运行期句柄。
//
// **生命周期**：Compile / CompileJSON 一次，多请求共享（无 per-call state，
// 并发安全）。
//
// **使用**：combine.go 在 translator 之后、adapter 之前调 RewriteBody；
// 在 adapter 之后调 RewriteHeader。两步可独立 no-op（spec.Body 或 spec.Headers
// 单独为空时对应方法直接 short-circuit）。
type Rewriter interface {
	// RewriteBody 改 body JSON 字段。返回 new body（可能跟入参同 slice）。
	RewriteBody(body []byte) ([]byte, error)
	// RewriteHeader 改 outgoing HTTP header（in-place）。
	RewriteHeader(h http.Header)
}

// Spec endpoint 上配置的 quirks 规则。两子段全可选。
type Spec struct {
	Body    BodySpec    `json:"body,omitempty"`
	Headers HeadersSpec `json:"headers,omitempty"`
}

// BodySpec body JSON 字段微调。所有字段可选。应用顺序：Rename → Strip → Set → SetDefault。
type BodySpec struct {
	// Rename：from → to（删 from，写 to；from 不存在跳过）。
	Rename map[string]string `json:"rename,omitempty"`
	// Strip：删除指定 key。
	Strip []string `json:"strip,omitempty"`
	// Set：覆写（已存在替换；不存在新增）。值是任意 JSON。
	Set map[string]json.RawMessage `json:"set,omitempty"`
	// SetDefault：仅在 key 不存在时写入。
	SetDefault map[string]json.RawMessage `json:"set_default,omitempty"`
}

// Empty 判断 body spec 是否全空。
func (s BodySpec) Empty() bool {
	return len(s.Rename) == 0 && len(s.Strip) == 0 &&
		len(s.Set) == 0 && len(s.SetDefault) == 0
}

// HeadersSpec HTTP header 微调。同样四种操作，但值类型都是 string（header value）。
type HeadersSpec struct {
	Rename     map[string]string `json:"rename,omitempty"`
	Strip      []string          `json:"strip,omitempty"`
	Set        map[string]string `json:"set,omitempty"`
	SetDefault map[string]string `json:"set_default,omitempty"`
}

// Empty 判断 header spec 是否全空。
func (s HeadersSpec) Empty() bool {
	return len(s.Rename) == 0 && len(s.Strip) == 0 &&
		len(s.Set) == 0 && len(s.SetDefault) == 0
}

// Empty 整个 Spec 全空 = no-op rewriter。
func (s Spec) Empty() bool {
	return s.Body.Empty() && s.Headers.Empty()
}

// Compile 把 spec 编译成 Rewriter；零开销（只是把 spec 包成实现接口的 struct）。
func Compile(spec Spec) Rewriter {
	return &compiled{spec: spec}
}

// CompileJSON 解析 endpoint.quirks JSON 字节 + Compile。
//
//   - 空字节 / 空白 = 返回 no-op Rewriter；不报错
//   - 解析失败（含未知字段 typo） = 返错；上层翻成 PhaseQuirks PrepareError
//
// 启用 DisallowUnknownFields 严格模式：deployer 写错字段名（如 "strips" / "header"）
// 会在 compile 时立即暴露，不会安静吞掉。
func CompileJSON(specJSON []byte) (Rewriter, error) {
	if len(bytes.TrimSpace(specJSON)) == 0 {
		return Compile(Spec{}), nil
	}
	dec := json.NewDecoder(bytes.NewReader(specJSON))
	dec.DisallowUnknownFields()
	var spec Spec
	if err := dec.Decode(&spec); err != nil {
		return nil, fmt.Errorf("quirks: parse spec: %w", err)
	}
	return Compile(spec), nil
}

// compiled 是 Compile 返回的实现。
type compiled struct {
	spec Spec
}

// RewriteBody 按 rename → strip → set → set_default 顺序应用 body spec。
func (c *compiled) RewriteBody(body []byte) ([]byte, error) {
	if c == nil || c.spec.Body.Empty() {
		return body, nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("quirks: parse body: %w", err)
	}

	for from, to := range c.spec.Body.Rename {
		if v, ok := m[from]; ok {
			m[to] = v
			delete(m, from)
		}
	}
	for _, k := range c.spec.Body.Strip {
		delete(m, k)
	}
	for k, v := range c.spec.Body.Set {
		m[k] = v
	}
	for k, v := range c.spec.Body.SetDefault {
		if _, exists := m[k]; !exists {
			m[k] = v
		}
	}

	out, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("quirks: re-marshal: %w", err)
	}
	return out, nil
}

// RewriteHeader 按 rename → strip → set → set_default 顺序应用 header spec（in-place）。
//
// **header 名规范化**：http.Header 内部存的是 canonical 形式（X-Request-Id）；
// 这里调 http.CanonicalHeaderKey 让 deployer 在配置里写大小写都行。
//
// **rename 语义**：from canonical → to canonical；from 不存在时跳过。多 value 一起搬。
// **strip 语义**：删 canonical key 的所有 value。
// **set 语义**：先 Del 再 Set（替换所有 value 成一个）。
// **set_default**：只在 canonical key 不存在时 Set 一个 value。
func (c *compiled) RewriteHeader(h http.Header) {
	if c == nil || c.spec.Headers.Empty() || h == nil {
		return
	}

	for from, to := range c.spec.Headers.Rename {
		fromKey := http.CanonicalHeaderKey(from)
		toKey := http.CanonicalHeaderKey(to)
		vals := h.Values(fromKey)
		if len(vals) == 0 {
			continue
		}
		h.Del(fromKey)
		// 多 value 一起搬：Set 第一个，Add 剩下
		h.Set(toKey, vals[0])
		for _, v := range vals[1:] {
			h.Add(toKey, v)
		}
	}
	for _, k := range c.spec.Headers.Strip {
		h.Del(k)
	}
	for k, v := range c.spec.Headers.Set {
		h.Set(k, v)
	}
	for k, v := range c.spec.Headers.SetDefault {
		canonical := http.CanonicalHeaderKey(k)
		if _, exists := h[canonical]; !exists {
			h.Set(canonical, v)
		}
	}
}

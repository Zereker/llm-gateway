// Package translator 把"协议间数据 shape 翻译"从 adapter 抽出来作为独立层。
//
// **架构定位**：
//
//	┌──────────┐       ┌────────────┐       ┌─────────┐       ┌──────────┐
//	│ Client   │  ───→ │ Translator │  ───→ │ Adapter │  ───→ │ Upstream │
//	│ (OpenAI) │       │ ↓ TranslateRequest  │ + auth  │       │ (Gemini) │
//	│          │  ←─── │ ↓ ResponseHandler   │ + URL   │  ←─── │          │
//	└──────────┘       └────────────┘       └─────────┘       └──────────┘
//
//	Adapter 关心：怎么打到这个 vendor 的 HTTP（URL / auth / headers）
//	Translator 关心：协议间 body shape 翻译 + usage 提取
//	M7 关心：编排
//
// **同协议短路**：用 identity translator，request 几乎透传，response 仅做 SSE / usage 解析。
//
// **跨协议**：每对 (source, target) 一个 translator 实现（pkg/translator/<from>_<to>/）。
// 流式翻译复杂度高（chunk 边界 + 部分 JSON 解析），v0.5 实现 buffer-then-translate
// （Flush 时一次性翻整 body），等 v0.6 单独迭代加流式翻译。
//
// **注册**：translator init() 调 Register；启动时 cmd 完成全部 blank import。
//
// 详见 docs/architecture/02-protocol-translation.md。
package translator

import (
	"sync"

	"github.com/zereker/llm-gateway/pkg/domain"
)

// Translator 把客户端协议翻译成上游协议（请求方向 + 响应方向）。
//
// 实现 MUST be safe for concurrent use（多 gin handler goroutine 并发拿同一 Translator
// 调 TranslateRequest / NewResponseHandler）。
type Translator interface {
	// Source 客户端使用的协议（envelope.SourceProtocol）。
	Source() domain.Protocol
	// Target 上游使用的协议（adapter.Metadata().NativeProtocol）。
	Target() domain.Protocol

	// TranslateRequest 客户端 body → 上游 body。
	// 同协议 identity 走最小逻辑（可能注入辅助字段如 stream_options.include_usage）。
	TranslateRequest(srcBody []byte) (dstBody []byte, err error)

	// NewResponseHandler 每请求一个；负责处理上游响应 chunk → 客户端 chunk。
	// 必须每请求 new（handler 内部有累积 buffer / SSE parser 状态）。
	NewResponseHandler() ResponseHandler
}

// ResponseHandler 处理一次上游响应：chunk-by-chunk 喂入；可选累积；最终 Flush 输出。
//
// **streaming 模式**（identity OpenAI）：Feed 直接返回 chunk 给客户端；usage 在
// Feed 阶段持续解析；Flush 返回 nil bytes + 已积累的 usage。
//
// **buffer-then-translate 模式**（openai_gemini）：Feed 全部累积，返回 nil；Flush
// 一次性翻译累积 body 并返回完整 OpenAI 格式 body + usage。客户端只在 Flush 后看到响应。
//
// 实现 MUST 单 goroutine（M7 Schedule 在同 handler goroutine 内顺序调用）。
type ResponseHandler interface {
	// Feed 喂上游响应的下一段 chunk。
	// 返回 clientBytes：要立即写客户端的字节；nil = 不写（buffer 模式）。
	Feed(chunk []byte) (clientBytes []byte, err error)

	// Flush 上游 EOF 后调；返回最后要写客户端的字节 + 提取到的 usage（nil = 缺失）。
	// 同 handler 多次 Flush 行为未定义；M7 只调一次。
	Flush() (clientBytes []byte, usage *domain.Usage, err error)
}

// Registry translator 全局注册表（init() Register 模式，类似 adapter）。
type Registry struct {
	mu sync.RWMutex
	m  map[key]Translator
}

type key struct {
	src, tgt domain.Protocol
}

var defaultRegistry = &Registry{m: make(map[key]Translator)}

// Register 全局注册（包 init() 内调用）。同 (source, target) 重复注册会 panic
// 让冲突在启动期暴露。
func Register(t Translator) {
	defaultRegistry.mu.Lock()
	defer defaultRegistry.mu.Unlock()
	k := key{src: t.Source(), tgt: t.Target()}
	if _, dup := defaultRegistry.m[k]; dup {
		panic("translator: duplicate registration for " + t.Source().String() + " → " + t.Target().String())
	}
	defaultRegistry.m[k] = t
}

// Find 找 (source, target) 对应的 translator；未注册返回 nil。
func Find(source, target domain.Protocol) Translator {
	defaultRegistry.mu.RLock()
	defer defaultRegistry.mu.RUnlock()
	return defaultRegistry.m[key{src: source, tgt: target}]
}

// Reset 清空注册表，仅供测试用。
func Reset() {
	defaultRegistry.mu.Lock()
	defer defaultRegistry.mu.Unlock()
	defaultRegistry.m = make(map[key]Translator)
}

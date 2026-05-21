package dispatch

import (
	"github.com/zereker/llm-gateway/pkg/adapter"
	"github.com/zereker/llm-gateway/pkg/domain"
	"github.com/zereker/llm-gateway/pkg/translator"
)

// Lookups 把请求级 adapter / translator 查询端口打包，给 InvokerFactory.For
// 和其它需要同时拿两个 lookup 的地方用，避签名臃肿。
type Lookups struct {
	Adapters    AdapterLookup
	Translators TranslatorLookup
}

// AdapterLookup vendor → adapter.Factory 的请求级查询端口。
//
// **为什么挂在请求级（rc.Adapters）而不是 Sender 启动期固定**：
// 多租户 / 灰度场景下，不同请求可以装配不同的 vendor 集合（如某租户仅放行
// OpenAI、另一租户额外放行内部 vLLM）。middleware（M2 Auth / 自定义）按
// tenant 决定 rc.Adapters，dispatch / invoker / eligibility 一律从 rc 取。
//
// 默认实现 DefaultAdapters 包装全局 adapter registry——零配置场景行为不变。
type AdapterLookup interface {
	Get(vendor string) adapter.Factory
}

// TranslatorLookup (src, tgt) → translator.Translator 的请求级查询端口。
// 设计动机同 AdapterLookup。
type TranslatorLookup interface {
	Find(src, tgt domain.Protocol) translator.Translator
}

// DefaultAdapters 包装全局 adapter registry；M3 Envelope 在 rc.Adapters 为
// nil 时填这个值。
type DefaultAdapters struct{}

func (DefaultAdapters) Get(vendor string) adapter.Factory { return adapter.Get(vendor) }

// DefaultTranslators 包装全局 translator registry；M3 Envelope 在
// rc.Translators 为 nil 时填这个值。
type DefaultTranslators struct{}

func (DefaultTranslators) Find(src, tgt domain.Protocol) translator.Translator {
	return translator.Find(src, tgt)
}

// AdaptersFrom 从 RequestContext 取 AdapterLookup；nil / 类型不符时退化到
// DefaultAdapters。
//
// **类型安全 helper**：rc.Adapters 声明为 any 是为了避 pkg/domain → pkg/dispatch
// 循环依赖；所有消费者都走这个 helper，不直接 type-assert。
func AdaptersFrom(rc *domain.RequestContext) AdapterLookup {
	if rc == nil {
		return DefaultAdapters{}
	}
	if l, ok := rc.Adapters.(AdapterLookup); ok && l != nil {
		return l
	}
	return DefaultAdapters{}
}

// TranslatorsFrom 从 RequestContext 取 TranslatorLookup；nil / 类型不符时退化
// 到 DefaultTranslators。设计同 AdaptersFrom。
func TranslatorsFrom(rc *domain.RequestContext) TranslatorLookup {
	if rc == nil {
		return DefaultTranslators{}
	}
	if l, ok := rc.Translators.(TranslatorLookup); ok && l != nil {
		return l
	}
	return DefaultTranslators{}
}

// LookupsFrom 从 rc 取打包的 Lookups（两个 lookup helper 的 sugar）。
func LookupsFrom(rc *domain.RequestContext) Lookups {
	return Lookups{
		Adapters:    AdaptersFrom(rc),
		Translators: TranslatorsFrom(rc),
	}
}

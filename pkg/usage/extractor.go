// Package usage 计量计价全家桶：Extractor + Outbox + PricingSpec + PriceCalculator。
//
// 详见 docs/architecture/05-metering-billing.md。
package usage

import (
	"context"
	"fmt"

	"github.com/zereker-labs/ai-gateway/pkg/domain"
)

// Extractor 工厂：每次请求新建一个 ExtractSession。
//
// 一个 Extractor 对应一种"上游响应格式"，可被多个 Adapter 复用
// （如 OpenAI / Azure / DeepSeek 都用 openai_compat）。
type Extractor interface {
	Name() string
	NewSession(c context.Context, meta domain.UsageMeta) ExtractSession
}

// ExtractSession 流式 / 非流式统一接口。
//
// 流式：for chunk { Feed(chunk) }；最后 Finalize
// 非流式：Feed(fullBody) 一次；然后 Finalize
type ExtractSession interface {
	Feed(chunk []byte) error
	Finalize() (*domain.Usage, error)
}

var extractorRegistry = map[string]Extractor{}

// RegisterExtractor 由各 Extractor 实现包的 init() 调用。
func RegisterExtractor(name string, e Extractor) {
	if _, ok := extractorRegistry[name]; ok {
		panic(fmt.Sprintf("usage: extractor %q already registered", name))
	}
	extractorRegistry[name] = e
}

// GetExtractor 按 name 返回 Extractor；未注册返回 nil。
func GetExtractor(name string) Extractor {
	return extractorRegistry[name]
}

// ExtractorNames 返回当前已注册的 Extractor 名称（启动诊断用）。
func ExtractorNames() []string {
	out := make([]string, 0, len(extractorRegistry))
	for n := range extractorRegistry {
		out = append(out, n)
	}
	return out
}

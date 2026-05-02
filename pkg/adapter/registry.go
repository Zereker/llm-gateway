package adapter

import (
	"fmt"

	"github.com/zereker-labs/ai-gateway/pkg/domain"
)

var registry = map[string]domain.AdapterFactory{}

// Register 注册一个 AdapterFactory；各 vendor adapter 包通过 init() 调用。
//
// 同名重复注册会 panic（启动期失败比静默覆盖好）。
func Register(vendor string, f domain.AdapterFactory) {
	if _, ok := registry[vendor]; ok {
		panic(fmt.Sprintf("adapter: vendor %q already registered", vendor))
	}
	registry[vendor] = f
}

// Get 根据 vendor 取出 factory；未注册返回 nil。
func Get(vendor string) domain.AdapterFactory {
	return registry[vendor]
}

// Vendors 返回当前已注册的厂商列表（启动诊断 / 与 ConfigStore 比对覆盖）。
func Vendors() []string {
	out := make([]string, 0, len(registry))
	for v := range registry {
		out = append(out, v)
	}
	return out
}

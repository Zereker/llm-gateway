package adapter

import "fmt"

var registry = map[string]Factory{}

// Register 注册一个 Factory；各 vendor adapter 包通过 init() 调用。
//
// 同名重复注册会 panic（启动期失败比静默覆盖好）。
func Register(vendor string, f Factory) {
	if _, ok := registry[vendor]; ok {
		panic(fmt.Sprintf("adapter: vendor %q already registered", vendor))
	}
	registry[vendor] = f
}

// Get 根据 vendor 取出 factory；未注册返回 nil。
func Get(vendor string) Factory {
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

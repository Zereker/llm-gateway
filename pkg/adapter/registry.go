package adapter

import "fmt"

var registry = map[string]Factory{}

// Register 注册一个 Factory；各 vendor adapter 包通过 init() 调用。
//
// 契约：
//   - **MUST** 在 init() 阶段调用；运行期调用不安全（registry 无锁；
//     且 Get 在请求热路径上无锁读，依赖 init 完成的内存可见性）。
//   - 同名重复注册 panic（启动期失败比静默覆盖好）。
func Register(vendor string, f Factory) {
	if _, ok := registry[vendor]; ok {
		panic(fmt.Sprintf("adapter: vendor %q already registered", vendor))
	}
	registry[vendor] = f
}

// Get 根据 vendor 取出 factory；未注册返回 nil。
// 假设所有 Register 调用已在 init() 阶段完成；运行期只读。
func Get(vendor string) Factory {
	return registry[vendor]
}


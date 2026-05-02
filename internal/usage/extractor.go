package usage

import (
	"context"
	"fmt"
)

// Extractor 把上游响应转成 Usage。
//
// 一个 Extractor 对应一种"上游响应格式"，可被多个 Adapter 复用
// （如 OpenAI / Azure / DeepSeek 都用 openai_compat）。
type Extractor interface {
	Name() string
	NewSession(ctx context.Context, meta Meta) Session
}

// Session 流式 / 非流式统一接口。
//
// 流式：for chunk { Feed(chunk) }；最后 Finalize
// 非流式：Feed(fullBody) 一次；然后 Finalize
type Session interface {
	Feed(chunk []byte) error
	Finalize() (*Usage, error)
}

var registry = map[string]Extractor{}

// Register 由各 Extractor 包的 init() 调用。
func Register(name string, e Extractor) {
	if _, ok := registry[name]; ok {
		panic(fmt.Sprintf("usage: extractor %q already registered", name))
	}
	registry[name] = e
}

// Get 按 name 返回 Extractor；未注册返回 nil。
func Get(name string) Extractor {
	return registry[name]
}

// Names 返回当前已注册的 Extractor 名称（启动诊断用）。
func Names() []string {
	out := make([]string, 0, len(registry))
	for n := range registry {
		out = append(out, n)
	}
	return out
}

// Package translator 实现协议族翻译（如 Anthropic ↔ OpenAI / Gemini ↔ OpenAI）。
//
// 详见 docs/architecture/02-protocol-translation.md 第 5 节。
package translator

import "github.com/zereker-labs/ai-gateway/pkg/ctx"

// Translator 把请求 / 响应在两个协议族之间双向翻译。
//
// 每个 Translator 是单向的 (src → dst)；需要双向时实例化两个。
type Translator interface {
	TranslateRequest(env *ctx.RequestEnvelope) ([]byte, error)
	TranslateResponse(resp *ctx.CanonicalResponse) (*ctx.CanonicalResponse, error)
	TranslateStreamChunk(chunk []byte) ([]byte, error)
}

type key struct {
	Src ctx.Protocol
	Dst ctx.Protocol
}

var registry = map[key]Translator{}

// Register 注册 (src → dst) Translator。
func Register(src, dst ctx.Protocol, t Translator) {
	registry[key{src, dst}] = t
}

// Get 返回 (src → dst) 的 Translator。src == dst 时返回 identity（透传）。
func Get(src, dst ctx.Protocol) Translator {
	if src == dst {
		return identity{}
	}
	return registry[key{src, dst}]
}

// identity 同协议透传 Translator。
type identity struct{}

func (identity) TranslateRequest(env *ctx.RequestEnvelope) ([]byte, error) {
	return env.RawBytes, nil
}

func (identity) TranslateResponse(r *ctx.CanonicalResponse) (*ctx.CanonicalResponse, error) {
	return r, nil
}

func (identity) TranslateStreamChunk(chunk []byte) ([]byte, error) {
	return chunk, nil
}

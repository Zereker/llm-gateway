package adapter

import "github.com/zereker-labs/ai-gateway/pkg/ctx"

// Translator 把请求 / 响应在两个协议族之间双向翻译。
//
// 详见 docs/architecture/02-protocol-translation.md 第 5 节。
//
// 每个 Translator 是单向的 (src → dst)；需要双向时实例化两个。
type Translator interface {
	TranslateRequest(env *ctx.RequestEnvelope) ([]byte, error)
	TranslateResponse(resp *ctx.CanonicalResponse) (*ctx.CanonicalResponse, error)
	TranslateStreamChunk(chunk []byte) ([]byte, error)
}

type translatorKey struct {
	Src ctx.Protocol
	Dst ctx.Protocol
}

var translatorRegistry = map[translatorKey]Translator{}

// RegisterTranslator 注册 (src → dst) Translator。各 Translator 包通过 init() 调用。
func RegisterTranslator(src, dst ctx.Protocol, t Translator) {
	translatorRegistry[translatorKey{src, dst}] = t
}

// GetTranslator 返回 (src → dst) 的 Translator。src == dst 时返回 identity（透传）。
func GetTranslator(src, dst ctx.Protocol) Translator {
	if src == dst {
		return identityTranslator{}
	}
	return translatorRegistry[translatorKey{src, dst}]
}

// identityTranslator 同协议透传。
type identityTranslator struct{}

func (identityTranslator) TranslateRequest(env *ctx.RequestEnvelope) ([]byte, error) {
	return env.RawBytes, nil
}

func (identityTranslator) TranslateResponse(r *ctx.CanonicalResponse) (*ctx.CanonicalResponse, error) {
	return r, nil
}

func (identityTranslator) TranslateStreamChunk(chunk []byte) ([]byte, error) {
	return chunk, nil
}

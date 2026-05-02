package adapter

import (
	"fmt"

	"github.com/zereker-labs/ai-gateway/pkg/domain"
)

// Translator 把请求 / 响应在两个协议族之间双向翻译。
//
// 详见 docs/architecture/02-protocol-translation.md 第 5 节。
//
// 每个 Translator 是单向的 (src → dst)；需要双向时实例化两个。
type Translator interface {
	TranslateRequest(env *domain.RequestEnvelope) ([]byte, error)
	TranslateResponse(resp *domain.CanonicalResponse) (*domain.CanonicalResponse, error)
	TranslateStreamChunk(chunk []byte) ([]byte, error)
}

type translatorKey struct {
	Src domain.Protocol
	Dst domain.Protocol
}

var translatorRegistry = map[translatorKey]Translator{}

// RegisterTranslator 注册 (src → dst) Translator。各 Translator 包通过 init() 调用。
//
// 契约同 Register：MUST 在 init() 阶段调用；同 (src, dst) 重复注册 panic。
func RegisterTranslator(src, dst domain.Protocol, t Translator) {
	k := translatorKey{src, dst}
	if _, ok := translatorRegistry[k]; ok {
		panic(fmt.Sprintf("translator: (%s → %s) already registered", src, dst))
	}
	translatorRegistry[k] = t
}

// GetTranslator 返回 (src → dst) 的 Translator。src == dst 时返回 identity（透传）。
func GetTranslator(src, dst domain.Protocol) Translator {
	if src == dst {
		return identityTranslator{}
	}
	return translatorRegistry[translatorKey{src, dst}]
}

// identityTranslator 同协议透传。
type identityTranslator struct{}

func (identityTranslator) TranslateRequest(env *domain.RequestEnvelope) ([]byte, error) {
	return env.RawBytes, nil
}

func (identityTranslator) TranslateResponse(r *domain.CanonicalResponse) (*domain.CanonicalResponse, error) {
	return r, nil
}

func (identityTranslator) TranslateStreamChunk(chunk []byte) ([]byte, error) {
	return chunk, nil
}

package identity

import (
	"github.com/zereker/llm-gateway/pkg/domain"
	"github.com/zereker/llm-gateway/pkg/translator"
	"github.com/zereker/llm-gateway/pkg/usage/extractor"
)

// anthropicTranslator is the Anthropic ↔ Anthropic identity translator.
//
// Covers the case where the client sends requests via the Anthropic SDK and
// the upstream is also Anthropic (either the real anthropic.com endpoint or
// a Bedrock-forwarded anthropic-compatible endpoint).
//
// **Request side**: pass-through (Anthropic has no stream_options-style
// enhancement field, so nothing needs to be injected).
// **Response side**: the handler passes chunks through as-is and extracts
// usage on the side via the extractor (factored out in v0.5 G6).
type anthropicTranslator struct{}

func (anthropicTranslator) Source() domain.Protocol { return domain.ProtoAnthropic }
func (anthropicTranslator) Target() domain.Protocol { return domain.ProtoAnthropic }

func (anthropicTranslator) TranslateRequest(srcBody []byte) ([]byte, error) {
	return srcBody, nil
}

func (anthropicTranslator) NewResponseHandler() translator.ResponseHandler {
	return &anthropicResponseHandler{ex: extractor.NewAnthropic()}
}

type anthropicResponseHandler struct {
	ex extractor.Session
}

func (h *anthropicResponseHandler) Feed(chunk []byte) ([]byte, error) {
	if len(chunk) == 0 {
		return nil, nil
	}
	h.ex.Feed(chunk)
	// Same-protocol pass-through: return the chunk to the client unchanged
	return chunk, nil
}

func (h *anthropicResponseHandler) Flush() ([]byte, *domain.Usage, error) {
	return nil, h.ex.Final(), nil
}

func init() {
	translator.Register(anthropicTranslator{})
}

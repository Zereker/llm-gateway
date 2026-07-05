// Package identity provides "same-protocol" translators: used when the
// client protocol equals the upstream protocol, where the request is nearly
// pass-through and the response only does SSE pass-through + usage extraction.
//
// init() registers ProtoOpenAI ↔ ProtoOpenAI (OpenAI client → OpenAI-compatible
// upstream, covering the real OpenAI as well as DeepSeek / ARK / Qwen and other
// vendors in the OpenAI protocol family).
//
// Future pure pass-through cases such as Anthropic ↔ Anthropic / Gemini ↔ Gemini
// are likewise extended within this package.
//
// **Usage extraction** goes through the OpenAI Session in pkg/usage/extractor
// (factored out in v0.5 G6); this handler's main path only handles chunk
// pass-through.
package identity

import (
	"encoding/json"

	"github.com/zereker/llm-gateway/pkg/domain"
	"github.com/zereker/llm-gateway/pkg/translator"
	"github.com/zereker/llm-gateway/pkg/usage/extractor"
)

// openaiTranslator is the OpenAI ↔ OpenAI identity translator.
//
// **Request side**: automatically injects stream_options.include_usage = true
// into requests with stream=true, so the upstream is guaranteed to return a
// usage chunk.
//
// **Response side**: the handler passes chunks through to the client and
// extracts usage on the side via the extractor.
type openaiTranslator struct{}

func (openaiTranslator) Source() domain.Protocol { return domain.ProtoOpenAI }
func (openaiTranslator) Target() domain.Protocol { return domain.ProtoOpenAI }

func (openaiTranslator) TranslateRequest(srcBody []byte) ([]byte, error) {
	return ensureStreamUsage(srcBody), nil
}

func (openaiTranslator) NewResponseHandler() translator.ResponseHandler {
	return &openaiResponseHandler{ex: extractor.NewOpenAI()}
}

type openaiResponseHandler struct {
	ex extractor.Session
}

func (h *openaiResponseHandler) Feed(chunk []byte) ([]byte, error) {
	if len(chunk) == 0 {
		return nil, nil
	}
	h.ex.Feed(chunk)
	// Same-protocol pass-through: return the chunk to the client unchanged
	return chunk, nil
}

func (h *openaiResponseHandler) Flush() ([]byte, *domain.Usage, error) {
	return nil, h.ex.Final(), nil
}

// ensureStreamUsage ensures stream_options.include_usage = true in a
// streaming body.
//
// Returns the original body on failure (invalid JSON / non-streaming) —
// the translator should not abort just because this enhancement failed.
func ensureStreamUsage(body []byte) []byte {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return body
	}
	// Leave non-streaming requests untouched
	if streamRaw, ok := m["stream"]; ok {
		var stream bool
		if err := json.Unmarshal(streamRaw, &stream); err == nil && !stream {
			return body
		}
	} else {
		// No stream field = defaults to false (OpenAI behavior), don't inject
		return body
	}

	// Parse the stream_options sub-object
	var so map[string]json.RawMessage
	if raw, ok := m["stream_options"]; ok {
		_ = json.Unmarshal(raw, &so)
	}
	if so == nil {
		so = make(map[string]json.RawMessage)
	}
	so["include_usage"] = json.RawMessage(`true`)

	soBytes, err := json.Marshal(so)
	if err != nil {
		return body
	}
	m["stream_options"] = soBytes
	out, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return out
}

func init() {
	translator.Register(openaiTranslator{})
}

package identity

import (
	"github.com/zereker/llm-gateway/internal/domain"
	"github.com/zereker/llm-gateway/internal/translator"
	"github.com/zereker/llm-gateway/internal/usage/extractor"
)

// responsesTranslator is the OpenAI Responses ↔ Responses identity translator.
//
// **Background**: the Responses API is the new protocol OpenAI introduced in
// 2024 H2 (/v1/responses); its shape differs from Chat Completions:
//
//	{
//	  "model": "gpt-4o",
//	  "input": "...",                  // string or message array
//	  "instructions": "...",           // system prompt
//	  "previous_response_id": "...",   // stateful continuation
//	  "stream": true,
//	  "store": true                    // whether to persist
//	}
//
// **Identity use case**: the client uses the Responses SDK and the upstream
// is also an OpenAI Responses endpoint — a pure pass-through. Currently the
// OpenAI adapter routes by ep.Routing.URL, so the deployer just needs to
// configure the endpoint's url as `https://api.openai.com/v1/responses` to
// route through this translator.
//
// **Request side**: pass-through (Responses has no stream_options-style
// enhancement field — unlike Chat Completions it doesn't need include_usage
// injected, since Responses streaming naturally sends a usage event at the end).
//
// **Response side**: the handler passes chunks through and extracts usage via
// the Responses extractor. Note the Responses usage field names differ from
// Chat Completions (input_tokens / output_tokens, not prompt_tokens /
// completion_tokens), and in streaming the usage arrives nested inside the
// final response.completed event — hence the dedicated extractor.
//
// **Cross-protocol** (responses_chat / responses_anthropic etc.): out of
// scope for the v1.0 minimum; most clients using the Responses protocol
// connect directly to an OpenAI upstream.
type responsesTranslator struct{}

func newResponses() translator.Translator { return responsesTranslator{} }

func (responsesTranslator) Source() domain.Protocol { return domain.ProtoResponses }
func (responsesTranslator) Target() domain.Protocol { return domain.ProtoResponses }

func (responsesTranslator) TranslateRequest(srcBody []byte) ([]byte, error) {
	return srcBody, nil
}

func (responsesTranslator) NewResponseHandler() translator.ResponseHandler {
	return &responsesResponseHandler{ex: extractor.NewResponses()}
}

type responsesResponseHandler struct {
	ex extractor.Session
}

func (h *responsesResponseHandler) Feed(chunk []byte) ([]byte, error) {
	if len(chunk) == 0 {
		return nil, nil
	}

	h.ex.Feed(chunk)

	return chunk, nil
}

func (h *responsesResponseHandler) Flush() ([]byte, *domain.Usage, error) {
	return nil, h.ex.Final(), nil
}

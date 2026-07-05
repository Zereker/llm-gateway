// Package extractor pulls the logic of "extracting usage from the upstream response
// per protocol" out of translator so it can be shared.
//
// **Background**: usage parsing used to be scattered across the ResponseHandler of
// all 5 translators — but normalized by **upstream protocol** there are really only
// 3 variants (OpenAI / Anthropic / Gemini), each duplicated across 2 translators.
// Once extracted, translator only cares about "translate chunk -> client format",
// and usage extraction runs as a side-channel.
//
// **Usage pattern** (inside a translator's ResponseHandler):
//
//	type myHandler struct {
//	    ex extractor.Session  // session matching the upstream protocol
//	    ...
//	}
//
//	func newHandler() *myHandler {
//	    return &myHandler{ex: extractor.NewOpenAI()}  // upstream is OpenAI
//	}
//
//	func (h *myHandler) Feed(chunk []byte) ([]byte, error) {
//	    h.ex.Feed(chunk)        // side-channel: extract usage
//	    return chunk, nil       // main path: pass through / translate the chunk
//	}
//
//	func (h *myHandler) Flush() ([]byte, *domain.Usage, error) {
//	    return nil, h.ex.Final(), nil
//	}
//
// **Adaptive mode**: each Session implementation determines SSE vs non-SSE from the
// prefix of the first chunk, so the translator doesn't need to declare up front
// whether the response is streaming or not.
//
// **Concurrency**: a single Session instance is **not** guaranteed goroutine-safe;
// M7 calls it sequentially within the same handler goroutine.
package extractor

import "github.com/zereker/llm-gateway/pkg/domain"

// Session is a usage-extraction session for a single request.
//
// State: internally accumulates an SSE buffer (streaming) or a body buffer
// (non-streaming). Final() triggers the final parse.
//
// Implementations MUST:
//   - call Feed and Final sequentially within the same goroutine
//   - make Final safe to call multiple times (idempotent; returns the same result)
//   - not retain a slice reference into a Feed chunk (copy into its own buffer)
type Session interface {
	// Feed supplies the next chunk of upstream response bytes. For streaming it may
	// be called multiple times; for non-streaming it may be fed all at once or in
	// pieces.
	Feed(chunk []byte)

	// Final returns the best usage estimate as of now; nil means the upstream
	// didn't return usage information.
	Final() *domain.Usage
}

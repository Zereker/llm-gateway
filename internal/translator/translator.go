// Package translator translates data shapes between protocols (body / SSE / usage).
//
// **Architecture position** (v0.6 facade):
//
//	internal/protocol.Handler = Combine(protocol.Factory, translator.Translator)
//
// translator only handles body shape; the HTTP layer goes through internal/protocol.
// The end-to-end Handler is dynamically composed by internal/protocol.Combine per
// request based on (srcProto, ep.Protocol). Consumers only see protocol.Handler,
// never consuming Translator directly.
//
// **Same-protocol shortcut**: identity translators are registered under
// internal/translator/identity (one each for OpenAI/Anthropic/Responses); the request
// is nearly pass-through, and the response only does SSE / usage parsing.
//
// **Cross-protocol**: one translator implementation per (source, target) pair
// (internal/translator/<from>_<to>/). Streaming translation is inherently complex
// (chunk boundaries + partial JSON parsing); the current implementation is
// buffer-then-translate (translates the whole body at once on Flush); true
// streaming translation is left for a future iteration.
//
// **Registration**: internal/builtin.NewLookup assembles every translator into
// one *Registry at application startup; protocol.DefaultLookup looks them up per
// request via Registry.FindVia(src, tgt, pivot).
//
// See docs/architecture/02-protocol-translation.md for details.
package translator

import (
	"sync"

	"github.com/zereker/llm-gateway/internal/domain"
)

// Translator translates the client protocol into the upstream protocol (both
// the request direction and the response direction).
//
// Implementations MUST be safe for concurrent use (multiple gin handler
// goroutines may concurrently call TranslateRequest / NewResponseHandler on
// the same Translator).
type Translator interface {
	// Source is the protocol used by the client (envelope.SourceProtocol).
	Source() domain.Protocol
	// Target is the protocol used by upstream (matches domain.Endpoint.Protocol).
	Target() domain.Protocol

	// TranslateRequest converts client body -> upstream body.
	// Same-protocol identity takes the minimal path (may inject helper fields
	// such as stream_options.include_usage).
	TranslateRequest(srcBody []byte) (dstBody []byte, err error)

	// NewResponseHandler returns one handler per request; it handles upstream
	// response chunks -> client chunks.
	// Must be created new per request (the handler carries internal
	// accumulation buffer / SSE parser state).
	NewResponseHandler() ResponseHandler
}

// ResponseHandler processes one upstream response: fed chunk-by-chunk;
// optionally accumulates; finally emits output on Flush.
//
// **Streaming mode** (identity OpenAI): Feed returns the chunk to the client
// immediately; usage is parsed incrementally during Feed; Flush returns nil
// bytes plus the usage accumulated so far.
//
// **Buffer-then-translate mode** (openai_gemini): Feed accumulates everything
// and returns nil; Flush translates the accumulated body all at once and
// returns the full OpenAI-format body plus usage. The client only sees the
// response after Flush.
//
// Implementations MUST be single-goroutine (M7 Schedule calls sequentially
// within the same handler goroutine).
type ResponseHandler interface {
	// Feed supplies the next chunk of the upstream response.
	// Returns clientBytes: bytes to write to the client immediately; nil means
	// don't write yet (buffer mode).
	Feed(chunk []byte) (clientBytes []byte, err error)

	// Flush is called after upstream EOF; returns the final bytes to write to
	// the client plus the extracted usage (nil = missing).
	// Behavior of calling Flush more than once on the same handler is
	// undefined; M7 only calls it once.
	Flush() (clientBytes []byte, usage *domain.Usage, err error)
}

// Registry holds an application's translator set, keyed by (source, target).
// It is assembled once at startup (see NewRegistry) and read concurrently on
// the request path.
type Registry struct {
	mu sync.RWMutex
	m  map[key]Translator
}

type key struct {
	src, tgt domain.Protocol
}

// NewRegistry builds an isolated translator registry. Registration is
// expected during application assembly; the returned registry is then safe
// for concurrent reads.
func NewRegistry(translators ...Translator) *Registry {
	r := &Registry{m: make(map[key]Translator, len(translators))}
	for _, t := range translators {
		r.Register(t)
	}

	return r
}

// Register adds a translator to this registry and panics on duplicates.
func (r *Registry) Register(t Translator) {
	if t == nil {
		panic("translator: nil registration")
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	k := key{src: t.Source(), tgt: t.Target()}
	if _, dup := r.m[k]; dup {
		panic("translator: duplicate registration for " + t.Source().String() + " → " + t.Target().String())
	}

	r.m[k] = t
}

// Find looks up a translator in this registry.
func (r *Registry) Find(source, target domain.Protocol) Translator {
	if r == nil {
		return nil
	}

	r.mu.RLock()
	defer r.mu.RUnlock()

	return r.m[key{src: source, tgt: target}]
}

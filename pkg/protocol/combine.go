package protocol

import (
	"context"
	"net/http"
	"sync"

	"github.com/zereker/llm-gateway/pkg/domain"
	"github.com/zereker/llm-gateway/pkg/protocol/quirks"
	"github.com/zereker/llm-gateway/pkg/translator"
)

// Combine assembles a Factory (vendor HTTP layer) + translator.Translator
// (body conversion) into a single Handler. **The core helper of the facade fusion.**
//
// **Usage pattern**: DefaultLookup.Get(ep, srcProto) calls Combine at request time
// to compose the current endpoint's Factory with the selected translator into a
// Handler; it is not statically registered in init(). This reflects the v0.6 shift
// of "protocol ownership" from the vendor level down to the endpoint level.
//
// **Constraint**: translator.Target() must == ep.Protocol; Combine does not
// validate this (it's a hot runtime path) — it's guaranteed by DefaultLookup
// when it picks via translator.Find(src, ep.Protocol).
func Combine(ad Factory, tr translator.Translator) Handler {
	if ad == nil {
		panic("protocol.Combine: nil Factory")
	}
	if tr == nil {
		panic("protocol.Combine: nil translator.Translator")
	}
	meta := ad.Metadata()
	return &combined{
		ad: ad,
		tr: tr,
		caps: Capabilities{
			SourceProtocol:      tr.Source(),
			UpstreamProtocol:    tr.Target(),
			SupportedModalities: meta.SupportedModalities,
		},
	}
}

// combined is the Handler implementation produced by Combine — internally the
// facade still calls the underlying adapter / translator.
//
// **quirksCache**: endpoint.Quirks JSON is compiled into a Rewriter the first
// time it's seen; subsequent requests with the same spec (i.e. string(rawJSON))
// hit the cache directly.
//   - key   = string(endpoint.Quirks) — the JSON literal; shared across different
//     endpoints with the same spec
//   - value = quirks.Rewriter
//
// No active invalidation — after the deployer edits SQL, the new spec's string
// naturally differs and gets a new entry; old entries are never evicted (scale
// is generally < 100, which is acceptable). Switch to a hashicorp lru if strict
// eviction is ever needed.
type combined struct {
	ad          Factory
	tr          translator.Translator
	caps        Capabilities
	quirksCache sync.Map // string(ep.Quirks) → quirks.Rewriter
}

func (c *combined) Capabilities() Capabilities { return c.caps }

func (c *combined) PrepareCall(ctx context.Context, ep *domain.Endpoint, srcBody []byte) (*Call, error) {
	// phase 1: translator — client protocol → upstream protocol shape
	upstreamBody, err := c.tr.TranslateRequest(srcBody)
	if err != nil {
		return nil, NewPrepareError(PhaseTranslate, err)
	}

	// phase 2: quirks — endpoint-configured body + header tweaks.
	// **Body and header run through together before handing off to the adapter**,
	// keeping quirks → adapter a one-way pipe; the adapter doesn't need to know
	// quirks exist.
	var extraHeaders http.Header
	if len(ep.Quirks) > 0 {
		rw, err := c.quirksFor(ep.Quirks)
		if err != nil {
			return nil, NewPrepareError(PhaseQuirks, err)
		}
		upstreamBody, err = rw.RewriteBody(upstreamBody)
		if err != nil {
			return nil, NewPrepareError(PhaseQuirks, err)
		}
		extraHeaders = make(http.Header)
		rw.RewriteHeader(extraHeaders) // run spec against an empty header: set / set_default take effect
	}

	// phase 3: adapter — HTTP envelope (URL / Auth / Content-Type), merging extraHeaders.
	// Adapter-internal convention: copy extraHeaders first, then write its own
	// protocol-required headers (written later so they override quirks), to
	// prevent a deployer from accidentally breaking Authorization / Content-Type.
	sess, err := c.ad.NewSession(ctx, ep, &domain.RequestEnvelope{
		SourceProtocol: c.caps.SourceProtocol,
		RawBytes:       srcBody, // backup for session implementations that may reference the original body
	})
	if err != nil {
		return nil, NewPrepareError(PhaseBuild, err)
	}
	req, err := sess.BuildRequest(upstreamBody, extraHeaders)
	if err != nil {
		_ = sess.Close()
		return nil, NewPrepareError(PhaseBuild, err)
	}
	// v0.5 slim session has no streaming state — close right after construction.
	_ = sess.Close()

	return &Call{Request: req, UpstreamBody: upstreamBody}, nil
}

// quirksFor gets (or builds) the Rewriter for endpoint.Quirks, cached in a sync.Map.
//
// **key**: string(rawSpec) — same string literal shares the same Rewriter; different
// endpoints configured with identical quirks share the same compiled artifact.
//
// **Errors are not cached**: a failed compile is not stored in the cache, so it
// retries compilation on every call (so a deployer's SQL edit takes effect
// immediately; caching a broken rule would only make debugging harder).
func (c *combined) quirksFor(rawSpec []byte) (quirks.Rewriter, error) {
	key := string(rawSpec)
	if cached, ok := c.quirksCache.Load(key); ok {
		return cached.(quirks.Rewriter), nil
	}
	rw, err := quirks.CompileJSON(rawSpec)
	if err != nil {
		return nil, err
	}
	actual, _ := c.quirksCache.LoadOrStore(key, rw)
	return actual.(quirks.Rewriter), nil
}

func (c *combined) NewResponseStream() ResponseStream {
	return &combinedStream{inner: c.tr.NewResponseHandler()}
}

// Classify passes through to the Factory's Classifier (if implemented).
//
// **Interface promotion**: Classifier is an optional implementation on Factory;
// combined automatically exposes this capability so upstream code only needs to
// type-assert protocol.Classifier.
func (c *combined) Classify(status int, body []byte) *domain.AdapterError {
	if cls, ok := c.ad.(Classifier); ok {
		return cls.Classify(status, body)
	}
	return nil
}

// combinedStream wraps translator.ResponseHandler as protocol.ResponseStream
// (identical interface shape — just a package rename + avoiding an import cycle risk).
type combinedStream struct {
	inner translator.ResponseHandler
}

func (s *combinedStream) Feed(chunk []byte) ([]byte, error)     { return s.inner.Feed(chunk) }
func (s *combinedStream) Flush() ([]byte, *domain.Usage, error) { return s.inner.Flush() }

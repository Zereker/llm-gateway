package protocol

import (
	"context"
	"io"
	"net/http"
	"sync"

	"github.com/zereker/llm-gateway/pkg/domain"
	"github.com/zereker/llm-gateway/pkg/protocol/quirks"
	"github.com/zereker/llm-gateway/pkg/translator"
)

// Combine assembles a Factory (vendor HTTP layer) + translator.Translator
// (body conversion) into a Handler. **The core helper for facade composition.**
//
// **Usage pattern**: DefaultLookup.Get(ep, srcProto) calls Combine at request
// time to combine the current endpoint's Factory with the selected translator
// into a Handler; it is not statically registered into a matrix at startup. This
// reflects v0.6's move of "protocol ownership" from vendor-level to endpoint-level.
//
// **Constraint**: translator.Target() must == ep.Protocol; Combine does not
// validate this (it's a hot runtime path) — DefaultLookup guarantees it when
// selecting via translators.FindVia(src, ep.Protocol, pivot).
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
// facade still delegates to the original adapter / translator.
//
// **quirksCache**: endpoint.Quirks JSON is compiled into a Rewriter the first
// time it's seen; subsequent requests with the same spec (i.e. the same
// string(rawJSON)) hit the cache directly.
//   - key   = string(endpoint.Quirks) — the JSON literal; endpoints with the
//     same spec share the compiled result
//   - value = quirks.Rewriter
//
// No active invalidation — once a deployer changes the SQL, the new spec's
// string naturally differs and a new entry is added; old entries are never
// evicted (cardinality is typically < 100, which is acceptable). Switch to a
// hashicorp lru if strict eviction is required.
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

	// phase 2: quirks — body + header tweaks configured on the endpoint.
	// **Body and header both finish before handing off to the adapter**,
	// keeping the quirks → adapter pipeline one-directional; the adapter
	// doesn't need to know quirks exist.
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
		rw.RewriteHeader(extraHeaders) // run the spec against an empty header: set / set_default take effect
	}

	// phase 3: adapter — HTTP envelope (URL / Auth / Content-Type), merging
	// extraHeaders. Internal adapter convention: copy extraHeaders first,
	// then write its own protocol-required headers (later writes override
	// quirks), preventing a deployer from accidentally breaking the request
	// by overriding Authorization / Content-Type.
	sess, err := c.ad.NewSession(ctx, ep, &domain.RequestEnvelope{
		SourceProtocol: c.caps.SourceProtocol,
		RawBytes:       srcBody, // backup copy for session implementations that may reference the original body
	})
	if err != nil {
		return nil, NewPrepareError(PhaseBuild, err)
	}
	req, err := sess.BuildRequest(upstreamBody, extraHeaders)
	if err != nil {
		_ = sess.Close()
		return nil, NewPrepareError(PhaseBuild, err)
	}
	// The v0.5 slim session has no streaming state — close it right after construction.
	_ = sess.Close()

	return &Call{Request: req, UpstreamBody: upstreamBody}, nil
}

// quirksFor gets (or builds) the Rewriter corresponding to endpoint.Quirks,
// cached in a sync.Map.
//
// **key**: string(rawSpec) — the same string literal maps to the same
// Rewriter; different endpoints configured with the same quirks share the
// same compiled artifact.
//
// **Errors are not cached**: on compile failure nothing is stored in the
// cache, so it's retried every time (this lets a deployer's SQL change take
// effect immediately; caching failed rules would only make debugging harder).
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

// Classify delegates to the Factory's Classifier, if implemented.
//
// **Interface promotion**: Factory's Classifier is an optional implementation;
// combined automatically surfaces this capability so callers only need to
// type-assert protocol.Classifier.
func (c *combined) Classify(status int, body []byte) *domain.AdapterError {
	if cls, ok := c.ad.(Classifier); ok {
		return cls.Classify(status, body)
	}
	return nil
}

// DecodeTransport delegates to the Factory's TransportDecoder, if
// implemented. Same capability-promotion pattern as Classify — callers only
// type-assert protocol.TransportDecoder; nil means no de-framing is needed.
func (c *combined) DecodeTransport(resp *http.Response) io.Reader {
	if dec, ok := c.ad.(TransportDecoder); ok {
		return dec.DecodeTransport(resp)
	}
	return nil
}

// combinedStream wraps a translator.ResponseHandler as a
// protocol.ResponseStream (identical interface shape — just renaming the
// package to avoid an import-cycle risk).
type combinedStream struct {
	inner translator.ResponseHandler
}

func (s *combinedStream) Feed(chunk []byte) ([]byte, error)     { return s.inner.Feed(chunk) }
func (s *combinedStream) Flush() ([]byte, *domain.Usage, error) { return s.inner.Flush() }

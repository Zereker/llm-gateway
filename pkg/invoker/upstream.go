// Package upstream encapsulates two actions: "make one call to an upstream"
// and "stream the response back"; the M7 driver loop keeps retry /
// fallback / cooldown orchestration to itself and hands the details of
// HTTP / protocol Handler / classify / streamed chunk forwarding to this
// package.
//
// **Responsibility boundary (after the v0.6 merge)**:
//
//   - Knows: making the HTTP call, driving the caller-supplied
//     protocol.Handler through PrepareCall / NewResponseStream, classifying
//     the outcome by HTTP status + Handler.Classify, copying streamed
//     chunks.
//   - Doesn't know: retry policy, cooldown, the Selection state machine,
//     the HTTP framework (gin / echo / chi are all fine), where the
//     protocol.Handler implementation comes from (global registry /
//     tenant-level override — the caller decides).
//
// **Usage shape** (inside M7):
//
//	sender := invoker.New()
//	for {
//	    ep := sel.Pick()
//	    if ep == nil { break }
//	    handler := lookups.Get(ep, srcProto)
//	    outcome, err := sender.Send(ctx, ep, env, rawBody, handler)
//	    sel.Report(ep, outcome.ToScheduleResult())
//	    if outcome.Success() {
//	        sender.Forward(w, outcome.Response, outcome.Handler.NewResponseStream())
//	        break
//	    }
//	}
//
// See docs/architecture/03-endpoint-scheduling.md for details.
package invoker

import (
	"errors"
	"net"
	"net/http"
	"time"

	"github.com/zereker/llm-gateway/pkg/protocol"
	"github.com/zereker/llm-gateway/pkg/selector"
)

// HTTPDoer abstracts the HTTP client. *http.Client satisfies it
// automatically; tests can inject a RoundTripper-like fake.
type HTTPDoer interface {
	Do(*http.Request) (*http.Response, error)
}

// Stage marks which internal phase of Send produced this Outcome — used by
// the wiring layer to translate into dispatch.Stage, letting Policy.Decide
// distinguish a prepare failure from an invoke failure.
type Stage int

const (
	// StageInvoke is the HTTP-call phase (default; success / network error /
	// upstream 4xx-5xx all belong to this phase).
	StageInvoke Stage = iota
	// StagePrepare means handler.PrepareCall failed (pre-call protocol
	// conversion / vendor HTTP construction).
	StagePrepare
)

// Outcome is the result of Send.
//
// Success = Class==ClassSuccess && Response != nil. Response.Body is closed
// by the caller (usually via a defer Close inside Forward).
// Failure = Response==nil (Send has already closed the failed response's
// body itself).
type Outcome struct {
	Response *http.Response // populated only on success; nil on failure
	Stage    Stage          // which phase produced this Outcome
	Class    selector.ErrorClass
	HTTPCode int
	Reason   string
	Latency  time.Duration

	// RetryAfter is the upstream's own recovery hint parsed from a failed
	// response's Retry-After / rate-limit reset headers; 0 = no hint. Feeds
	// reset-aware cooldown (the TTL follows the upstream's stated reset time
	// instead of the static per-class duration).
	RetryAfter time.Duration

	// Handler is the protocol.Handler to use during Forward on the success
	// path; meaningless on failure. The caller calls
	// outcome.Handler.NewResponseStream() to get the response-stream
	// processor to pass to Forward.
	Handler protocol.Handler
}

// Success reports whether the outcome succeeded (HTTP 2xx + no
// protocol-level error).
func (o Outcome) Success() bool {
	return o.Class == selector.ClassSuccess && o.Response != nil
}

// ToScheduleResult converts to the selector.Result that sel.Report expects.
func (o Outcome) ToScheduleResult() selector.Result {
	return selector.Result{
		Class:      o.Class,
		HTTPCode:   o.HTTPCode,
		Reason:     o.Reason,
		Latency:    o.Latency,
		RetryAfter: o.RetryAfter,
	}
}

// ErrInvalidRequest is returned when Send fails to translate the request
// body (the caller should abort with 400 directly — do not retry, since
// switching endpoints for the same request will fail too).
var ErrInvalidRequest = errors.New("upstream: invalid request body")

// =============================================================================
// Sender
// =============================================================================

// Sender encapsulates two actions: "make one call to an upstream" and
// "stream forward".
//
// It holds no request-level state; both the Send and Forward methods can be
// called concurrently by multiple requests. **It holds no lookups at all**
// — adapter / translator / handler are all request-level dependencies that
// the caller passes through when calling Send.
type Sender struct {
	client HTTPDoer
	hooks  hookSet
}

// Option configures optional settings on Sender.
type Option func(*senderConfig)

// senderConfig holds the temporary configuration written by Option during
// New; New produces the Sender once configuration is finalized.
type senderConfig struct {
	client HTTPDoer
	hooks  []Hook
}

// WithHTTPClient injects a custom HTTP client; if not called, defaults to
// http.DefaultClient.
func WithHTTPClient(c HTTPDoer) Option {
	return func(cfg *senderConfig) { cfg.client = c }
}

// WithHooks registers a set of Hooks (observers). Multiple calls accumulate;
// when the same hook implements multiple Observer interfaces it lands in
// multiple buckets at once, each firing once at runtime per bucket.
//
// See hooks.go for details.
func WithHooks(hooks ...Hook) Option {
	return func(c *senderConfig) { c.hooks = append(c.hooks, hooks...) }
}

// per-attempt timeout boundaries (Transport-level, apply to all requests on
// this client):
//
//   - dialTimeout / tlsHandshakeTimeout: connection-establishment phase. A
//     hung endpoint (accepts but never responds / half-open connection)
//     fails fast here instead of burning through the whole request budget.
//   - responseHeaderTimeout: from finishing the request write to the
//     response header arriving (≈ TTFB). An LLM's first token can be slow
//     (long prompt / cold start), so 30s gives ample headroom while still
//     being **bounded** — leaving budget for retry / fallback (the
//     request-level total timeout defaults to 60s+).
//   - Response body read time is **not** limited: a streaming response can
//     legitimately run for minutes, backstopped by the request-level total
//     timeout (middleware.Timeout).
//
// **Why not http.DefaultClient**: it has no timeout at all (a hang means
// permanent occupancy), and DefaultTransport's MaxIdleConnsPerHost=2 means
// under high QPS against the same upstream host, connections get
// rebuilt frantically (latency + port exhaustion).
const (
	dialTimeout           = 5 * time.Second
	tlsHandshakeTimeout   = 5 * time.Second
	responseHeaderTimeout = 30 * time.Second
	idleConnTimeout       = 90 * time.Second
	maxIdleConns          = 512
	maxIdleConnsPerHost   = 128
)

// defaultHTTPClient is the default client for data-plane upstream calls;
// override it with WithHTTPClient when different parameters are needed
// (e.g. mTLS / proxy / custom timeouts).
func defaultHTTPClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			DialContext: (&net.Dialer{
				Timeout:   dialTimeout,
				KeepAlive: 30 * time.Second,
				// SSRF guard: block cloud metadata endpoints before dialing
				// (based on the resolved real IP, blocking DNS-rebinding).
				// Only blocks metadata, not self-hosted private-network
				// upstreams. See ssrf.go.
				Control: blockMetadataDial,
			}).DialContext,
			ForceAttemptHTTP2:     true,
			TLSHandshakeTimeout:   tlsHandshakeTimeout,
			ResponseHeaderTimeout: responseHeaderTimeout,
			IdleConnTimeout:       idleConnTimeout,
			MaxIdleConns:          maxIdleConns,
			MaxIdleConnsPerHost:   maxIdleConnsPerHost,
		},
		// Client.Timeout is intentionally not set — it covers body reading
		// and would cut off long streams. All phased timeouts live on the
		// Transport instead.
	}
}

// New constructs a Sender; with zero configuration it uses
// defaultHTTPClient + no hooks.
//
// protocol.Handler is passed in by the caller at Send time; Sender itself
// holds none, which supports per-request overrides for multi-tenant /
// canary scenarios.
func New(opts ...Option) *Sender {
	cfg := &senderConfig{
		client: defaultHTTPClient(),
	}

	for _, opt := range opts {
		opt(cfg)
	}

	return &Sender{
		client: cfg.client,
		hooks:  classifyHooks(cfg.hooks),
	}
}

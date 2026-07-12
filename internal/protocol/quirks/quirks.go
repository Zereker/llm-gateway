// Package quirks defines the endpoint-level request tweak DSL — the final
// correction pass applied after the upstream protocol translation.
//
// **Architecture position** (v0.7):
//
//	internal/protocol/combine.go PrepareCall:
//	  client body
//	    → translator.TranslateRequest                  (client protocol → upstream protocol shape)
//	    → ep.Quirks.RewriteBody + RewriteHeader        ← this package (body + header run in one pass)
//	    → protocol.Session.BuildRequest(body, headers) (HTTP envelope + merge quirks headers)
//	    → upstream
//
// **vendor Session merge rule**: the vendor Session copies quirks headers into
// req.Header first, then **writes** the protocol-required headers (Auth /
// Content-Type / vendor version headers) afterward — the later write wins.
// That way a deployer mistakenly overriding Authorization can't break the
// request, while still being free to add vendor-private headers.
//
// **Why this is needed**: the translator only handles the "client protocol →
// upstream protocol" shape conversion; within the same upstream protocol,
// different vendors / models still have subtle differences. Two typical
// categories of difference:
//
//	body fields
//	- OpenAI o1/o3/o4 reasoning models: max_tokens → max_completion_tokens; strip
//	  temperature / top_p / presence_penalty / frequency_penalty
//	- DeepSeek deepseek-reasoner: similar restrictions
//	- Anthropic Claude 3.7+ extended_thinking: insert a thinking block + force temperature=1
//	- vLLM / Ollama: strip certain OpenAI-specific fields
//
//	header fields
//	- different vendors use different trace-id header names (X-Request-Id / X-Trace-Id /
//	  X-Ark-Request-Id / x-ds-request-id, etc.) — the gateway always writes X-Request-Id
//	  internally, and a deployer-configured rename lets the upstream receive the header
//	  name it expects
//	- vendor-private headers (e.g. X-API-Version) need to be hardcoded on the endpoint
//
// **Why not bake this into the vendor Factory**: quirks is deployment
// knowledge, not code knowledge. The same vendor can easily have multiple
// endpoints deployed with different quirks (one for o1, one for gpt-4o), and
// vendor code shouldn't hardcode which model gets which rules. **The deployer
// configures it directly in the endpoints.quirks JSON column; cmd doesn't need
// a rebuild.**
//
// **DSL**: the endpoints.quirks column stores JSON like this:
//
//	{
//	  "body": {
//	    "rename":      {"max_tokens": "max_completion_tokens"},
//	    "strip":       ["temperature", "top_p"],
//	    "set":         {"reasoning_effort": "high"},
//	    "set_default": {"max_completion_tokens": 4096}
//	  },
//	  "headers": {
//	    "rename":      {"X-Request-Id": "X-Ark-Request-Id"},
//	    "strip":       ["X-Internal-Debug"],
//	    "set":         {"X-Custom-Tag": "prod"},
//	    "set_default": {"User-Agent": "llm-gateway/1.0"}
//	  }
//	}
//
// Either the body / headers sub-section can be omitted; all-empty / a NULL
// column = no-op. Within each sub-section the application order is fixed:
// rename → strip → set → set_default (first make room, then clean up, then
// overwrite, then fill in defaults last).
//
// **strict mode**: CompileJSON uses DisallowUnknownFields, so a typo'd field
// name fails immediately at compile time (combine.go turns it into a
// PhaseQuirks PrepareError).
package quirks

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// Rewriter is the runtime handle for the "upstream request tweaks" configured
// on an endpoint.
//
// **Lifecycle**: Compile / CompileJSON once, shared across many requests (no
// per-call state, concurrency-safe).
//
// **Usage**: combine.go calls the two methods RewriteBody + RewriteHeader
// after the translator and before protocol.Session.BuildRequest (RewriteHeader
// runs on a fresh http.Header{}), then passes the final body + header to
// protocol.Session.BuildRequest. Each step can independently no-op (when
// spec.Body or spec.Headers is empty on its own, the corresponding method
// short-circuits immediately).
type Rewriter interface {
	// RewriteBody rewrites body JSON fields. Returns the new body (may share
	// the same underlying slice as the input).
	RewriteBody(body []byte) ([]byte, error)
	// RewriteHeader rewrites the outgoing HTTP headers (in-place).
	RewriteHeader(h http.Header)
}

// Spec is the quirks ruleset configured on an endpoint. Both sub-sections are optional.
type Spec struct {
	Body    BodySpec    `json:"body,omitempty"`
	Headers HeadersSpec `json:"headers,omitempty"`
}

// BodySpec tweaks body JSON fields. All fields optional. Application order: Rename → Strip → Set → SetDefault.
type BodySpec struct {
	// Rename: from → to (deletes from, writes to; skipped if from doesn't exist).
	Rename map[string]string `json:"rename,omitempty"`
	// Strip: deletes the given keys.
	Strip []string `json:"strip,omitempty"`
	// Set: overwrites (replaces if present; adds if absent). Value is arbitrary JSON.
	Set map[string]json.RawMessage `json:"set,omitempty"`
	// SetDefault: only written if the key doesn't already exist.
	SetDefault map[string]json.RawMessage `json:"set_default,omitempty"`
}

// Empty reports whether the body spec is entirely empty.
func (s BodySpec) Empty() bool {
	return len(s.Rename) == 0 && len(s.Strip) == 0 &&
		len(s.Set) == 0 && len(s.SetDefault) == 0
}

// HeadersSpec tweaks HTTP headers. Same four operations, but values are all string (header value).
type HeadersSpec struct {
	Rename     map[string]string `json:"rename,omitempty"`
	Strip      []string          `json:"strip,omitempty"`
	Set        map[string]string `json:"set,omitempty"`
	SetDefault map[string]string `json:"set_default,omitempty"`
}

// Empty reports whether the header spec is entirely empty.
func (s HeadersSpec) Empty() bool {
	return len(s.Rename) == 0 && len(s.Strip) == 0 &&
		len(s.Set) == 0 && len(s.SetDefault) == 0
}

// Empty reports whether the whole Spec is empty — i.e. a no-op rewriter.
func (s Spec) Empty() bool {
	return s.Body.Empty() && s.Headers.Empty()
}

// Compile compiles a spec into a Rewriter; zero overhead (just wraps the spec in a struct implementing the interface).
func Compile(spec Spec) Rewriter {
	return &compiled{spec: spec}
}

// CompileJSON parses the endpoint.quirks JSON bytes and calls Compile.
//
//   - empty bytes / whitespace = returns a no-op Rewriter; no error
//   - parse failure (including unknown-field typos) = returns an error; the caller turns it into a PhaseQuirks PrepareError
//
// Strict mode via DisallowUnknownFields is enabled: a deployer typo in a field
// name (e.g. "strips" / "header") is surfaced immediately at compile time
// instead of being silently swallowed.
func CompileJSON(specJSON []byte) (Rewriter, error) {
	if len(bytes.TrimSpace(specJSON)) == 0 {
		return Compile(Spec{}), nil
	}

	dec := json.NewDecoder(bytes.NewReader(specJSON))
	dec.DisallowUnknownFields()

	var spec Spec
	if err := dec.Decode(&spec); err != nil {
		return nil, fmt.Errorf("quirks: parse spec: %w", err)
	}

	// Body keys are case-sensitive JSON; header names are case-insensitive.
	if err := checkRenameTargetCollision(spec.Body.Rename, false); err != nil {
		return nil, fmt.Errorf("quirks: body.rename: %w", err)
	}

	if err := checkRenameTargetCollision(spec.Headers.Rename, true); err != nil {
		return nil, fmt.Errorf("quirks: headers.rename: %w", err)
	}

	return Compile(spec), nil
}

// checkRenameTargetCollision rejects a rename map where two distinct source
// keys map to the same destination — applying those renames is order-dependent
// (Go map iteration is randomized), so the surviving value would vary per
// request. caseInsensitive folds destinations to lower case (header names).
func checkRenameTargetCollision(rename map[string]string, caseInsensitive bool) error {
	if len(rename) < 2 {
		return nil
	}

	seen := make(map[string]string, len(rename))
	for from, to := range rename {
		key := to
		if caseInsensitive {
			key = strings.ToLower(to)
		}

		if prev, dup := seen[key]; dup {
			return fmt.Errorf("multiple sources rename to %q (%q and %q)", to, prev, from)
		}

		seen[key] = from
	}

	return nil
}

// compiled is the implementation returned by Compile.
type compiled struct {
	spec Spec
}

// RewriteBody applies the body spec in rename → strip → set → set_default order.
func (c *compiled) RewriteBody(body []byte) ([]byte, error) {
	if c == nil || c.spec.Body.Empty() {
		return body, nil
	}

	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("quirks: parse body: %w", err)
	}

	for from, to := range c.spec.Body.Rename {
		if v, ok := m[from]; ok {
			m[to] = v
			delete(m, from)
		}
	}

	for _, k := range c.spec.Body.Strip {
		delete(m, k)
	}

	for k, v := range c.spec.Body.Set {
		m[k] = v
	}

	for k, v := range c.spec.Body.SetDefault {
		if _, exists := m[k]; !exists {
			m[k] = v
		}
	}

	out, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("quirks: re-marshal: %w", err)
	}

	return out, nil
}

// RewriteHeader applies the header spec in rename → strip → set → set_default order (in-place).
//
// **Header name normalization**: http.Header internally stores canonical form
// (X-Request-Id); we call http.CanonicalHeaderKey here so deployers can write
// either case in config.
//
// **rename semantics**: from canonical → to canonical; skipped if from doesn't exist. Multiple values are moved together.
// **strip semantics**: deletes all values for the canonical key.
// **set semantics**: Del first then Set (replaces all values with a single one).
// **set_default**: only Sets a value if the canonical key doesn't already exist.
func (c *compiled) RewriteHeader(h http.Header) {
	if c == nil || c.spec.Headers.Empty() || h == nil {
		return
	}

	for from, to := range c.spec.Headers.Rename {
		fromKey := http.CanonicalHeaderKey(from)
		toKey := http.CanonicalHeaderKey(to)

		vals := h.Values(fromKey)
		if len(vals) == 0 {
			continue
		}

		h.Del(fromKey)
		// Move multiple values together: Set the first, Add the rest
		h.Set(toKey, vals[0])

		for _, v := range vals[1:] {
			h.Add(toKey, v)
		}
	}

	for _, k := range c.spec.Headers.Strip {
		h.Del(k)
	}

	for k, v := range c.spec.Headers.Set {
		h.Set(k, v)
	}

	for k, v := range c.spec.Headers.SetDefault {
		canonical := http.CanonicalHeaderKey(k)
		if _, exists := h[canonical]; !exists {
			h.Set(canonical, v)
		}
	}
}

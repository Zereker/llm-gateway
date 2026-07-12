package moderation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"

	"github.com/zereker/llm-gateway/internal/domain"
)

// DenylistGuard is a regex-based content-blocking guard — a match on any
// pattern blocks the request.
//
// A cheap, deterministic guardrail (PII keywords / sensitive terms / prompt
// injection probes, etc.), applied ahead of the potentially expensive LLM
// moderator. CheckInput scans the request body (env.RawBytes); when
// check_output=true it also scans the response chunk by chunk.
//
// **Doesn't leak the matched pattern**: the block error only says "blocked by
// content policy", to avoid exposing the deny rules to client probing via the
// 400 body (M8 splices the error string into the response). Match details go
// to the span/log instead.
//
// **Streaming output is best-effort, not a hard guarantee**: check_output
// scans **already-translated SSE-framed bytes** (data: {...}\n\n) chunk by
// chunk, not the decoded body text. Under streaming, each token typically
// forms its own frame with JSON/SSE framing in between, so patterns split
// across frames (e.g. the body text "kill" cut into "ki"/"ll" across two
// frames) can't be scanned — even buffering across chunks can't reassemble
// the continuous body text. To **fully block** a violating output you must
// use the non-streaming path (buffer-then-scan): the non-streaming path's
// Flush delivers the entire body at once, so the scan covers the complete
// text and can truly block it. Security-critical denylists should be paired
// with non-streaming mode; under streaming it can only catch patterns that
// fall within a single frame. CheckInput (pre-side, scanning the whole body
// at once) isn't subject to this limitation.
type DenylistGuard struct {
	patterns    []*regexp.Regexp
	checkOutput bool
}

// ErrDenied is the generic block error (does not include the matched pattern).
var ErrDenied = errors.New("blocked by content policy")

// NewDenylistGuard compiles the patterns (Go RE2 syntax). Any compile failure
// returns an error immediately (startup fail-fast).
func NewDenylistGuard(patterns []string, checkOutput bool) (*DenylistGuard, error) {
	compiled := make([]*regexp.Regexp, 0, len(patterns))
	for _, p := range patterns {
		re, err := regexp.Compile(p)
		if err != nil {
			return nil, fmt.Errorf("denylist: invalid pattern %q: %w", p, err)
		}

		compiled = append(compiled, re)
	}

	return &DenylistGuard{patterns: compiled, checkOutput: checkOutput}, nil
}

// CheckInput scans the request body.
//
// It scans the raw bytes AND, when the body is JSON, every decoded string
// value. Scanning only the raw bytes would let a client smuggle a banned term
// past the regex as JSON unicode escapes (e.g. "kill" for
// "kill"): the raw bytes don't contain the literal term, but the upstream JSON
// parser decodes it normally. Decoding string values first closes that bypass.
func (g *DenylistGuard) CheckInput(_ context.Context, env *domain.RequestEnvelope) error {
	if env == nil {
		return nil
	}

	if err := g.scan(env.RawBytes); err != nil {
		return err
	}

	for _, s := range decodeJSONStrings(env.RawBytes) {
		if err := g.scan([]byte(s)); err != nil {
			return err
		}
	}

	return nil
}

// decodeJSONStrings walks an arbitrary JSON document and returns every string
// value (object keys and array/nested elements included), with JSON escapes
// already decoded by the parser. Returns nil when the body isn't valid JSON —
// the raw-byte scan still applies in that case.
func decodeJSONStrings(b []byte) []string {
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return nil
	}

	var (
		out  []string
		walk func(any)
	)

	walk = func(node any) {
		switch t := node.(type) {
		case string:
			out = append(out, t)
		case []any:
			for _, e := range t {
				walk(e)
			}
		case map[string]any:
			for k, e := range t {
				out = append(out, k)

				walk(e)
			}
		}
	}
	walk(v)

	return out
}

// CheckOutput scans the response chunk by chunk (only when check_output=true).
//
// **Under streaming, chunk is the bytes of a single SSE frame** — patterns
// split across frames can't be scanned (see the type doc). In non-streaming
// mode (buffer-then-translate), Flush delivers the entire body as one chunk,
// so the scan covers the complete body text and is a true hard guarantee.
func (g *DenylistGuard) CheckOutput(_ context.Context, chunk []byte) error {
	if !g.checkOutput {
		return nil
	}

	return g.scan(chunk)
}

func (g *DenylistGuard) scan(b []byte) error {
	for _, re := range g.patterns {
		if re.Match(b) {
			return ErrDenied
		}
	}

	return nil
}

// Compile-time assertion.
var _ Moderator = (*DenylistGuard)(nil)

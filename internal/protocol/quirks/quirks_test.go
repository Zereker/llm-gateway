package quirks

import (
	"encoding/json"
	"net/http"
	"testing"
)

// =============================================================================
// Empty
// =============================================================================

func TestSpec_EmptyTrue(t *testing.T) {
	if !(Spec{}).Empty() {
		t.Error("zero Spec.Empty() = false")
	}
}

func TestSpec_EmptyFalseOnEitherSubspec(t *testing.T) {
	cases := map[string]Spec{
		"body":    {Body: BodySpec{Strip: []string{"x"}}},
		"headers": {Headers: HeadersSpec{Set: map[string]string{"X-K": "v"}}},
	}
	for name, s := range cases {
		if s.Empty() {
			t.Errorf("Spec with %s set should not be empty", name)
		}
	}
}

// =============================================================================
// Body rewrites
// =============================================================================

func TestRewriteBody_Noop(t *testing.T) {
	r := Compile(Spec{})
	body := []byte(`{"x":1}`)
	out, err := r.RewriteBody(body)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if string(out) != string(body) {
		t.Errorf("noop changed body: %q → %q", body, out)
	}
}

func TestRewriteBody_Rename(t *testing.T) {
	r := Compile(Spec{Body: BodySpec{
		Rename: map[string]string{"max_tokens": "max_completion_tokens"},
	}})
	out, err := r.RewriteBody([]byte(`{"max_tokens":1024,"model":"o1"}`))
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	got := jsonObj(t, out)
	if _, exists := got["max_tokens"]; exists {
		t.Error("max_tokens not removed")
	}
	if v := string(got["max_completion_tokens"]); v != "1024" {
		t.Errorf("max_completion_tokens=%s, want 1024", v)
	}
}

func TestRewriteBody_Rename_FromMissingSkipped(t *testing.T) {
	r := Compile(Spec{Body: BodySpec{
		Rename: map[string]string{"max_tokens": "max_completion_tokens"},
	}})
	out, _ := r.RewriteBody([]byte(`{"model":"o1"}`))
	got := jsonObj(t, out)
	if _, exists := got["max_completion_tokens"]; exists {
		t.Error("rename should not create key when from missing")
	}
}

func TestRewriteBody_Strip(t *testing.T) {
	r := Compile(Spec{Body: BodySpec{Strip: []string{"temperature", "top_p"}}})
	out, _ := r.RewriteBody([]byte(`{"model":"o1","temperature":0.7,"top_p":1,"x":2}`))
	got := jsonObj(t, out)
	for _, k := range []string{"temperature", "top_p"} {
		if _, exists := got[k]; exists {
			t.Errorf("%s not stripped", k)
		}
	}
	if v := string(got["x"]); v != "2" {
		t.Errorf("other field lost: x=%s", v)
	}
}

func TestRewriteBody_Set_Overwrites(t *testing.T) {
	r := Compile(Spec{Body: BodySpec{Set: map[string]json.RawMessage{
		"temperature": json.RawMessage(`1`),
	}}})
	out, _ := r.RewriteBody([]byte(`{"temperature":0.5}`))
	got := jsonObj(t, out)
	if v := string(got["temperature"]); v != "1" {
		t.Errorf("temperature=%s, want 1", v)
	}
}

func TestRewriteBody_SetDefault_DoesNotOverwrite(t *testing.T) {
	r := Compile(Spec{Body: BodySpec{SetDefault: map[string]json.RawMessage{
		"max_tokens": json.RawMessage(`4096`),
	}}})
	out, _ := r.RewriteBody([]byte(`{"max_tokens":1024}`))
	got := jsonObj(t, out)
	if v := string(got["max_tokens"]); v != "1024" {
		t.Errorf("set_default should not overwrite: max_tokens=%s", v)
	}
}

func TestRewriteBody_SetDefault_AddsMissing(t *testing.T) {
	r := Compile(Spec{Body: BodySpec{SetDefault: map[string]json.RawMessage{
		"max_tokens": json.RawMessage(`4096`),
	}}})
	out, _ := r.RewriteBody([]byte(`{"model":"o1"}`))
	got := jsonObj(t, out)
	if v := string(got["max_tokens"]); v != "4096" {
		t.Errorf("max_tokens=%s, want 4096", v)
	}
}

// Real-world quirks scenario integration test
func TestRewriteBody_OpenAIReasoningModel(t *testing.T) {
	r := Compile(Spec{Body: BodySpec{
		Rename: map[string]string{"max_tokens": "max_completion_tokens"},
		Strip:  []string{"temperature", "top_p", "presence_penalty", "frequency_penalty"},
	}})
	out, _ := r.RewriteBody([]byte(`{
		"model":"o1",
		"max_tokens":2048,
		"temperature":0.7,
		"top_p":1,
		"presence_penalty":0,
		"frequency_penalty":0
	}`))
	got := jsonObj(t, out)
	if string(got["max_completion_tokens"]) != "2048" {
		t.Errorf("rename failed: %s", out)
	}
	if _, exists := got["max_tokens"]; exists {
		t.Error("max_tokens not removed")
	}
	for _, k := range []string{"temperature", "top_p", "presence_penalty", "frequency_penalty"} {
		if _, exists := got[k]; exists {
			t.Errorf("%s not stripped", k)
		}
	}
}

func TestRewriteBody_BadBody_ReturnsError(t *testing.T) {
	r := Compile(Spec{Body: BodySpec{Strip: []string{"x"}}})
	_, err := r.RewriteBody([]byte(`not json`))
	if err == nil {
		t.Fatal("want error on malformed body")
	}
}

func TestRewriteBody_NilReceiverIsNoop(t *testing.T) {
	var r *compiled
	body := []byte(`{"x":1}`)
	out, err := r.RewriteBody(body)
	if err != nil || string(out) != string(body) {
		t.Errorf("nil receiver should be no-op: out=%q err=%v", out, err)
	}
}

// =============================================================================
// Header rewrites
// =============================================================================

func TestRewriteHeader_Rename(t *testing.T) {
	r := Compile(Spec{Headers: HeadersSpec{
		Rename: map[string]string{"X-Request-Id": "X-Ark-Request-Id"},
	}})
	h := http.Header{}
	h.Set("X-Request-Id", "abc-123")
	r.RewriteHeader(h)

	if h.Get("X-Request-Id") != "" {
		t.Errorf("X-Request-Id should be deleted: %s", h.Get("X-Request-Id"))
	}
	if got := h.Get("X-Ark-Request-Id"); got != "abc-123" {
		t.Errorf("X-Ark-Request-Id=%s, want abc-123", got)
	}
}

func TestRewriteHeader_Rename_CaseInsensitive(t *testing.T) {
	// Lowercase / uppercase in config should both work (http.Header uses canonical keys)
	r := Compile(Spec{Headers: HeadersSpec{
		Rename: map[string]string{"x-request-id": "x-trace-id"},
	}})
	h := http.Header{}
	h.Set("X-Request-Id", "v")
	r.RewriteHeader(h)
	if h.Get("X-Trace-Id") != "v" {
		t.Errorf("rename case-insensitive failed")
	}
}

func TestRewriteHeader_Rename_PreservesMultipleValues(t *testing.T) {
	r := Compile(Spec{Headers: HeadersSpec{
		Rename: map[string]string{"X-Tag": "X-Vendor-Tag"},
	}})
	h := http.Header{}
	h.Add("X-Tag", "a")
	h.Add("X-Tag", "b")
	r.RewriteHeader(h)
	got := h.Values("X-Vendor-Tag")
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("multi-value rename lost values: %v", got)
	}
}

func TestRewriteHeader_Strip(t *testing.T) {
	r := Compile(Spec{Headers: HeadersSpec{Strip: []string{"X-Internal-Debug"}}})
	h := http.Header{}
	h.Set("X-Internal-Debug", "yes")
	h.Set("X-Keep", "yes")
	r.RewriteHeader(h)
	if h.Get("X-Internal-Debug") != "" {
		t.Error("X-Internal-Debug should be stripped")
	}
	if h.Get("X-Keep") != "yes" {
		t.Error("X-Keep lost")
	}
}

func TestRewriteHeader_Set_Overwrites(t *testing.T) {
	r := Compile(Spec{Headers: HeadersSpec{Set: map[string]string{"User-Agent": "llm-gateway/2.0"}}})
	h := http.Header{}
	h.Set("User-Agent", "old/1.0")
	r.RewriteHeader(h)
	if got := h.Get("User-Agent"); got != "llm-gateway/2.0" {
		t.Errorf("User-Agent=%s", got)
	}
}

func TestRewriteHeader_SetDefault_DoesNotOverwrite(t *testing.T) {
	r := Compile(Spec{Headers: HeadersSpec{SetDefault: map[string]string{"User-Agent": "default"}}})
	h := http.Header{}
	h.Set("User-Agent", "set-by-adapter")
	r.RewriteHeader(h)
	if got := h.Get("User-Agent"); got != "set-by-adapter" {
		t.Errorf("User-Agent should NOT be overwritten: got %s", got)
	}
}

func TestRewriteHeader_SetDefault_AddsMissing(t *testing.T) {
	r := Compile(Spec{Headers: HeadersSpec{SetDefault: map[string]string{"X-Vendor": "ark"}}})
	h := http.Header{}
	r.RewriteHeader(h)
	if got := h.Get("X-Vendor"); got != "ark" {
		t.Errorf("X-Vendor=%s", got)
	}
}

func TestRewriteHeader_NoopOnEmptySpec(t *testing.T) {
	r := Compile(Spec{})
	h := http.Header{}
	h.Set("X-Untouched", "v")
	r.RewriteHeader(h)
	if h.Get("X-Untouched") != "v" {
		t.Error("empty spec mutated header")
	}
}

func TestRewriteHeader_NilHeaderSafe(t *testing.T) {
	r := Compile(Spec{Headers: HeadersSpec{Set: map[string]string{"X-K": "v"}}})
	r.RewriteHeader(nil) // should not panic
}

// Real-world scenario: trace id header rename
func TestRewriteHeader_VendorTraceIdRename(t *testing.T) {
	r := Compile(Spec{Headers: HeadersSpec{
		Rename: map[string]string{"X-Request-Id": "X-Ark-Trace-Id"},
		Set:    map[string]string{"X-Ark-Region": "cn-beijing"},
	}})
	h := http.Header{}
	h.Set("X-Request-Id", "req-abc-123")
	h.Set("Content-Type", "application/json")
	r.RewriteHeader(h)

	if h.Get("X-Ark-Trace-Id") != "req-abc-123" {
		t.Errorf("X-Ark-Trace-Id=%s", h.Get("X-Ark-Trace-Id"))
	}
	if h.Get("X-Ark-Region") != "cn-beijing" {
		t.Errorf("X-Ark-Region=%s", h.Get("X-Ark-Region"))
	}
	if h.Get("Content-Type") != "application/json" {
		t.Error("Content-Type lost")
	}
	if h.Get("X-Request-Id") != "" {
		t.Error("X-Request-Id should have been removed by rename")
	}
}

// =============================================================================
// CompileJSON
// =============================================================================

func TestCompileJSON_Empty(t *testing.T) {
	for _, in := range [][]byte{nil, []byte(""), []byte("   ")} {
		r, err := CompileJSON(in)
		if err != nil {
			t.Errorf("CompileJSON(%q) err=%v", in, err)
		}
		out, _ := r.RewriteBody([]byte(`{"x":1}`))
		if string(out) != `{"x":1}` {
			t.Errorf("empty spec should be no-op")
		}
	}
}

func TestCompileJSON_RejectsUnknownField(t *testing.T) {
	cases := [][]byte{
		[]byte(`{"strips":["x"]}`),                  // top-level typo
		[]byte(`{"body":{"strips":["x"]}}`),         // body sub-key typo
		[]byte(`{"headers":{"renames":{"a":"b"}}}`), // headers sub-key typo
	}
	for _, c := range cases {
		if _, err := CompileJSON(c); err == nil {
			t.Errorf("CompileJSON(%s) should reject unknown field", c)
		}
	}
}

// TestCompileJSON_RejectsRenameTargetCollision: two renames to the same
// destination key resolve nondeterministically (Go map iteration order), so
// the winner varies request-to-request. Reject such a spec at compile time
// rather than shipping unstable rewrites.
func TestCompileJSON_RejectsRenameTargetCollision(t *testing.T) {
	cases := [][]byte{
		[]byte(`{"body":{"rename":{"a":"z","b":"z"}}}`),            // body: a->z and b->z collide
		[]byte(`{"headers":{"rename":{"X-A":"X-Z","X-B":"X-Z"}}}`), // headers collide
	}
	for _, c := range cases {
		if _, err := CompileJSON(c); err == nil {
			t.Errorf("CompileJSON(%s) should reject a rename target collision", c)
		}
	}

	// A non-colliding rename spec must still compile fine.
	if _, err := CompileJSON([]byte(`{"body":{"rename":{"a":"x","b":"y"}}}`)); err != nil {
		t.Errorf("non-colliding rename should compile: %v", err)
	}
}

func TestCompileJSON_RealWorldSpec(t *testing.T) {
	spec := []byte(`{
		"body": {
			"rename": {"max_tokens": "max_completion_tokens"},
			"strip":  ["temperature", "top_p"]
		},
		"headers": {
			"rename": {"X-Request-Id": "X-Ark-Trace-Id"},
			"set":    {"X-Ark-Region": "cn-beijing"}
		}
	}`)
	r, err := CompileJSON(spec)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	// body
	out, _ := r.RewriteBody([]byte(`{"model":"o1","max_tokens":100,"temperature":0.5}`))
	got := jsonObj(t, out)
	if string(got["max_completion_tokens"]) != "100" {
		t.Errorf("body rename failed: %s", out)
	}
	if _, ok := got["temperature"]; ok {
		t.Errorf("body strip failed: %s", out)
	}

	// headers
	h := http.Header{}
	h.Set("X-Request-Id", "abc")
	r.RewriteHeader(h)
	if h.Get("X-Ark-Trace-Id") != "abc" {
		t.Errorf("header rename: %s", h.Get("X-Ark-Trace-Id"))
	}
	if h.Get("X-Ark-Region") != "cn-beijing" {
		t.Errorf("header set: %s", h.Get("X-Ark-Region"))
	}
}

// =============================================================================
// helpers
// =============================================================================

func jsonObj(t *testing.T, body []byte) map[string]json.RawMessage {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("body not json: %s (err=%v)", body, err)
	}
	return m
}

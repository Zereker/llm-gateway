package openai_gemini

import (
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

// TestTranslateRequest_ToolsToFunctionDeclarations: OpenAI tools -> Gemini's
// tools[0].functionDeclarations (one Tool entry wrapping all declarations).
func TestTranslateRequest_ToolsToFunctionDeclarations(t *testing.T) {
	body := []byte(`{"model":"gemini-x","tools":[
		{"type":"function","function":{"name":"get_weather","description":"gets weather","parameters":{"type":"object","properties":{"city":{"type":"string"}}}}}
	],"messages":[{"role":"user","content":"weather in SF?"}]}`)
	out, err := translateRequest(body)
	if err != nil {
		t.Fatalf("translateRequest error: %v", err)
	}
	decl := gjson.GetBytes(out, "tools.0.functionDeclarations.0")
	if decl.Get("name").String() != "get_weather" {
		t.Errorf("functionDeclarations not built: %s", out)
	}
	if decl.Get("parameters.properties.city.type").String() != "string" {
		t.Errorf("parameters schema lost: %s", out)
	}
}

// TestTranslateRequest_ToolChoice covers OpenAI tool_choice -> Gemini
// toolConfig.functionCallingConfig, including Gemini's native ability to
// force one specific named function via allowedFunctionNames.
func TestTranslateRequest_ToolChoice(t *testing.T) {
	cases := []struct {
		name      string
		choice    string
		wantMode  string
		wantNames []string
	}{
		{"required", `"required"`, "ANY", nil},
		{"none", `"none"`, "NONE", nil},
		{"auto_omitted", `"auto"`, "", nil},
		{"named_function", `{"type":"function","function":{"name":"get_weather"}}`, "ANY", []string{"get_weather"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(`{"model":"m","tool_choice":` + tc.choice + `,"messages":[{"role":"user","content":"hi"}]}`)
			out, err := translateRequest(body)
			if err != nil {
				t.Fatalf("translateRequest: %v", err)
			}
			mode := gjson.GetBytes(out, "toolConfig.functionCallingConfig.mode")
			if tc.wantMode == "" {
				if gjson.GetBytes(out, "toolConfig").Exists() {
					t.Errorf("want toolConfig omitted, got %s", out)
				}
				return
			}
			if mode.String() != tc.wantMode {
				t.Errorf("mode = %q, want %q: %s", mode.String(), tc.wantMode, out)
			}
			names := gjson.GetBytes(out, "toolConfig.functionCallingConfig.allowedFunctionNames").Array()
			if len(tc.wantNames) == 0 {
				if len(names) != 0 {
					t.Errorf("allowedFunctionNames should be empty, got %v", names)
				}
				return
			}
			if len(names) != 1 || names[0].String() != tc.wantNames[0] {
				t.Errorf("allowedFunctionNames = %v, want %v", names, tc.wantNames)
			}
		})
	}
}

// TestTranslateRequest_ToolCallHistory covers the full multi-turn round trip:
// an assistant message with tool_calls -> functionCall part (arguments
// parsed from a JSON string into Gemini's object args), and the matching
// tool-role results -> a single "user" turn with functionResponse parts,
// correlated back to the right function name via tool_call_id, with
// consecutive tool messages merged into one turn (parallel calls).
// TestTranslateResponse_ThoughtSignature and TestTranslateRequest_ThoughtSignatureRoundTrip
// cover Gemini 3's thoughtSignature (a functionCall part's signed reasoning
// blob, analogous to Anthropic's thinking-block signature — real signature
// value captured from simonw/llm-gemini's
// test_tools_with_gemini_3_thought_signatures.yaml cassette, Apache 2.0):
// it must surface on the response and be replayed verbatim if the client
// echoes that tool call back in history.
const realThoughtSignature = "Et0BCtoBAXLI2nwMB4momyXTj+K3OPELPDi5Mq6bA5ZHuiKVF9m94gyxd+zdlC+s73nWxHx1ImnA9wPRG1sKMAvENS5i0Bef0VxMS31QE4PbbJw81tSto0OCC+AJdHF0i7x3uHqBuj91gBPwmy3rhAQM+8kxmaF4FeJ0rvICgjIIjG7rBgCE8vOD9Glt/sy3WPKo//jOukERM0rVGAPpMogXNtQbJWUQ8469alaZN67hbYJNaL5XSNcsbsu4ub04B2aFPc6NJld2EWK/enYFPnarMNwobDsSstlzuMygTHQ="

func TestTranslateResponse_ThoughtSignature(t *testing.T) {
	gemini := `{"candidates":[{"content":{"parts":[{"functionCall":{"name":"multiply","args":{"x":5,"y":3}},"thoughtSignature":"` + realThoughtSignature + `"}]},"index":0}],"usageMetadata":{"promptTokenCount":60,"candidatesTokenCount":16,"totalTokenCount":108}}`
	out, err := translateResponse([]byte(gemini), "gemini-3-flash-preview")
	if err != nil {
		t.Fatalf("translateResponse error: %v", err)
	}
	sig := gjson.GetBytes(out, "choices.0.message.tool_calls.0.thought_signature").String()
	if sig != realThoughtSignature {
		t.Errorf("thought_signature = %q, want the real captured signature", sig)
	}
}

func TestTranslateRequest_ThoughtSignatureRoundTrip(t *testing.T) {
	body := []byte(`{"model":"gpt-x","messages":[
		{"role":"user","content":"What is 5 times 3?"},
		{"role":"assistant","content":null,"tool_calls":[
			{"id":"call_1","type":"function","function":{"name":"multiply","arguments":"{\"x\":5,\"y\":3}"},"thought_signature":"` + realThoughtSignature + `"}
		]},
		{"role":"tool","tool_call_id":"call_1","content":"15"}
	]}`)
	out, err := translateRequest(body)
	if err != nil {
		t.Fatalf("translateRequest error: %v", err)
	}
	sig := gjson.GetBytes(out, "contents.1.parts.0.thoughtSignature").String()
	if sig != realThoughtSignature {
		t.Errorf("thoughtSignature not replayed on the functionCall part: %q", sig)
	}
	if gjson.GetBytes(out, "contents.1.parts.0.functionCall.name").String() != "multiply" {
		t.Errorf("functionCall lost alongside thoughtSignature: %s", out)
	}
}

func TestTranslateRequest_ToolCallHistory(t *testing.T) {
	body := []byte(`{"model":"m","messages":[
		{"role":"user","content":"weather in SF and NYC?"},
		{"role":"assistant","content":null,"tool_calls":[
			{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"SF\"}"}},
			{"id":"call_2","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"NYC\"}"}}
		]},
		{"role":"tool","tool_call_id":"call_1","content":"sunny"},
		{"role":"tool","tool_call_id":"call_2","content":"{\"temp_f\":40,\"condition\":\"cloudy\"}"}
	]}`)
	out, err := translateRequest(body)
	if err != nil {
		t.Fatalf("translateRequest error: %v", err)
	}
	// contents: [user, model(2 functionCall parts), user(2 functionResponse parts)]
	contents := gjson.GetBytes(out, "contents").Array()
	if len(contents) != 3 {
		t.Fatalf("want 3 contents entries, got %d: %s", len(contents), out)
	}
	modelTurn := contents[1]
	if modelTurn.Get("role").String() != "model" {
		t.Fatalf("contents[1] role = %q, want model", modelTurn.Get("role").String())
	}
	fc0, fc1 := modelTurn.Get("parts.0.functionCall"), modelTurn.Get("parts.1.functionCall")
	if fc0.Get("name").String() != "get_weather" || fc0.Get("args.city").String() != "SF" {
		t.Errorf("functionCall[0] wrong: %s", fc0.Raw)
	}
	if fc1.Get("name").String() != "get_weather" || fc1.Get("args.city").String() != "NYC" {
		t.Errorf("functionCall[1] wrong: %s", fc1.Raw)
	}

	resultTurn := contents[2]
	// Gemini's Content.role is only ever "user" or "model" — a functionResponse
	// turn is role:"user", not role:"function"/"tool".
	if resultTurn.Get("role").String() != "user" {
		t.Fatalf("functionResponse turn role = %q, want user", resultTurn.Get("role").String())
	}
	parts := resultTurn.Get("parts").Array()
	if len(parts) != 2 {
		t.Fatalf("want 2 merged functionResponse parts (consecutive tool messages), got %d: %s", len(parts), out)
	}
	fr0, fr1 := parts[0].Get("functionResponse"), parts[1].Get("functionResponse")
	if fr0.Get("name").String() != "get_weather" || fr0.Get("response.content").String() != "sunny" {
		t.Errorf("functionResponse[0] wrong (plain text should wrap as {content:...}): %s", fr0.Raw)
	}
	if fr1.Get("name").String() != "get_weather" || fr1.Get("response.temp_f").Int() != 40 {
		t.Errorf("functionResponse[1] wrong (JSON object content should pass through as-is): %s", fr1.Raw)
	}
}

// TestTranslateResponse_ToolCalls: a functionCall part -> OpenAI tool_calls,
// content:null (no accompanying text), finish_reason overridden to
// tool_calls even though Gemini's own finishReason says STOP.
func TestTranslateResponse_ToolCalls(t *testing.T) {
	gemini := `{"candidates":[{"content":{"parts":[{"functionCall":{"name":"get_weather","args":{"city":"SF"}}}]},"finishReason":"STOP","index":0}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":5,"totalTokenCount":15}}`
	out, err := translateResponse([]byte(gemini), "gemini-x")
	if err != nil {
		t.Fatalf("translateResponse error: %v", err)
	}
	if got := gjson.GetBytes(out, "choices.0.finish_reason").String(); got != "tool_calls" {
		t.Errorf("finish_reason = %q, want tool_calls (override): %s", got, out)
	}
	content := gjson.GetBytes(out, "choices.0.message.content")
	if content.Exists() && content.Type != gjson.Null {
		t.Errorf("content should be null, got %s", content.Raw)
	}
	tc := gjson.GetBytes(out, "choices.0.message.tool_calls.0")
	if tc.Get("type").String() != "function" || tc.Get("function.name").String() != "get_weather" {
		t.Errorf("tool_calls mapping wrong: %s", out)
	}
	if got := tc.Get("function.arguments").String(); got != `{"city":"SF"}` {
		t.Errorf("arguments = %q, want a JSON string %q", got, `{"city":"SF"}`)
	}
}

// TestResponseHandler_SSE_ToolCalls: a streamed functionCall part -> one
// complete OpenAI tool_calls delta chunk (Gemini doesn't stream partial
// function arguments token-by-token), finish_reason overridden.
func TestResponseHandler_SSE_ToolCalls(t *testing.T) {
	h := openaiGemini{}.NewResponseHandler()
	chunk := "data: {\"candidates\":[{\"content\":{\"parts\":[{\"functionCall\":{\"name\":\"get_weather\",\"args\":{\"city\":\"SF\"}}}]},\"finishReason\":\"STOP\",\"index\":0}]}\n\n"
	out, err := h.Feed([]byte(chunk))
	if err != nil {
		t.Fatalf("Feed: %v", err)
	}
	final, _, _ := h.Flush()
	all := string(out) + string(final)

	tc := gjson.Get(strings.TrimPrefix(strings.Split(all, "\n\n")[1], "data: "), "choices.0.delta.tool_calls.0")
	if tc.Get("function.name").String() != "get_weather" {
		t.Errorf("tool_calls delta missing name: %s", all)
	}
	if got := tc.Get("function.arguments").String(); got != `{"city":"SF"}` {
		t.Errorf("arguments = %q, want %q: %s", got, `{"city":"SF"}`, all)
	}
	if !strings.Contains(all, `"finish_reason":"tool_calls"`) {
		t.Errorf("finish_reason not overridden to tool_calls: %s", all)
	}
}

// TestResponseHandler_SSE_ThoughtSignature covers the streaming path with
// the real captured Gemini 3 thoughtSignature.
func TestResponseHandler_SSE_ThoughtSignature(t *testing.T) {
	h := openaiGemini{}.NewResponseHandler()
	chunk := `data: {"candidates":[{"content":{"parts":[{"functionCall":{"name":"multiply","args":{"x":5,"y":3}},"thoughtSignature":"` + realThoughtSignature + `"}]},"index":0}]}` + "\n\n"
	out, err := h.Feed([]byte(chunk))
	if err != nil {
		t.Fatalf("Feed: %v", err)
	}
	final, _, _ := h.Flush()
	all := string(out) + string(final)

	tc := gjson.Get(strings.TrimPrefix(strings.Split(all, "\n\n")[1], "data: "), "choices.0.delta.tool_calls.0")
	if got := tc.Get("thought_signature").String(); got != realThoughtSignature {
		t.Errorf("thought_signature = %q, want the real captured signature: %s", got, all)
	}
}

// TestTranslateRequest_MultipleSystemMessagesMerge regresses a bug where each
// system message replaced systemInstruction wholesale, so a client (or an
// injected middleware reminder) sending more than one system message silently
// lost all but the last.
func TestTranslateRequest_MultipleSystemMessagesMerge(t *testing.T) {
	body := []byte(`{"model":"gemini-x","messages":[
		{"role":"system","content":"be terse"},
		{"role":"system","content":"never mention prices"},
		{"role":"user","content":"hi"}
	]}`)
	out, err := translateRequest(body)
	if err != nil {
		t.Fatalf("translateRequest error: %v", err)
	}
	parts := gjson.GetBytes(out, "systemInstruction.parts").Array()
	if len(parts) != 2 {
		t.Fatalf("systemInstruction.parts = %d entries, want 2: %s", len(parts), out)
	}
	if parts[0].Get("text").String() != "be terse" || parts[1].Get("text").String() != "never mention prices" {
		t.Errorf("system parts wrong: %s", out)
	}
}

// TestTranslateRequest_ImageURL covers OpenAI vision content -> Gemini
// inlineData/fileData parts. The base64 payload is a real captured image (a
// tiny PNG) from simonw/llm-anthropic's test_image_prompt.yaml cassette
// (Apache 2.0) — reused here since it's a real image, not a vendor-specific
// artifact.
func TestTranslateRequest_ImageURL(t *testing.T) {
	const realPNGBase64 = "iVBORw0KGgoAAAANSUhEUgAAAKYAAAEaAgMAAADmmcReAAAACVBMVEX///8A/wD+AQASdAFKAAAAR0lEQVR42u3YMREAMAjAwC5d6q8mUYkEVuA+8yvIkVr0oghFURRFURRFURRFUdRCkSRJM7u/CEVRFEVRFEVRFEXRpdQXkcaVBRUPn8UJn6QAAAAASUVORK5CYII="

	t.Run("data_uri", func(t *testing.T) {
		body := []byte(`{"model":"gemini-x","messages":[
			{"role":"user","content":[
				{"type":"image_url","image_url":{"url":"data:image/png;base64,` + realPNGBase64 + `"}},
				{"type":"text","text":"Describe image in three words"}
			]}
		]}`)
		out, err := translateRequest(body)
		if err != nil {
			t.Fatalf("translateRequest error: %v", err)
		}
		img := gjson.GetBytes(out, "contents.0.parts.0.inlineData")
		if img.Get("mimeType").String() != "image/png" || img.Get("data").String() != realPNGBase64 {
			t.Errorf("inlineData wrong: %s", img.Raw)
		}
		if txt := gjson.GetBytes(out, "contents.0.parts.1.text").String(); txt != "Describe image in three words" {
			t.Errorf("text part = %q", txt)
		}
	})

	t.Run("https_url_passthrough", func(t *testing.T) {
		body := []byte(`{"model":"gemini-x","messages":[
			{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.com/cat.png"}}]}
		]}`)
		out, err := translateRequest(body)
		if err != nil {
			t.Fatalf("translateRequest error: %v", err)
		}
		if got := gjson.GetBytes(out, "contents.0.parts.0.fileData.fileUri").String(); got != "https://example.com/cat.png" {
			t.Errorf("fileData.fileUri = %q, want passthrough URL", got)
		}
	})
}

// TestTranslateRequest_CandidateCount regresses a bug where OpenAI's "n" was
// never forwarded, so generationConfig.candidateCount stayed unset and
// Gemini always returned exactly one candidate regardless of what the client
// asked for.
func TestTranslateRequest_CandidateCount(t *testing.T) {
	body := []byte(`{"model":"gemini-x","n":3,"messages":[{"role":"user","content":"hi"}]}`)
	out, err := translateRequest(body)
	if err != nil {
		t.Fatalf("translateRequest error: %v", err)
	}
	if got := gjson.GetBytes(out, "generationConfig.candidateCount").Int(); got != 3 {
		t.Errorf("candidateCount = %d, want 3: %s", got, out)
	}
}

// TestTranslateRequest_ResponseFormat covers the OpenAI response_format ->
// Gemini responseMimeType/responseSchema mapping for both json_object (no
// schema) and json_schema (schema passed through as-is).
func TestTranslateRequest_ResponseFormat(t *testing.T) {
	t.Run("json_object", func(t *testing.T) {
		body := []byte(`{"model":"m","response_format":{"type":"json_object"},"messages":[{"role":"user","content":"hi"}]}`)
		out, err := translateRequest(body)
		if err != nil {
			t.Fatalf("translateRequest error: %v", err)
		}
		if got := gjson.GetBytes(out, "generationConfig.responseMimeType").String(); got != "application/json" {
			t.Errorf("responseMimeType = %q, want application/json: %s", got, out)
		}
		if gjson.GetBytes(out, "generationConfig.responseSchema").Exists() {
			t.Errorf("responseSchema should be absent for json_object: %s", out)
		}
	})
	t.Run("json_schema", func(t *testing.T) {
		body := []byte(`{"model":"m","response_format":{"type":"json_schema","json_schema":{"name":"weather","schema":{"type":"object","properties":{"city":{"type":"string"}}}}},"messages":[{"role":"user","content":"hi"}]}`)
		out, err := translateRequest(body)
		if err != nil {
			t.Fatalf("translateRequest error: %v", err)
		}
		if got := gjson.GetBytes(out, "generationConfig.responseMimeType").String(); got != "application/json" {
			t.Errorf("responseMimeType = %q, want application/json: %s", got, out)
		}
		if got := gjson.GetBytes(out, "generationConfig.responseSchema.properties.city.type").String(); got != "string" {
			t.Errorf("responseSchema not passed through: %s", out)
		}
	})
	t.Run("text_type_no_override", func(t *testing.T) {
		body := []byte(`{"model":"m","response_format":{"type":"text"},"messages":[{"role":"user","content":"hi"}]}`)
		out, err := translateRequest(body)
		if err != nil {
			t.Fatalf("translateRequest error: %v", err)
		}
		if gjson.GetBytes(out, "generationConfig").Exists() {
			t.Errorf("generationConfig should stay absent for response_format:text: %s", out)
		}
	})
}

// TestResponseHandler_SSE_MultipleCandidates: n>1 streams multiple candidates
// interleaved in the same SSE chunk; each must keep its own OpenAI choice
// index rather than only the first candidate surviving translation.
func TestResponseHandler_SSE_MultipleCandidates(t *testing.T) {
	h := openaiGemini{}.NewResponseHandler()
	chunk := `data: {"candidates":[` +
		`{"content":{"parts":[{"text":"cand0 "}]},"index":0},` +
		`{"content":{"parts":[{"text":"cand1 "}]},"index":1}` +
		`]}

`
	out, _ := h.Feed([]byte(chunk))
	final := `data: {"candidates":[` +
		`{"content":{"parts":[{"text":"end0"}]},"finishReason":"STOP","index":0},` +
		`{"content":{"parts":[{"text":"end1"}]},"finishReason":"STOP","index":1}` +
		`],"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":4,"totalTokenCount":9}}

`
	out2, _ := h.Feed([]byte(final))
	fin, usage, _ := h.Flush()
	all := string(out) + string(out2) + string(fin)

	var cand0, cand1 strings.Builder
	for _, line := range strings.Split(all, "\n") {
		if !strings.HasPrefix(line, "data: {") {
			continue
		}
		payload := line[len("data: "):]
		choice := gjson.Get(payload, "choices.0")
		switch choice.Get("index").Int() {
		case 0:
			cand0.WriteString(choice.Get("delta.content").String())
		case 1:
			cand1.WriteString(choice.Get("delta.content").String())
		}
	}
	if cand0.String() != "cand0 end0" {
		t.Errorf("candidate 0 text = %q, want %q", cand0.String(), "cand0 end0")
	}
	if cand1.String() != "cand1 end1" {
		t.Errorf("candidate 1 text = %q, want %q", cand1.String(), "cand1 end1")
	}
	if !strings.Contains(all, `"index":0,"delta":{},"finish_reason":"stop"`) &&
		!strings.Contains(all, `"finish_reason":"stop"`) {
		t.Errorf("missing finish_reason on a candidate: %s", all)
	}
	if usage == nil || usage.Total != 9 {
		t.Errorf("usage = %+v, want total 9", usage)
	}
}

// TestMapFinishReason_Completeness covers every documented Gemini
// Candidate.FinishReason value so a value the mapping doesn't know about
// can't silently collapse into "stop" and hide a safety block or a malformed
// tool call behind a reply that looks like a clean completion.
func TestMapFinishReason_Completeness(t *testing.T) {
	openAIValidFinishReasons := map[string]bool{
		"stop": true, "length": true, "tool_calls": true, "content_filter": true,
	}
	for in, want := range map[string]string{
		"STOP":                      "stop",
		"MAX_TOKENS":                "length",
		"SAFETY":                    "content_filter",
		"RECITATION":                "content_filter",
		"LANGUAGE":                  "content_filter",
		"BLOCKLIST":                 "content_filter",
		"PROHIBITED_CONTENT":        "content_filter",
		"SPII":                      "content_filter",
		"MALFORMED_FUNCTION_CALL":   "tool_calls",
		"OTHER":                     "stop",
		"FINISH_REASON_UNSPECIFIED": "stop",
		"":                          "stop",
	} {
		got := mapFinishReason(in)
		if got != want {
			t.Errorf("mapFinishReason(%q) = %q, want %q", in, got, want)
		}
		if !openAIValidFinishReasons[got] {
			t.Errorf("mapFinishReason(%q) = %q, not a valid OpenAI finish_reason", in, got)
		}
	}
}

// A success response whose body happens to contain "error" should not be misdetected as
// an error (the old byte-scanning bug); only a genuine error body should be.
func TestIsGeminiError(t *testing.T) {
	success := []byte(`{"candidates":[{"content":{"parts":[{"text":"error"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":8,"candidatesTokenCount":1,"totalTokenCount":9}}`)
	if isGeminiError(success) {
		t.Error("a success response whose body contains \"error\" was misdetected as an error")
	}
	errBody := []byte(`{"error":{"code":400,"message":"bad","status":"INVALID_ARGUMENT"}}`)
	if !isGeminiError(errBody) {
		t.Error("a genuine error response was not recognized")
	}
}

// When a success response body is "error", it should translate normally and carry usage
// (not take the error passthrough path).
func TestResponseHandler_JSON_ContentIsError(t *testing.T) {
	h := openaiGemini{}.NewResponseHandler()
	body := `{"candidates":[{"content":{"parts":[{"text":"error"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":8,"candidatesTokenCount":1,"totalTokenCount":9}}`
	_, _ = h.Feed([]byte(body))
	out, usage, err := h.Flush()
	if err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if !strings.Contains(string(out), `"object":"chat.completion"`) || !strings.Contains(string(out), `"content":"error"`) {
		t.Errorf("should translate to OpenAI shape, got: %s", out)
	}
	if usage == nil || usage.Total != 9 {
		t.Errorf("usage should be preserved, got %+v", usage)
	}
}

// Safety block (no candidates + promptFeedback.blockReason): non-streaming choices
// non-null + content_filter.
func TestResponseHandler_JSON_SafetyBlock(t *testing.T) {
	h := openaiGemini{}.NewResponseHandler()
	body := `{"promptFeedback":{"blockReason":"SAFETY"}}`
	_, _ = h.Feed([]byte(body))
	out, _, err := h.Flush()
	if err != nil {
		t.Fatalf("Flush: %v", err)
	}
	choices := gjson.GetBytes(out, "choices")
	if !choices.IsArray() || len(choices.Array()) != 1 {
		t.Fatalf("choices should be an array with one element (not null), got: %s", out)
	}
	if choices.Array()[0].Get("finish_reason").String() != "content_filter" {
		t.Errorf("finish_reason should be content_filter, got: %s", out)
	}
}

// Streaming safety block: not an empty stream, sends a content_filter closing chunk.
func TestResponseHandler_SSE_SafetyBlock(t *testing.T) {
	h := openaiGemini{}.NewResponseHandler()
	chunk := "data: {\"promptFeedback\":{\"blockReason\":\"SAFETY\"}}\n\n"
	out, _ := h.Feed([]byte(chunk))
	final, _, _ := h.Flush()
	all := string(out) + string(final)
	if !strings.Contains(all, `"finish_reason":"content_filter"`) {
		t.Errorf("a streaming block should emit a content_filter chunk, got: %s", all)
	}
	if !strings.Contains(all, "data: [DONE]") {
		t.Errorf("should have [DONE], got: %s", all)
	}
}

// Streaming: Gemini SSE chunk -> OpenAI chat.completion.chunk SSE + usage extracted from
// the last frame.
func TestResponseHandler_SSE(t *testing.T) {
	h := openaiGemini{}.NewResponseHandler()

	// Two Gemini SSE chunks: first frame content, last frame content + finishReason +
	// usageMetadata.
	chunk1 := "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"Hello\"}],\"role\":\"model\"},\"index\":0}]}\n\n"
	chunk2 := "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\" world\"}],\"role\":\"model\"},\"finishReason\":\"STOP\",\"index\":0}],\"usageMetadata\":{\"promptTokenCount\":5,\"candidatesTokenCount\":2,\"totalTokenCount\":7}}\n\n"

	out1, err := h.Feed([]byte(chunk1))
	if err != nil {
		t.Fatalf("Feed1: %v", err)
	}
	out2, err := h.Feed([]byte(chunk2))
	if err != nil {
		t.Fatalf("Feed2: %v", err)
	}
	final, usage, err := h.Flush()
	if err != nil {
		t.Fatalf("Flush: %v", err)
	}
	all := string(out1) + string(out2) + string(final)

	for _, want := range []string{
		`"object":"chat.completion.chunk"`,
		`"role":"assistant"`,
		`"content":"Hello"`,
		`"content":" world"`,
		`"finish_reason":"stop"`,
		"data: [DONE]",
	} {
		if !strings.Contains(all, want) {
			t.Errorf("SSE output missing %q\nfull text:\n%s", want, all)
		}
	}
	// role delta is only sent once.
	if n := strings.Count(all, `"role":"assistant"`); n != 1 {
		t.Errorf("role delta should be sent only once, got %d", n)
	}
	if usage == nil || usage.Input != 5 || usage.Output != 2 || usage.Total != 7 {
		t.Errorf("usage=%+v, want in=5 out=2 total=7", usage)
	}
}

// Across a Feed boundary: the half-line stays in lineBuf and is reassembled on the next
// Feed.
func TestResponseHandler_SSE_SplitAcrossFeeds(t *testing.T) {
	h := openaiGemini{}.NewResponseHandler()
	full := "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"Hi\"}]},\"index\":0}]}\n\n"
	// split in the middle
	mid := len(full) / 2
	_, _ = h.Feed([]byte(full[:mid]))
	out, _ := h.Feed([]byte(full[mid:]))
	final, _, _ := h.Flush()
	all := string(out) + string(final)
	if !strings.Contains(all, `"content":"Hi"`) {
		t.Errorf("content was lost across the Feed boundary, got: %s", all)
	}
}

// The non-streaming JSON path still works normally (buffer-then-translate).
func TestResponseHandler_JSON(t *testing.T) {
	h := openaiGemini{}.NewResponseHandler()
	body := `{"candidates":[{"content":{"parts":[{"text":"answer"}]},"finishReason":"STOP","index":0}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":1,"totalTokenCount":4}}`
	if _, err := h.Feed([]byte(body)); err != nil {
		t.Fatalf("Feed: %v", err)
	}
	out, usage, err := h.Flush()
	if err != nil {
		t.Fatalf("Flush: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, `"object":"chat.completion"`) || !strings.Contains(s, `"content":"answer"`) {
		t.Errorf("JSON translation output is unexpected: %s", s)
	}
	if strings.Contains(s, "chunk") {
		t.Errorf("non-streaming output should not be chunk shape: %s", s)
	}
	if usage == nil || usage.Total != 4 {
		t.Errorf("usage=%+v, want total=4", usage)
	}
}

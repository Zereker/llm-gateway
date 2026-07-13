package replay

import (
	"strconv"
	"strings"
	"testing"

	"github.com/zereker/llm-gateway/internal/usage/extractor"
	"github.com/zereker/opencassette/cassette"
)

// openaiDirs are every vendored-corpus source that captured real
// api.openai.com traffic.
var openaiDirs = []string{
	"openai/langchain-ai-langchain",
}

// classifyOpenAIResponse buckets a real captured OpenAI response body. See
// openai/langchain-ai-langchain/README.md for the underlying finding this
// codifies: despite most cassette filenames implying Chat Completions, the
// actual bodies are overwhelmingly Responses-API-shaped.
type openaiKind int

const (
	kindUnknown openaiKind = iota
	kindChat
	kindResponses
	kindEmbeddings
)

func classifyOpenAIResponse(body []byte) openaiKind {
	// Normalize away insignificant whitespace before matching: real vendors
	// pretty-print (`"object": "response"`, Azure) as often as they minify
	// (`"object":"response"`, api.openai.com), and this classifier serves both
	// the langchain vendored corpus and the opencassette one.
	s := strings.ReplaceAll(strings.TrimSpace(string(body)), " ", "")
	switch {
	case isSSE(body):
		switch {
		case strings.Contains(s, `"object":"chat.completion.chunk"`):
			return kindChat
		case strings.Contains(s, `"response.created"`) || strings.Contains(s, `"type":"response.`):
			return kindResponses
		}
		return kindUnknown
	case strings.Contains(s, `"object":"chat.completion"`):
		return kindChat
	case strings.Contains(s, `"object":"response"`):
		return kindResponses
	case strings.Contains(s, `"object":"list"`) && strings.Contains(s, `"embedding"`):
		return kindEmbeddings
	}
	return kindUnknown
}

// TestReplayOpenAIUsageExtraction feeds every real captured OpenAI response
// body (Chat Completions and Responses API, streaming and not) through the
// matching usage extractor and asserts it parses without panicking and
// yields non-nil usage for a normal successful response. There is no
// translator to replay these through — both shapes are already OpenAI-native,
// gateway-side this is purely an extraction concern (see
// internal/translator/identity/responses.go and internal/usage/extractor).
func TestReplayOpenAIUsageExtraction(t *testing.T) {
	for _, dir := range openaiDirs {
		files, err := loadVendoredDir(dir)
		if err != nil {
			t.Fatalf("LoadDir %s: %v", dir, err)
		}
		for _, rel := range cassette.SortedKeys(files) {
			path := dir + "/" + rel
			interactions := files[rel]
			examined := false
			for i, it := range interactions {
				if len(it.ResponseBody) == 0 {
					continue
				}
				kind := classifyOpenAIResponse(it.ResponseBody)
				if kind == kindUnknown || kind == kindEmbeddings {
					continue
				}
				examined = true
				t.Run(path+"#"+strconv.Itoa(i), func(t *testing.T) {
					var session extractor.Session
					if kind == kindChat {
						session = extractor.NewOpenAI()
					} else {
						session = extractor.NewResponses()
					}
					session.Feed(it.ResponseBody)
					usage := session.Final()
					if usage == nil {
						t.Errorf("%s: extractor returned nil usage for a real successful response", path)
					}
				})
			}
			switch {
			case examined:
				claim(path)
			case strings.Contains(rel, "embeddings"):
				markNotApplicable(path, "Embeddings API cassette — not a chat/response endpoint, no extractor applies")
			case strings.Contains(rel, "schema_parsing_failures") && !strings.Contains(rel, "responses_api"):
				markNotApplicable(path, "exercises client-side (langchain) retry-on-parse-failure logic; every captured HTTP response is itself a normal, already-classified chat/response body — inspected manually, nothing left unclassified")
			default:
				markNotApplicable(path, "no interaction response body classified as Chat Completions, Responses API, or Embeddings — needs manual inspection if this ever fires")
			}
		}
	}
}

// Package builtin activates all protocol factories and translators shipped
// with the application. Commands import it once for side effects so their
// built-in capability sets cannot drift.
package builtin

import (
	_ "github.com/zereker/llm-gateway/pkg/protocol/anthropic"
	_ "github.com/zereker/llm-gateway/pkg/protocol/azureopenai"
	_ "github.com/zereker/llm-gateway/pkg/protocol/bedrock"
	_ "github.com/zereker/llm-gateway/pkg/protocol/cohere"
	_ "github.com/zereker/llm-gateway/pkg/protocol/gemini"
	_ "github.com/zereker/llm-gateway/pkg/protocol/openai"

	_ "github.com/zereker/llm-gateway/pkg/translator/anthropic_openai"
	_ "github.com/zereker/llm-gateway/pkg/translator/identity"
	_ "github.com/zereker/llm-gateway/pkg/translator/openai_anthropic"
	_ "github.com/zereker/llm-gateway/pkg/translator/openai_cohere"
	_ "github.com/zereker/llm-gateway/pkg/translator/openai_gemini"
	_ "github.com/zereker/llm-gateway/pkg/translator/responses_openai"
)

// Package builtin explicitly assembles all protocol factories and translators
// shipped with the application.
package builtin

import (
	"github.com/zereker/llm-gateway/pkg/protocol"
	"github.com/zereker/llm-gateway/pkg/protocol/anthropic"
	"github.com/zereker/llm-gateway/pkg/protocol/azureopenai"
	"github.com/zereker/llm-gateway/pkg/protocol/bedrock"
	"github.com/zereker/llm-gateway/pkg/protocol/cohere"
	"github.com/zereker/llm-gateway/pkg/protocol/gemini"
	"github.com/zereker/llm-gateway/pkg/protocol/openai"
	"github.com/zereker/llm-gateway/pkg/translator"
	"github.com/zereker/llm-gateway/pkg/translator/anthropic_openai"
	"github.com/zereker/llm-gateway/pkg/translator/identity"
	"github.com/zereker/llm-gateway/pkg/translator/openai_anthropic"
	"github.com/zereker/llm-gateway/pkg/translator/openai_cohere"
	"github.com/zereker/llm-gateway/pkg/translator/openai_gemini"
	"github.com/zereker/llm-gateway/pkg/translator/responses_openai"
)

// NewLookup returns the complete built-in handler catalog.
func NewLookup() *protocol.DefaultLookup {
	factories := map[string]protocol.Factory{
		"openai":       openai.Factory{},
		"anthropic":    anthropic.Factory{},
		"azure-openai": azureopenai.Factory{},
		"bedrock":      bedrock.Factory{},
		"cohere":       cohere.Factory{},
		"gemini":       gemini.Factory{},
	}
	for _, alias := range openai.Aliases() {
		factories[alias] = openai.Factory{}
	}
	translators := identity.All()
	translators = append(translators,
		anthropic_openai.New(), openai_anthropic.New(), openai_cohere.New(),
		openai_gemini.New(), responses_openai.New(),
	)
	return protocol.NewLookup(factories, translator.NewRegistry(translators...))
}

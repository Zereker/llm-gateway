package identity

import "github.com/zereker/llm-gateway/pkg/translator"

// All returns all same-protocol translators shipped by this package.
func All() []translator.Translator {
	return []translator.Translator{newOpenAI(), newAnthropic(), newResponses()}
}

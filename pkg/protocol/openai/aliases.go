package openai

import "github.com/zereker/llm-gateway/pkg/protocol"

// init registers a set of OpenAI-compatible vendor aliases.
//
// These vendors all run the OpenAI protocol (same /v1/chat/completions, same
// request/response format, and Bearer token auth); the only differences are
// the endpoint URL / API key / actual model name. They can share Factory{}
// without duplicating the implementation.
//
// **Protocol ownership**: when the deployer writes endpoint SQL, they set
// `protocol: openai` explicitly; DefaultLookup picks the translator by
// ep.Protocol, decoupled from vendor.
//
// When a vendor needs dedicated handling (e.g. the reasoning_content field in
// DeepSeek-R1 responses, or a fully different protocol like Anthropic's),
// pull it out of this list into its own subpackage.
func init() {
	aliases := []string{
		"ark", // Volcengine Ark — ByteDance's model hosting platform, serving DeepSeek / GLM / Qwen etc.
		// Can add later: moonshot / zhipu / qwen / doubao etc.
	}
	for _, v := range aliases {
		protocol.RegisterFactory(v, Factory{})
	}
}

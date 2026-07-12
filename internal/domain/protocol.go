package domain

import (
	"encoding/json"
	"fmt"
)

// Protocol is the protocol family used by the client.
type Protocol int

const (
	ProtoUnknown   Protocol = iota
	ProtoOpenAI             // /v1/chat/completions, /v1/embeddings, /v1/images, ...
	ProtoAnthropic          // /v1/messages
	ProtoGemini             // /v1beta/models/.../generateContent
	ProtoBedrock            // AWS Bedrock's Converse API (model-agnostic wire shape; NOT InvokeModel, which reuses ProtoAnthropic since its body already is Anthropic Messages JSON — see internal/protocol/bedrock's doc comment)
	ProtoCustom             // vendor-custom; the Adapter interprets it itself
	ProtoResponses          // OpenAI Responses API (/v1/responses; a new protocol introduced in 2024 H2)
	ProtoCohere             // Cohere v2 /v2/chat (message.content array + nested usage.tokens)
)

// Wire names for each Protocol — shared between String() and ParseProtocol()
// so the two can't drift out of sync with each other.
const (
	protoNameOpenAI    = "openai"
	protoNameAnthropic = "anthropic"
	protoNameGemini    = "gemini"
	protoNameBedrock   = "bedrock"
	protoNameCustom    = "custom"
	protoNameResponses = "responses"
	protoNameCohere    = "cohere"
)

func (p Protocol) String() string {
	switch p {
	case ProtoOpenAI:
		return protoNameOpenAI
	case ProtoAnthropic:
		return protoNameAnthropic
	case ProtoGemini:
		return protoNameGemini
	case ProtoBedrock:
		return protoNameBedrock
	case ProtoCustom:
		return protoNameCustom
	case ProtoResponses:
		return protoNameResponses
	case ProtoCohere:
		return protoNameCohere
	default:
		return unknownLabel
	}
}

// ParseProtocol is the inverse of String() — converts a value read from a SQL
// VARCHAR column back into a Protocol.
// An unknown string returns ProtoUnknown (the caller decides how to handle it).
func ParseProtocol(s string) Protocol {
	switch s {
	case protoNameOpenAI:
		return ProtoOpenAI
	case protoNameAnthropic:
		return ProtoAnthropic
	case protoNameGemini:
		return ProtoGemini
	case protoNameBedrock:
		return ProtoBedrock
	case protoNameCustom:
		return ProtoCustom
	case protoNameResponses:
		return ProtoResponses
	case protoNameCohere:
		return ProtoCohere
	default:
		return ProtoUnknown
	}
}

// MarshalJSON serializes Protocol to a string (for human-readable HTTP / log display).
func (p Protocol) MarshalJSON() ([]byte, error) {
	return json.Marshal(p.String())
}

// UnmarshalJSON accepts the string form ("openai" / "anthropic" / ...).
//
// Strict mode: an unknown value returns an error, so a misconfigured protocol
// name doesn't get silently persisted.
func (p *Protocol) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}

	parsed := ParseProtocol(s)
	if parsed == ProtoUnknown && s != "" && s != unknownLabel {
		return fmt.Errorf("domain: unknown protocol %q", s)
	}

	*p = parsed

	return nil
}

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
	ProtoBedrock            // AWS Bedrock format
	ProtoCustom             // vendor-custom; interpreted by the Adapter itself
	ProtoResponses          // OpenAI Responses API (/v1/responses; a new protocol introduced in 2024 H2)
)

func (p Protocol) String() string {
	switch p {
	case ProtoOpenAI:
		return "openai"
	case ProtoAnthropic:
		return "anthropic"
	case ProtoGemini:
		return "gemini"
	case ProtoBedrock:
		return "bedrock"
	case ProtoCustom:
		return "custom"
	case ProtoResponses:
		return "responses"
	default:
		return "unknown"
	}
}

// ParseProtocol is the reverse of String() — converts a value read from a SQL
// VARCHAR column into Protocol.
// An unknown string returns ProtoUnknown (caller decides how to handle it).
func ParseProtocol(s string) Protocol {
	switch s {
	case "openai":
		return ProtoOpenAI
	case "anthropic":
		return ProtoAnthropic
	case "gemini":
		return ProtoGemini
	case "bedrock":
		return ProtoBedrock
	case "custom":
		return ProtoCustom
	case "responses":
		return ProtoResponses
	default:
		return ProtoUnknown
	}
}

// MarshalJSON serializes Protocol into a string (for human-readable HTTP / log display).
func (p Protocol) MarshalJSON() ([]byte, error) {
	return json.Marshal(p.String())
}

// UnmarshalJSON accepts the string form ("openai" / "anthropic" / ...).
//
// Strict mode: an unknown value returns an error, to prevent a misconfigured
// protocol name from being silently persisted.
func (p *Protocol) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	parsed := ParseProtocol(s)
	if parsed == ProtoUnknown && s != "" && s != "unknown" {
		return fmt.Errorf("domain: unknown protocol %q", s)
	}
	*p = parsed
	return nil
}

package domain

import (
	"encoding/json"
	"fmt"
)

// Protocol 客户端使用的协议族。
type Protocol int

const (
	ProtoUnknown   Protocol = iota
	ProtoOpenAI             // /v1/chat/completions, /v1/embeddings, /v1/images, ...
	ProtoAnthropic          // /v1/messages
	ProtoGemini             // /v1beta/models/.../generateContent
	ProtoBedrock            // AWS Bedrock 格式
	ProtoCustom             // 厂商自定义；Adapter 自行解释
	ProtoResponses          // OpenAI Responses API（/v1/responses；2024 H2 推出的新协议）
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

// ParseProtocol 反 String()——SQL VARCHAR 列读出来转 Protocol。
// 未知字符串返回 ProtoUnknown（caller 自行决定如何处理）。
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

// MarshalJSON 把 Protocol 序列化成字符串（HTTP / 日志显示给人看）。
func (p Protocol) MarshalJSON() ([]byte, error) {
	return json.Marshal(p.String())
}

// UnmarshalJSON 接受字符串形式（"openai" / "anthropic" / ...）。
//
// 严格模式：未知值返 error，避免 配置错协议名静默落库。
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

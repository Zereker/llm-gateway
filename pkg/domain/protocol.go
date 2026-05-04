package domain

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

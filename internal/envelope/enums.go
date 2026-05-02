// Package envelope 定义请求信封：原始字节 + 解析后的 CanonicalRequest +
// 协议族 + 模态。
//
// Envelope 是 M3 Envelope middleware 的产物，由 Detector + Parser 实现填充。
package envelope

// SourceProtocol 客户端使用的协议族。
type SourceProtocol int

const (
	ProtoUnknown   SourceProtocol = iota
	ProtoOpenAI                   // /v1/chat/completions, /v1/embeddings, /v1/images, ...
	ProtoAnthropic                // /v1/messages
	ProtoGemini                   // /v1beta/models/.../generateContent
	ProtoBedrock                  // AWS Bedrock 格式
	ProtoCustom                   // 厂商自定义；Adapter 自行解释
)

func (p SourceProtocol) String() string {
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
	default:
		return "unknown"
	}
}

// Modality 请求模态。
type Modality int

const (
	ModalityChat      Modality = iota // 含 Anthropic Messages
	ModalityEmbedding
	ModalityImage // 含文生图、图生图、Inpaint，Adapter 内部按 Parsed 分发
	ModalityRerank
	ModalityTTS
	ModalityASR
	ModalityTask // 异步任务（视频生成、长音频合成等），轮询模型
)

func (m Modality) String() string {
	switch m {
	case ModalityChat:
		return "chat"
	case ModalityEmbedding:
		return "embedding"
	case ModalityImage:
		return "image"
	case ModalityRerank:
		return "rerank"
	case ModalityTTS:
		return "tts"
	case ModalityASR:
		return "asr"
	case ModalityTask:
		return "task"
	}
	return "unknown"
}

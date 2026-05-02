package domain

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

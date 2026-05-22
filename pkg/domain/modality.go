package domain

import (
	"encoding/json"
	"fmt"
)

// Modality 请求模态。
type Modality int

const (
	ModalityChat Modality = iota // 含 Anthropic Messages
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

// ParseModality 反向：字符串 → Modality；未知 → 0 (ModalityChat) + error。
func ParseModality(s string) (Modality, error) {
	switch s {
	case "chat":
		return ModalityChat, nil
	case "embedding":
		return ModalityEmbedding, nil
	case "image":
		return ModalityImage, nil
	case "rerank":
		return ModalityRerank, nil
	case "tts":
		return ModalityTTS, nil
	case "asr":
		return ModalityASR, nil
	case "task":
		return ModalityTask, nil
	}
	return 0, fmt.Errorf("unknown modality %q", s)
}

// MarshalJSON 让 endpoint capabilities JSON 里 modality 以 "chat" / "embedding"
// 这种 deployer 友好的字符串落库，而不是 enum 数字。
func (m Modality) MarshalJSON() ([]byte, error) {
	return json.Marshal(m.String())
}

// UnmarshalJSON 反向；未知值返 error 让启动期 / mapper 直接看到。
func (m *Modality) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	parsed, err := ParseModality(s)
	if err != nil {
		return err
	}
	*m = parsed
	return nil
}

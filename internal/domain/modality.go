package domain

import (
	"encoding/json"
	"fmt"
)

// Modality is the request modality.
type Modality int

const (
	ModalityChat Modality = iota // includes Anthropic Messages
	ModalityEmbedding
	ModalityImage // includes text-to-image, image-to-image, inpaint; the Adapter dispatches internally by Parsed
	ModalityRerank
	ModalityTTS
	ModalityASR
	ModalityTask // async tasks (video generation, long-form audio synthesis, etc.), polling model
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

// ParseModality is the reverse: string → Modality; unknown → 0 (ModalityChat) + error.
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

// MarshalJSON makes modality persist in endpoint capabilities JSON as a
// deployer-friendly string like "chat" / "embedding", rather than an enum
// number.
func (m Modality) MarshalJSON() ([]byte, error) {
	return json.Marshal(m.String())
}

// UnmarshalJSON is the reverse; an unknown value returns an error so it's
// surfaced directly at startup / in the mapper.
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

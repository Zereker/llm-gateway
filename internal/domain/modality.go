package domain

import (
	"encoding/json"
	"fmt"
)

// unknownLabel is the shared String() fallback for every enum in this package
// (Modality, Protocol, BudgetStatus, AttemptOutcome, ...) whose value has no
// recognized name.
const unknownLabel = "unknown"

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

// Wire names for each Modality — shared between String() and ParseModality()
// so the two can't drift out of sync with each other.
const (
	modalityNameChat      = "chat"
	modalityNameEmbedding = "embedding"
	modalityNameImage     = "image"
	modalityNameRerank    = "rerank"
	modalityNameTTS       = "tts"
	modalityNameASR       = "asr"
	modalityNameTask      = "task"
)

func (m Modality) String() string {
	switch m {
	case ModalityChat:
		return modalityNameChat
	case ModalityEmbedding:
		return modalityNameEmbedding
	case ModalityImage:
		return modalityNameImage
	case ModalityRerank:
		return modalityNameRerank
	case ModalityTTS:
		return modalityNameTTS
	case ModalityASR:
		return modalityNameASR
	case ModalityTask:
		return modalityNameTask
	}

	return unknownLabel
}

// ParseModality is the reverse: string → Modality; unknown → 0 (ModalityChat) + error.
func ParseModality(s string) (Modality, error) {
	switch s {
	case modalityNameChat:
		return ModalityChat, nil
	case modalityNameEmbedding:
		return ModalityEmbedding, nil
	case modalityNameImage:
		return ModalityImage, nil
	case modalityNameRerank:
		return ModalityRerank, nil
	case modalityNameTTS:
		return ModalityTTS, nil
	case modalityNameASR:
		return ModalityASR, nil
	case modalityNameTask:
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

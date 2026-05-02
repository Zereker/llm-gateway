package adapter

import "github.com/zereker-labs/ai-gateway/pkg/domain"

// ModelCapabilities 模型对外暴露的能力（可选实现，本期不强制）。
type ModelCapabilities struct {
	MaxContextTokens      int
	SupportsThinking      bool
	SupportsTools         bool
	SupportsVision        bool
	SupportsStructuredOut bool
	SupportsMultiTurn     bool
	MaxToolCalls          int
	SupportedModalities   []domain.Modality
}

// CapabilityProvider Adapter 可选实现。端点选择层在"按能力过滤"时调用。
type CapabilityProvider interface {
	ModelCapabilities(model string) *ModelCapabilities
}

package middleware

import "github.com/zereker-labs/ai-gateway/pkg/domain"

// Detector 识别请求的协议族与模态。M3 Envelope middleware 的依赖。
//
// 默认实现按 URL 路径优先匹配（如 /v1/messages → Anthropic + Chat），body 特征兜底。
type Detector interface {
	Detect(path string, body []byte) (domain.Protocol, domain.Modality)
}

// Parser 把 RawBytes 解析为 CanonicalRequest。M3 Envelope middleware 的依赖。
//
// 不同 SourceProtocol 用不同实现；Parser 内部按 SourceProtocol 分发。
type Parser interface {
	Parse(raw []byte, proto domain.Protocol, mod domain.Modality) (domain.CanonicalRequest, error)
}

// Envelope() gin.HandlerFunc 实现待补；接口已就位，可独立实现 + 单测。

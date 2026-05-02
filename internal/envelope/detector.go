package envelope

// Detector 识别请求的协议族与模态。
//
// 默认实现按 URL 路径优先匹配（如 /v1/messages → Anthropic + Chat），body 特征兜底。
type Detector interface {
	Detect(path string, body []byte) (SourceProtocol, Modality)
}

// Parser 把 RawBytes 解析为 CanonicalRequest。
//
// 不同 SourceProtocol 用不同实现；Parser 内部按 SourceProtocol 分发。
type Parser interface {
	Parse(raw []byte, proto SourceProtocol, mod Modality) (CanonicalRequest, error)
}

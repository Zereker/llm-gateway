package middleware

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/zereker-labs/ai-gateway/pkg/domain"
)

// DefaultDetector 按 URL 路径优先匹配协议族 + 模态。
//
// 路径规则（精确 / 后缀匹配）：
//
//	/v1/chat/completions                 → ProtoOpenAI    + ModalityChat
//	/v1/messages                         → ProtoAnthropic + ModalityChat
//	/v1/embeddings                       → ProtoOpenAI    + ModalityEmbedding
//	/v1/images/generations               → ProtoOpenAI    + ModalityImage
//	/v1beta/models/<model>:generateContent → ProtoGemini    + ModalityChat
//
// body 参数当前未用（v0.1 仅靠路径），但保留接口以便后续按 body 特征兜底。
//
// 零值即可用：var d DefaultDetector。
type DefaultDetector struct{}

// Detect 实现 middleware.Detector.Detect。
func (DefaultDetector) Detect(path string, _ []byte) (domain.Protocol, domain.Modality) {
	if i := strings.IndexByte(path, '?'); i >= 0 {
		path = path[:i]
	}
	switch {
	case strings.HasSuffix(path, "/chat/completions"):
		return domain.ProtoOpenAI, domain.ModalityChat
	case strings.HasSuffix(path, "/messages"):
		return domain.ProtoAnthropic, domain.ModalityChat
	case strings.HasSuffix(path, "/embeddings"):
		return domain.ProtoOpenAI, domain.ModalityEmbedding
	case strings.Contains(path, "/images/generations"):
		return domain.ProtoOpenAI, domain.ModalityImage
	case strings.Contains(path, "/v1beta/models/"):
		return domain.ProtoGemini, domain.ModalityChat
	}
	return domain.ProtoUnknown, domain.ModalityChat
}

// DefaultParser 把 OpenAI 形态的 JSON body 反序列化为 CanonicalRequest。
//
// 仅支持 ProtoOpenAI（v0.1 范围）；其他 Protocol 返回错误，由调用方决定下一步。
//
// 零值即可用：var p DefaultParser。
type DefaultParser struct{}

// Parse 实现 middleware.Parser.Parse。
func (DefaultParser) Parse(raw []byte, proto domain.Protocol, _ domain.Modality) (domain.CanonicalRequest, error) {
	if len(raw) == 0 {
		return domain.CanonicalRequest{}, errors.New("default parser: empty body")
	}
	if proto != domain.ProtoOpenAI {
		return domain.CanonicalRequest{}, fmt.Errorf("default parser: unsupported protocol %s (v0.1 only OpenAI)", proto)
	}
	var req domain.CanonicalRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return req, fmt.Errorf("default parser: %w", err)
	}
	if req.Model == "" {
		return req, errors.New("default parser: missing 'model' field")
	}
	return req, nil
}

// 编译期断言。
var (
	_ Detector = DefaultDetector{}
	_ Parser   = DefaultParser{}
)

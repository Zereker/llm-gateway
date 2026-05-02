package envelope

import "time"

// Envelope M3 Envelope middleware 的产物。
//
// 业务逻辑读 Parsed（结构化），透传到上游用 RawBytes（原始字节）。
// 这一双通道设计让"网关本身关心的字段（model / stream 等）"与
// "网关不关心但要保留的字段（reasoning_details / metadata 等）"完全解耦。
type Envelope struct {
	RawBytes       []byte
	Parsed         CanonicalRequest
	SourceProtocol SourceProtocol
	Modality       Modality
	RequestTime    time.Time
}

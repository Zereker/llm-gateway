package domain

// RequestEnvelope M3 Envelope middleware 的产物。
//
// 业务逻辑在 RawBytes（原始字节）+ Model（M3 从 body 提取的 model 字段）+
// SourceProtocol / Modality 上做决策；body 翻译 / 字段映射全部下放给 pkg/translator
// 各 translator 实现，本结构**不**承载 canonical 化职责。
//
// 设计精神：M3 只做"读 body + 拿 model 做路由"，不做参数解析；CanonicalRequest
// 这种"统一 internal 表示"曾经存在但全字段无消费者，已删（v1.0 review 决定）。
// 上游 / 客户端协议 shape 转换由 pkg/translator/<src>_<tgt>/ 各自处理。
//
// **不放 RequestTime**：latency 计算用 rc.StartTime（M1 写）即可；M3 进入时刻没
// 独立消费者，加了反而双源。
type RequestEnvelope struct {
	// RawBytes 客户端请求 body 原始字节；body 已在 c.Request.Body 上 NopCloser 重置，
	// 下游再读 c.Request.Body 也能拿到同样内容。
	RawBytes []byte

	// Model 从 body 顶层 `model` 字段提取的模型名（M5 ModelService 据此查 catalog）。
	// 三个客户端协议（OpenAI Chat / Anthropic Messages / OpenAI Responses）顶层
	// 都有 model 字段，所以本字段总能填上。
	Model string

	// SourceProtocol 客户端协议（M1 路由侧 WithSourceProtocol middleware 写入）。
	SourceProtocol Protocol

	// Modality 模态（M1 路由侧 WithSourceProtocol middleware 写入）。
	Modality Modality
}

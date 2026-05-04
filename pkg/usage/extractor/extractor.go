// Package extractor 把"按协议从上游响应里提 usage"的逻辑从 translator 抽出来共享。
//
// **背景**：5 个 translator 的 ResponseHandler 里都散落着 usage 解析——但按**上游协议**
// 归一只有 3 套（OpenAI / Anthropic / Gemini），每套被 2 个 translator 重复实现。
// 抽出来后 translator 只关心"翻译 chunk → client format"，usage 提取走 side-channel。
//
// **使用模式**（在 translator 的 ResponseHandler 里）：
//
//	type myHandler struct {
//	    ex extractor.Session  // 跟上游协议匹配的 session
//	    ...
//	}
//
//	func newHandler() *myHandler {
//	    return &myHandler{ex: extractor.NewOpenAI()}  // 上游是 OpenAI
//	}
//
//	func (h *myHandler) Feed(chunk []byte) ([]byte, error) {
//	    h.ex.Feed(chunk)        // side-channel: 提 usage
//	    return chunk, nil       // 主路径：透传 / 翻译 chunk
//	}
//
//	func (h *myHandler) Flush() ([]byte, *domain.Usage, error) {
//	    return nil, h.ex.Final(), nil
//	}
//
// **自适应模式**：每个 Session 实现都按第一个 chunk 的 prefix 判 SSE / non-SSE，
// translator 不用预先告诉是流式还是非流式。
//
// **并发**：单 Session 实例**不**保证 goroutine-safe；M7 在同一 handler goroutine 内顺序调用。
package extractor

import "github.com/zereker-labs/ai-gateway/pkg/domain"

// Session 一次请求的 usage 提取会话。
//
// 状态：内部累积 SSE buffer（流式）或 body buffer（非流式）。Final() 触发最终解析。
//
// 实现 MUST：
//   - Feed 与 Final 同 goroutine 顺序调用
//   - Final 多次调安全（幂等；返回相同结果）
//   - 不持有 Feed chunk 的 slice 引用（拷进自己的 buffer）
type Session interface {
	// Feed 喂下一段上游响应字节。流式可调多次；非流式可一次性也可分段。
	Feed(chunk []byte)

	// Final 返回截至此刻最佳 usage 估计；nil = 上游没返 usage 信息。
	Final() *domain.Usage
}

package adapter

import (
	"context"
	"net/http"

	"github.com/zereker-labs/ai-gateway/pkg/domain"
)

// Metadata 是静态、厂商级别的元信息（不绑定具体请求）。
//
// 由 Factory.Metadata() 返回；启动期就能拿到，可用来：
//   - 与 ConfigStore 中的 vendor 集合做覆盖比对（漏注册告警）
//   - 路由层根据 SupportedModalities 做能力过滤
//   - 调度日志 / metric 标签
type Metadata struct {
	Vendor              string
	NativeProtocol      domain.Protocol
	SupportedModalities []domain.Modality
}

// Factory 是注册到 adapter registry 的工厂。
//
// 一个 vendor 一个 factory；factory 本身无状态。
// 每次请求由 NewSession 构造一个 Session 实例，承载本次请求的全部状态。
type Factory interface {
	Metadata() Metadata

	// NewSession 创建本次请求专属的 Session。
	//
	// 返回的 Session 已完成对 endpoint 凭证 / URL / 请求体形态的初始化；
	// 调用方按 Session 的契约（先 BuildRequest，再 Feed*，最后 Finalize）使用。
	NewSession(c context.Context, ep *domain.Endpoint, env *domain.RequestEnvelope) (Session, error)
}

// Session 承载单次上游调用的全部状态：请求构造 + 流式响应处理。
//
// 调用顺序（不可乱序）：
//
//  1. BuildRequest() 一次  → 产出待发上游的 *http.Request（URL + Headers + Body 一次性组装）。
//  2. 实际 HTTP 调用由调用方做（dispatch 层），把 response.Body 的 chunk 喂回 Feed。
//  3. Feed(chunk) 多次     → 每次返回应写给客户端的字节（流式翻译 / 透传 / 审核）；
//     非流式场景仅 Feed(整个 body) 一次，返回值通常为空。
//  4. Finalize() 一次      → 返回终态 FinalizeResult（Usage + Response + Error）。
//
// Session 实例只在 M7 Schedule 内的 executor 中存活到 Finalize；不需要挂到 RequestContext。
// 实现只在单 goroutine 内被使用（与 gin handler 同协程），无需自加锁。
type Session interface {
	BuildRequest() (*http.Request, error)
	Feed(chunk []byte) ([]byte, error)
	Finalize() FinalizeResult
}

// FinalizeResult 是 Session.Finalize 的终态。
//
// 三个字段都是 nilable，分别表示：
//   - Usage:    上游 usage 提取成功时非 nil；缺失 / 提取失败时 nil
//   - Response: 跨协议反向翻译后的响应；同协议透传时通常 nil（chunk 已直写客户端）
//   - Error:    成功时 nil；上游 / 解析 / 翻译失败时非 nil（已分类）
//
// 用 struct 包装而不是裸三元组，避免调用方记忆顺序 + 漏 nil check。
type FinalizeResult struct {
	Usage    *domain.Usage
	Response *domain.CanonicalResponse
	Error    *domain.AdapterError
}

package adapter

import (
	"context"
	"io"
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
	// Vendor 是开放集合（运维 / Admin 在 ConfigStore 注册任意名字 + Adapter 实现 init() 注册同名 Factory），
	// 故意不做 enum；与 domain.Endpoint.Vendor 字段对齐。
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
	// 实现负责按 ep.URL / ep.APIKey 等初始化内部状态；
	// 调用方按 Session 的契约（见 Session doc）使用。
	NewSession(c context.Context, ep *domain.Endpoint, env *domain.RequestEnvelope) (Session, error)
}

// Session 承载单次上游调用的全部状态：请求构造 + 流式响应处理 + 资源清理。
//
// 调用方（dispatch layer = M7 RetryExecutor / pkg/middleware/schedule）的标准流程：
//
//	sess, err := factory.NewSession(ctx, ep, env)
//	if err != nil { return err }
//	defer sess.Close()  // 异常 / 提前结束 / panic 路径都要释放
//
//	req, err := sess.BuildRequest()
//	if err != nil { return err }
//	resp, err := httpClient.Do(req)
//	if err != nil { return err }
//	defer resp.Body.Close()
//
//	// dispatch layer 负责读 resp.Body，把字节切片喂回 Feed。
//	// chunk 边界由 dispatch 决定（按 SSE event / 固定大小 / Read 一次的实际长度均可）；
//	// Session 实现负责内部累积 / 重组（如 SSE event 跨 chunk 时 buffer）。
//	buf := make([]byte, 4096)
//	for {
//	    n, rerr := resp.Body.Read(buf)
//	    if n > 0 {
//	        out, err := sess.Feed(buf[:n])
//	        if err != nil { return err }
//	        writer.Write(out)  // 写客户端
//	    }
//	    if rerr == io.EOF { break }
//	    if rerr != nil    { return rerr }
//	}
//
//	result := sess.Finalize()
//	rc.Usage = result.Usage
//	rc.Error = result.Error
//
// 契约：
//   - 单 goroutine 使用（与 gin handler 同协程）；实现无需自加锁。
//   - BuildRequest → Feed* → Finalize 顺序调用；违反契约时实现可 panic / 返回错误，行为未定义。
//   - Close 在**所有**路径上必须调用（defer），包括 BuildRequest 失败 / Feed 中途出错 / panic；幂等。
//   - Session 三方法刻意合并到一个接口，便于共享私有 buffer / 解析器状态；按 phase 拆会逼出复杂的状态传递。
//
// Feed 返回字节的生命周期：
//   - 返回的 []byte 在**下次 Feed / Finalize / Close 之前**有效。
//   - 调用方必须在该窗口内 Write 出去；不要长期持有。
//   - 实现可借此复用底层 buffer（sync.Pool 等），避免每次 alloc。
type Session interface {
	BuildRequest() (*http.Request, error)
	Feed(chunk []byte) ([]byte, error)
	Finalize() FinalizeResult
	io.Closer // Close 释放 Session 持有的资源；必须由 dispatch defer 调用；幂等
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

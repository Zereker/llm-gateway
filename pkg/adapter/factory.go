// Package adapter 定义 vendor HTTP-layer 的契约 + 注册表。
//
// **架构定位（v0.6 facade）**：
//
//	pkg/protocol.Handler = Combine(adapter.Factory, translator.Translator)
//
// adapter 只管 vendor 特定的 HTTP 层（URL / auth headers / TLS / proxy）；body
// shape 翻译走 pkg/translator；端到端协议处理走 pkg/protocol.Handler facade。
// 消费侧（dispatcher / invoker / eligibility）只看 protocol.Handler，不直接接触
// adapter.Factory——adapter 是 facade 内部细节。
//
// **新增 vendor 步骤**：
//  1. 实现 Factory + Session（slim 版：BuildRequest(body) + Close()）
//  2. init() 注册到 adapter.Register
//  3. 如果跟现有 src/tgt 协议组合没覆盖：在 pkg/translator/<from>_<to>/ 加 Translator
//  4. cmd/gateway 加 blank import
//
// 例：
//   - DeepSeek / ARK：vendor=ark，endpoint.Protocol=OpenAI（identity translator）
//   - Vertex Gemini：vendor=gemini，endpoint.Protocol=Gemini（客户端 OpenAI → openai_gemini）
//   - Anthropic Claude：vendor=anthropic，endpoint.Protocol=Anthropic（identity 或 openai_anthropic）
package adapter

import (
	"context"
	"net/http"

	"github.com/zereker/llm-gateway/pkg/domain"
)

// Metadata 是静态、厂商级别的元信息（不绑定具体请求）。
//
// 由 Factory.Metadata() 返回；启动期就能拿到，可用来：
//   - 与 ConfigStore 中的 vendor 集合做覆盖比对（漏注册告警）
//   - protocol.Capabilities 透出 SupportedModalities 给 eligibility 过滤
//   - 调度日志 / metric 标签
//
// **不带 NativeProtocol**：协议归属是 endpoint 级属性（domain.Endpoint.Protocol），
// 不是 vendor 级——同 vendor 可以挂多条 endpoint 走不同协议。
type Metadata struct {
	Vendor              string            // vendor 名（跟 endpoints.vendor 对齐）
	SupportedModalities []domain.Modality // 能处理的模态
}

// Factory 是注册到 adapter registry 的工厂。
//
// 一个 vendor 一个 factory；factory 本身无状态、单实例。
// 每次请求由 NewSession 构造一个 Session 实例。
//
// Factory 实现 MUST be safe for concurrent use（多 gin handler goroutine 同时调用 NewSession）。
type Factory interface {
	Metadata() Metadata

	// NewSession 创建本次请求专属的 Session。
	NewSession(c context.Context, ep *domain.Endpoint, env *domain.RequestEnvelope) (Session, error)
}

// Session **slim 版**（v0.5 重构）：只负责构造上游 HTTP 请求 + 释放资源。
//
// 不再有 Feed / Finalize / FinalizeResult——chunk 流处理 + usage 提取
// 全部搬到 pkg/translator.ResponseHandler。
//
// **契约**：
//   - 单 goroutine 使用（与 gin handler 同协程）；实现无需自加锁
//   - BuildRequest 调一次；body / extraHeaders 都是 caller（pkg/protocol.combine）
//     已经跑完 translator + quirks 之后的最终产物
//   - Close 必须在所有路径上 defer 调用；幂等
type Session interface {
	// BuildRequest 构造发往上游的 HTTP request。
	//
	// **参数**：
	//   - body：translator + quirks.RewriteBody 跑完后的字节（直接塞进 req.Body）
	//   - extraHeaders：quirks.RewriteHeader 跑完后的最终 header；nil 表示无额外
	//     header。adapter 应先把 extraHeaders 拷贝到 req.Header，再写自己协议
	//     必需的 Auth / Content-Type 等（adapter 的协议头**最后写，覆盖** quirks
	//     ——避免 deployer 误改 Authorization 把请求打挂）
	BuildRequest(body []byte, extraHeaders http.Header) (*http.Request, error)

	// Close 释放 Session 持有的资源；必须由 dispatch defer 调用；幂等。
	Close() error
}

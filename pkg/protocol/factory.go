package protocol

import (
	"context"
	"fmt"
	"net/http"

	"github.com/zereker/llm-gateway/pkg/domain"
)

// =============================================================================
// vendor Factory / Session / Metadata
// =============================================================================
//
// **架构关系**：
//
//	Handler = Combine(Factory, translator.Translator)
//
// Factory 管 vendor HTTP 层（URL / auth headers / TLS / proxy）；body shape
// 翻译走 pkg/translator；端到端协议处理走 Handler facade。
//
// **facade 边界**（重要——v0.7 合并 pkg/adapter 进 pkg/protocol 之后的纪律）：
//
//   允许直接消费 Factory / Session / RegisterFactory / LookupFactory：
//     - 本包内部（combine.go / registry.go）
//     - pkg/protocol/<vendor>/ 子包（init() 注册自己）
//     - cmd/gateway（composition root，目前没用，留出口给未来 cli 自检）
//
//   **禁止** 在 pkg/dispatch / pkg/middleware / pkg/invoker / pkg/selector /
//   pkg/router 等数据面里 type-assert 或直接调 Factory / LookupFactory。它们
//   只通过 protocol.Handler / protocol.Lookup 两个 facade 类型跟协议层交互。
//
//   反例：dispatch 里 LookupFactory(ep.Vendor) 拿出来 type-assert 判 vendor 走
//   不同逻辑——这等于把 vendor 知识扩散到调度层，废掉了 facade 的价值。
//
// **新增 vendor 步骤**：
//  1. 在 pkg/protocol/<vendor>/ 写一个实现 Factory + Session 的 struct
//  2. init() 调 protocol.RegisterFactory("<vendor>", yourFactory)
//  3. 如果客户端协议跟 endpoint.Protocol 之间没覆盖：在
//     pkg/translator/<src>_<tgt>/ 加 Translator
//  4. cmd/gateway 加 blank import 触发 init()
//
// 例：
//   - DeepSeek / ARK：vendor=ark，endpoint.Protocol=OpenAI（identity translator）
//   - Vertex Gemini：vendor=gemini，endpoint.Protocol=Gemini（客户端 OpenAI → openai_gemini）

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

// Factory 是注册到 vendor registry 的工厂。
//
// 一个 vendor 一个 factory；factory 本身无状态、单实例。
// 每次请求由 NewSession 构造一个 Session 实例。
//
// Factory 实现 MUST be safe for concurrent use（多 gin handler goroutine 并发调 NewSession）。
type Factory interface {
	Metadata() Metadata

	// NewSession 创建本次请求专属的 Session。
	NewSession(c context.Context, ep *domain.Endpoint, env *domain.RequestEnvelope) (Session, error)
}

// Session **slim 版**：只负责构造上游 HTTP 请求 + 释放资源。
//
// 不再有 Feed / Finalize / FinalizeResult——chunk 流处理 + usage 提取全部搬到
// pkg/translator.ResponseHandler。
//
// **契约**：
//   - 单 goroutine 使用（与 gin handler 同协程）；实现无需自加锁
//   - BuildRequest 调一次；body / extraHeaders 都是 caller（pkg/protocol.combined）
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

// =============================================================================
// vendor Factory registry
// =============================================================================

var factoryRegistry = map[string]Factory{}

// RegisterFactory 注册一个 vendor Factory；vendor 名作为 registry key。
//
// **vendor != Metadata().Vendor 的场景**：OpenAI-compatible 别名（ark / deepseek /
// qwen 等）复用同一个 Factory，但要在 registry 里挂多个名字。所以 vendor 是显式
// 参数，不从 Metadata 派生。
//
// 契约：
//   - **MUST** 在 init() 阶段调用；运行期调用不安全（registry 无锁；
//     且 LookupFactory 在请求热路径上无锁读，依赖 init 完成的内存可见性）
//   - 同名重复注册 panic（启动期失败比静默覆盖好）
func RegisterFactory(vendor string, f Factory) {
	if vendor == "" {
		panic("protocol: RegisterFactory vendor name empty")
	}
	if _, ok := factoryRegistry[vendor]; ok {
		panic(fmt.Sprintf("protocol: vendor %q already registered", vendor))
	}
	factoryRegistry[vendor] = f
}

// LookupFactory 根据 vendor 取出 Factory；未注册返回 nil。
// 假设所有 RegisterFactory 调用已在 init() 阶段完成；运行期只读。
func LookupFactory(vendor string) Factory {
	return factoryRegistry[vendor]
}

// ResetFactories 清空 vendor 注册表——**仅供测试**。
//
// 生产环境不应调（factory 注册在 init() 阶段一次性完成；Reset 之后 LookupFactory
// 全返 nil → DefaultLookup 全返 nil → 所有请求 503）。
func ResetFactories() {
	factoryRegistry = map[string]Factory{}
}

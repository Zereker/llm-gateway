// Package adapter 定义 vendor adapter 的契约 + 注册表。
//
// **v0.5 架构（slim adapter + translator）**：
//
//	┌──────────┐       ┌────────────┐       ┌─────────┐       ┌──────────┐
//	│ Client   │  ───→ │ Translator │  ───→ │ Adapter │  ───→ │ Upstream │
//	└──────────┘       └────────────┘       └─────────┘       └──────────┘
//
// Adapter 的责任收窄到只管 HTTP 层（URL / auth headers / TLS / proxy 等 vendor 特定细节），
// 数据 shape / SSE 解析 / usage 提取等"协议层"任务搬到 pkg/translator。
//
// **新增 vendor 步骤**：
//  1. 实现 Factory + Session（slim 版：BuildRequest(body) + Close()）
//  2. init() 注册到 adapter.Register
//  3. 如果上游协议跟客户端不同：在 pkg/translator/<from>_<to>/ 加 Translator
//  4. cmd/gateway 加 blank import
//
// 例：
//   - DeepSeek / ARK：vendor=ark + OpenAI 协议族（identity translator 透传）
//   - Vertex Gemini：vendor=gemini + openai_gemini Translator 翻译
//   - Anthropic Claude：vendor=anthropic + openai_anthropic Translator（future）
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
//   - 路由层根据 SupportedModalities 做能力过滤
//   - 调度日志 / metric 标签
//   - **关键**：M7 用 NativeProtocol 找对应 Translator（envelope.SourceProtocol → NativeProtocol）
type Metadata struct {
	Vendor              string            // vendor 名（跟 endpoints.vendor 对齐）
	NativeProtocol      domain.Protocol   // 上游使用的协议
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
//   - BuildRequest 调一次；body 是 Translator 已经翻译好的"上游协议"字节
//   - Close 必须在所有路径上 defer 调用；幂等
type Session interface {
	// BuildRequest 构造发往上游的 HTTP request。
	//
	// body 是 translator 已经翻译过的字节（直接塞进 request body）。
	// adapter 的工作：URL / auth header / Content-Type / 其它 vendor 特定 header。
	BuildRequest(body []byte) (*http.Request, error)

	// Close 释放 Session 持有的资源；必须由 dispatch defer 调用；幂等。
	Close() error
}

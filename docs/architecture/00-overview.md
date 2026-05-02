# 00 — Overview

## 1. 项目目标

ai-gateway 是一个统一的 LLM 推理请求入口，对上承接客户端流量（兼容多种主流协议），对下路由到多种推理后端：

- **闭源 SaaS**：OpenAI、Anthropic、Google Vertex、AWS Bedrock、Azure OpenAI、Mistral、DeepSeek …
- **开源自部署**：vLLM、Ollama、Text Generation Inference、SGLang …

核心要解决的问题：

1. **协议碎片化**：客户端可能用 OpenAI / Anthropic / 自定义协议；后端可能是任意厂商。需要一层透明转换。
2. **endpoint 多样性 + 不可靠**：同一模型在多个厂商或多个区域有部署，单 endpoint 失败需自动重试或换线路。
3. **多租户公平性**：不同用户、不同模型、不同上游需要独立限流与隔离。
4. **可计量性**：每个请求的 token 用量、成本、延迟、错误必须被准确记录，支持后续计费、成本归因和容量规划。
5. **可观测性**：单请求可追踪、聚合指标可告警、异常可还原。

## 2. 非目标

明确**不做**的事，避免设计膨胀：

- ❌ 不做 prompt 工程框架（不是 LangChain）
- ❌ 不做模型服务本身（不是 vLLM、TGI）
- ❌ 不做 RAG / 向量检索（不是 LlamaIndex）
- ❌ 不做应用层业务逻辑（不是 BFF）
- ❌ 不做多模型 ensemble / chain（每请求只路由到一个 endpoint）

## 3. 设计原则

| # | 原则 | 含义 |
|---|------|------|
| P1 | **Middleware-first** | 横切关注点（鉴权、限流、调度、错误处理、计量）全部以 middleware 组合实现，不放在业务 handler 里。Handler 极简化（< 30 行）。 |
| P2 | **typed RequestContext + gin.Context 存取** | 跨 middleware 的请求级状态用一个 typed `*domain.RequestContext` struct 承载（保证类型安全），通过 `gin.Context.Set/Get` 传递（保证与 gin 生态兼容、不污染函数签名）。**杜绝**散落的 `c.Set("foo", ...)` `c.Set("bar", ...)`。 |
| P3 | **Adapter per provider** | 每个上游厂商一个 `Adapter` 实现，封装协议、签名、URL、流式解析、Usage 提取等所有差异。新增厂商不改主链路，只加 Adapter。 |
| P4 | **Canonical request schema** | 内部用一套规范化请求结构（`CanonicalRequest`，OpenAI 兼容形态）作为通用语言；客户端协议 → Canonical 由 `Envelope` 完成；Canonical → 上游协议由 `Adapter.TransformRequest` 完成。 |
| P5 | **Pluggable infrastructure** | 所有外部依赖（配置中心、身份系统、预算检查、计量事件总线）都通过接口抽象。默认实现走本地文件 / SQLite / 内存，**零外部依赖即可启动**；生产可挂 etcd / Kafka / Postgres。 |
| P6 | **预检与扣减分离** | 限流的预检（read-only）在请求前做，真实扣减（用真实 Usage）在响应后做。两阶段分离避免"预扣失败回滚"的复杂性。 |
| P7 | **失败可降级，错误必分类** | 上游失败按错误类（`ErrTransient` / `ErrRateLimit` / `ErrPermanent` / `ErrInvalid` / `ErrUnknown`）分类，重试策略和 Cooldown 时长按类决定。错误不分类等于不可重试。 |
| P8 | **可观测性内置** | 每个 middleware 自带 metric（duration / count / error）；trace 字段（如 `scheduling_decision`）落到结构化日志；Prometheus + OpenTelemetry 一等公民。 |

## 4. 系统全景

### 4.1 单请求生命周期

```
HTTP request
   │
   ▼
┌────────────────────────────────────────────────────────────────┐
│  M1  TraceContext        生成 trace_id / request_id，初始化 logger │
│        │                                                            │
│  M9  Recover(defer)      panic 防护（注册早，覆盖整条链）            │
│        │                                                            │
│  M2  Auth                身份识别 → rc.Identity（UserID / Group） │
│        │                                                            │
│  M3  Envelope            读 body、识别协议与 modality、解析 Canonical │
│        │                                                            │
│  M4  Budget              预算 / 配额检查（IdentityProvider 接口）    │
│        │                                                            │
│  M5  ModelService        模型路由配置加载 → rc.ModelService + Pricing │
│        │                                                            │
│  M6  Limit               三层限流预检（read-only）                   │
│        │                                                            │
│  M8  ContentModeration   请求内容审核（可选）                        │
│        │                                                            │
│  M7  Schedule            选 endpoint → Adapter → 流式响应            │
│        │  ↳ RetryExecutor: L1 同 endpoint 重试 / L2 换 endpoint     │
│        │                  /  L3 换模型（可选） + Cooldown 反馈       │
│        ▼                                                            │
│  ResponseWriter          流式或非流式回写客户端                       │
│                                                                      │
│  M10 Tracing(defer)      聚合 metric、发送 Usage 事件、写 trace      │
└────────────────────────────────────────────────────────────────┘
   │
   ▼
HTTP response（流式 SSE 或一次性 JSON）
```

### 4.2 组件分层

```
┌────────────────────────────────────────────────────────┐
│  HTTP layer (gin)                                       │
├────────────────────────────────────────────────────────┤
│  Middleware chain (M1-M10)                              │
├────────────────────────────────────────────────────────┤
│  Domain services                                        │
│  ├─ Adapter Registry        → 协议转换层（02）          │
│  ├─ Scheduler + Filters     → 端点选择层（03）          │
│  ├─ LimitChecker            → 限流层（04）              │
│  ├─ TokenExtractor          → 计量提取层（05）          │
│  └─ Pricing Engine          → 计价层（05）              │
├────────────────────────────────────────────────────────┤
│  Infrastructure abstractions（06）                      │
│  ├─ ConfigStore             → etcd / file / SQLite     │
│  ├─ IdentityProvider        → APIKey / JWT / 自定义    │
│  ├─ BudgetChecker           → 内存 / 远程 SDK         │
│  ├─ UsageEventBus           → Kafka / 文件 / 内存     │
│  └─ Cache (Redis / 本地)    → 限流 / 配置缓存         │
└────────────────────────────────────────────────────────┘
```

## 5. 关键术语

| 术语 | 定义 |
|------|------|
| **RequestContext** | 一次 HTTP 请求的全链路可变状态，typed struct，存放在 `gin.Context` 中。详见 [01](01-request-pipeline.md)。 |
| **Envelope** | 解析后的请求信封，包含原始字节、Canonical 解析结果、源协议、modality 等。 |
| **CanonicalRequest** | 网关内部统一的请求形态（OpenAI 兼容 schema）；所有 Adapter 输入都是它。 |
| **Adapter** | 对接单个上游厂商的实现，封装 URL / Header / Body 转换、流式解析、Usage 提取。一厂商一 Adapter（可跨多 modality）。 |
| **Translator** | 双向协议翻译模块（如 Anthropic ↔ OpenAI），独立于 Adapter。 |
| **Scheduler** | 端点选择器，按 Filter 链 + 打分机制从候选 endpoint 池中选出一个。 |
| **RetryExecutor** | 包装 Adapter 调用的重试与降级控制器。 |
| **Endpoint** | 一个具体的上游接入点（厂商 + 区域 + 凭证 + URL）。同一模型可有多个 endpoint。 |
| **ModelService** | 一个对外暴露的模型名（如 `gpt-4o`），背后绑定一组 endpoint 与限流 / 计价配置。 |
| **Modality** | 请求模态：`chat` / `message` / `embedding` / `image` / `audio` / `task` 等。 |
| **LimitSpec** | 单次请求的三层限流阈值（用户层 / 模型层 / endpoint 层）。 |
| **Usage** | 一次请求的资源消耗：input/output/total tokens、reasoning tokens、模态相关字段（图片张数、音频秒数等）。 |
| **PricingSpec** | 模型的价格规格（每 1k input/output token 的单价、最低消费等），版本化存储。 |
| **Provider** | 厂商概念，与 Adapter 一一对应（`openai` / `anthropic` / `vllm` / ...）。 |

## 6. 阅读路径

### 实现者视角

按编号顺序读 01 → 07，能从 RequestContext 数据结构、middleware 契约一路读到部署清单。

### 接入者视角（想加新 Provider）

只读 02（Adapter 接口）+ 06（IdentityProvider / ConfigStore 注入）即可。

### 运维视角

读 06（基础设施抽象）+ 07（部署 / 监控 / 告警）。

### 评审者视角

读 00（本文档）→ 06（架构边界）→ 07（路线图），即可对整体设计形成判断。

## 7. 开源声明

- **License**：Apache-2.0
- **零云依赖启动**：所有组件均有本地 / 内存默认实现，单二进制可跑通端到端流程
- **无内部代码遗留**：所有抽象接口与默认实现均为通用术语，不绑定任何特定云厂商或内部系统

## 8. 文档版本

| 版本 | 日期 | 说明 |
|------|------|------|
| v0.1-draft | 2026-05-02 | 初版（基于内部成熟设计的开源化适配） |

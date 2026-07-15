[English](10-policy-enforcement.md) | [简体中文](10-policy-enforcement.zh-CN.md)

# 可插拔策略执行

网关是供应商中立的策略执行点，而不是内容安全或 DLP 产品。
检测逻辑由可替换的 `policy.Engine` 提供；M8 负责将决策准确地
应用到客户端请求或转换后的响应。

```text
trusted identity + runtime-only text
                 |
                 v
            policy.Engine
                 |
        allow / deny / redact
                 |
                 v
       M8 enforcement + safe audit
```

## 决策合约

`internal/policy.Engine` 接收执行阶段、可信主体、模型/模态、
仅存在于运行时的内容、已提取文本段和所选策略引用。它返回
`allow`、`deny` 或 `redact`，并携带策略版本、稳定的 rule ID 和
reason code。引擎错误、无效决策、无效 mutation、不支持的结构和
缓冲区溢出一律 fail-closed。

JSON `AuditRecord` 不包含内容字节和替换值。mutation 审计只记录 ID、
kind 和 RFC 6901 target。引擎与检测器错误不会进入 decision 或客户端响应。

M8 在洋葱模型的返回阶段负责审计。下游执行期间，它收集输入和
输出记录，并在 `c.Next()` 返回后发出 `policy_decision`。每条记录
都区分策略动作和执行结果：
`allowed`、`denied`、`applied` 或 `failed`。M10 不再中继策略状态。

## 绑定和持久化

不可变策略定义存放在 `policy_definitions`；`policy_bindings` 为
可信作用域选择一个定义。解析通过一次查询按以下优先级完成：

```text
API key > project > account > global
```

定义是完整快照，不做合并。命中和未命中结果都使用有界 TTL 缓存；
Console 写入后发布 cache-bus 失效事件。Project 已存在于领域契约，
但在认证和 RBAC 能提供可信 project identity 之前，Console 拒绝创建
project binding。

没有 binding 时，现有 moderation 配置保持原有行为。如果 active binding
要求执行输入或输出策略，但没有配置 engine，则请求 fail-closed。响应通过
以下 header 公开本次生效的策略：

- `X-Gateway-Policy-ID: <policy-id>@<version>`
- `X-Gateway-Policy-Output-Mode: disabled|strict_buffered|best_effort_streaming`

## 请求提取和编辑

`JSONDocumentAdapter` 仅从 OpenAI 中提取已知的用户/内容文本节点
聊天、响应、Anthropic、Gemini 和嵌入结构的 JSON。它不
提取路由字段、工具定义、图像 URL 或任意元数据。
段携带 RFC 6901 JSON 指针，例如 `/messages/0/content` 或
`/input/0/content/0/text`。

对于 `redact`，每个 target 都必须对应一个已提取的 UTF-8 文本节点。
target 未知、重复、非文本、指向路由字段或格式错误时，整个 mutation 集合
都会被拒绝。适配器以原子方式重建新的 JSON 文档，保留路由和协议字段，
同时替换请求信封和 HTTP request body，之后才允许进行协议转换和上游调度。
因此，部分应用 mutation 的请求永远不会被转发。

Console 模拟接口接收合成 decision 和 request body，使用同一套适配器执行，
但不会调用检测引擎或上游模型：

- `GET/POST /api/v1/policies`
- `DELETE /api/v1/policies/:policyID`
- `GET/POST /api/v1/policy-bindings`
- `DELETE /api/v1/policy-bindings/:scopeKind?scope_id=...`
- `POST /api/v1/policies/simulate`

## 响应模式

`disabled` 跳过输出评估。其他模式刻意不同
保证：

| 模式 | 交付方式 | 保证与失败行为 |
|---|---|---|
| `strict_buffered` | 提交响应 header 前，缓冲完整且已转换的客户端响应 | 对完整响应评估一次；allow / redact 成功后原子释放；deny、引擎错误、无效 mutation 或缓冲区溢出会在首字节前返回网关错误 |
| `best_effort_streaming` | 每个通过检查的 frame 立即发送 | 跨 SSE frame 累积解码文本，可以识别被拆分的敏感词；后续违规只能截断已经提交的 stream，无法撤回已发送字节；流式 redact fail-closed |

严格模式默认每个响应最多缓冲 4 MiB，可按策略定义配置，且拒绝超过
64 MiB 的配置值。best-effort 解码维护有界的 64 KiB 滚动文本窗口。
严格模式会增加与响应大小成正比的内存占用和完整响应延迟。严格 redact
要求缓冲后的客户端输出是受支持的 JSON 文档；完整 SSE transcript 不是
单个 JSON 文档，因此 redact 会 fail-closed。

Invoker 会延迟提交上游响应 header / status，直到出现第一个可发送的客户端
字节。因此严格模式失败仍可返回正常 JSON 错误。best-effort stream 一旦提交，
后续错误只会记录为截断用量，不能再触发重试或回退。

## 扩展

Denylist、OpenAI moderation、`Moderator`、guard chain 和 stream decorator
统一通过 `moderation.ModeratorEngine` 转换。新的集成应实现
`policy.Engine`，并通过 `middleware.WithPolicyEngine`（或
`router.Deps.PolicyEngine`）注入；检测逻辑和供应商凭据仍位于网关策略领域之外。

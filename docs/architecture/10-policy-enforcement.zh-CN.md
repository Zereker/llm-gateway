# 可插拔策略执行层

Gateway 是厂商无关的策略执行点，不自行建设内容安全或企业 DLP 产品。可替换的
`policy.Engine` 负责返回决策，M8 负责把决策应用到客户端请求或翻译后的响应。

## 决策与审计

引擎接收阶段、可信身份、模型/模态、仅在运行时存在的内容、结构化文本片段及已选中的
持久化策略引用，返回带版本、规则 ID 和原因码的 `allow`、`deny` 或 `redact`。
引擎异常、非法决策、无效 mutation、不支持的文档结构及缓冲区溢出都会 fail-closed。

原始内容、替换值和内部错误不会进入 JSON 或 `AuditRecord`；mutation 审计只记录
ID、类型和 RFC 6901 目标。显式引擎的 `Decision.Cause` 不会返回给客户端，只有
legacy adapter 为兼容旧行为保留输入审核错误文案。

M8 在洋葱模型的返回阶段统一写审计：下游执行时收集输入与输出决策，`c.Next()`
返回后写出 `policy_decision`。审计同时区分决策动作与执行结果：`allowed`、
`denied`、`applied`、`failed`。M10 不传递策略状态。

## 持久化与优先级

不可变版本存放在 `policy_definitions`，`policy_bindings` 将完整策略绑定到可信作用域：

```text
API Key > Project > Account > Global
```

不同作用域不会合并。解析使用单次查询、正/负 TTL 缓存；Console 写操作通过 cache bus
广播失效。Project 已进入领域契约，但在可信 Project 身份和 RBAC 完成前 Console
拒绝创建 Project binding。

没有 binding 时，原有 moderation 配置保持原行为；已有 binding 需要执行而未配置
engine 时 fail-closed。响应暴露实际配置：

- `X-Gateway-Policy-ID: <policy-id>@<version>`
- `X-Gateway-Policy-Output-Mode: disabled|strict_buffered|best_effort_streaming`

## 请求提取与脱敏

`JSONDocumentAdapter` 只提取 OpenAI Chat、Responses、Anthropic、Gemini 和 embedding
JSON 中已知的用户/内容文本节点，不提取模型路由、工具定义、图片 URL 或任意 metadata。
文本节点以 RFC 6901 JSON Pointer 标识，例如 `/messages/0/content`。

执行 `redact` 时，每个目标必须是已提取的 UTF-8 文本节点。未知、重复、非文本、路由
字段或非法指针会使整组 mutation 失败。适配器原子重建 JSON，同时替换 envelope 与
HTTP request body，之后才允许协议翻译和上游请求，因此不会转发半完成的脱敏结果。

Console 的 Governance 页面与 API 支持策略版本发布、停用、绑定和合成决策模拟；模拟
复用同一 mutation executor，但不会调用检测器或上游模型：

- `GET/POST /admin/policies`
- `DELETE /admin/policies/:policyID`
- `GET/POST /admin/policy-bindings`
- `DELETE /admin/policy-bindings/:scopeKind?scope_id=...`
- `POST /admin/policies/simulate`

## 响应模式

| 模式 | 行为与保证 |
|---|---|
| `disabled` | 不执行输出策略 |
| `strict_buffered` | 在提交响应头前缓冲并一次性检查完整的客户端输出；allow/redact 原子释放，deny、引擎异常、非法 mutation 或超限会在首字节前返回网关 JSON 错误 |
| `best_effort_streaming` | 每个已通过的 frame 立即发送；解码文本会跨 SSE frame 累积，可以发现跨 frame 的敏感词，但首字节之后的违规只能截断，已发送内容无法撤回；流式 redact fail-closed |

strict 默认每个响应最多缓冲 4 MiB，可按策略配置且上限为 64 MiB；best-effort 使用
有界的 64 KiB 滚动文本窗口。strict 的代价是响应大小级别的内存和完整
响应延迟。strict redact 要求缓冲后的客户端输出是支持的 JSON 文档；完整 SSE transcript
不是单个 JSON，因此脱敏会 fail-closed。

Invoker 会延迟上游响应头和状态码，直到出现第一个通过策略的客户端字节。因此 strict
失败仍可返回正常 JSON 错误；best-effort 一旦提交后则不能 retry/fallback，并会记录
truncated usage。

## 兼容与扩展

原有 denylist、OpenAI moderator、`Moderator`、guard chain 和 stream decorator 通过
`moderation.LegacyEngine` 继续工作。新的检测/DLP 集成应实现 `policy.Engine`，通过
`middleware.WithPolicyEngine` 或 `router.Deps.PolicyEngine` 注入，Gateway 不接管检测
规则与厂商凭据。

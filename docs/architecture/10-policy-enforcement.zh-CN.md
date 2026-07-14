# 可插拔策略执行层

Gateway 的职责是作为策略执行点，而不是自行建设完整的内容安全、法规知识库或企业 DLP 平台。检测引擎负责决策，Gateway 负责执行。

`internal/policy.Engine` 接收输入/输出阶段、可信身份、模型与模态信息，以及仅在运行时存在的内容；返回带策略版本、规则 ID 和原因码的 `allow`、`deny` 或 `redact` 决策。

- `allow`：继续执行请求。
- `deny`：返回稳定的 `content_rejected`，不暴露新引擎的内部原因。
- `redact`：携带明确 mutation；M2.2 的安全 mutation executor 完成前必须 fail-closed。
- 引擎异常或非法决策：fail-closed。

审计记录只包含策略引用、动作、规则、原因码，以及 mutation 的 ID、类型和结构目标。原始内容、匹配值、替换值和内部错误不会进入 JSON 或安全审计。

M8 拥有这些审计事件：在下游处理期间收集决策，并在 `c.Next()` 返回后通过共享 `AuditTracer` 统一写出。M10 不传递、也不解析 policy 状态。

策略绑定优先级为：

```text
API Key > Project > Account > Global
```

作用域只能来自认证后得到的可信身份，不能由调用者 Header 指定。同一作用域选择最高的已启用不可变版本。Project 已进入领域契约，但在可信 Project 身份和 RBAC 完成前不会开放产品配置。

原有 `moderation.driver`、denylist、OpenAI moderator、`Moderator`、guard chain 和流式 wrapper 保持兼容，通过 `moderation.LegacyEngine` 转换为新决策契约。新实现应直接实现 `policy.Engine`。

M2.1 只建立决策和执行边界，不提升流式安全保证。严格缓冲与 best-effort 流式扫描仍属于 M2.3。

# Architecture

本目录是 ai-gateway 的架构与实现规范文档，是代码实现的**唯一真源**。

## 文档列表

按依赖顺序阅读：

| # | 文档 | 主题 |
|---|------|-----|
| 00 | [overview](00-overview.md) | 项目目标、设计原则、系统全景、术语表 |
| 01 | [request-pipeline](01-request-pipeline.md) | `RequestContext` 数据结构 + 10 个 middleware 的契约（输入/输出/错误/顺序约束） |
| 02 | [protocol-translation](02-protocol-translation.md) | `Adapter` / `RequestEnvelope` / `Translator` / `ResponseSession` 接口与数据流 |
| 03 | [endpoint-scheduling](03-endpoint-scheduling.md) | `Scheduler` + Filter 链 + `RetryExecutor` 三级降级 + Cooldown / PrefixCache |
| 04 | [rate-limiting](04-rate-limiting.md) | 三层限流（用户 / 模型 / endpoint）+ `LimitSpec` 四级查询链 + 预检 / Consume 协议 |
| 05 | [metering-billing](05-metering-billing.md) | `Usage` 数据总线 + `TokenExtractor` + `PricingSpec` 版本化 + 异步管道 |
| 06 | [pluggable-infra](06-pluggable-infra.md) | `ConfigStore` / `IdentityProvider` / `BudgetChecker` / `UsageEventBus` 抽象接口与默认实现策略 |
| 07 | [roadmap](07-roadmap.md) | 开源版分阶段路线图（v0.1 MVP → v0.5 → v1.0）+ 验收标准 |

## 文档约定

- **接口签名权威**：文档中的 Go interface / struct 字段是实现规范，PR 修改代码必须同步修改对应文档。
- **跨主题改动**：涉及多个主题的设计变更（如新增 middleware、新增字段到 `RequestContext`），需在同一 PR 中更新所有相关文档。
- **示例代码**：仅示例，不要求逐字与代码一致；接口签名、字段名、错误语义是规范。

## 适用范围

- ✅ 服务端架构、组件接口、跨组件协议
- ✅ 可扩展性 / 插件接入指南（写在 06）
- ❌ 用户使用指南（放在 `docs/usage/`）
- ❌ Provider 接入教程（放在 `docs/providers/`）
- ❌ 运维 / 部署文档（放在 `docs/operations/`）

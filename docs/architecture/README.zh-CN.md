[English](README.md) | [简体中文](README.zh-CN.md)

# 架构

该目录是 `llm-gateway` 的架构和接口合约的单一事实来源。该代码实现了此处记录的内容；如果实现需要更改此处描述的边界，请先更新此目录，然后更改代码。

## 文档索引

| # | 文档 | 主题 |
|---|------|------|
| 00 | [概述](00-overview.zh-CN.md) | 系统边界、组件分层、请求生命周期 |
| 01 | [请求管道](01-request-pipeline.zh-CN.md) | `internal/requeststate.State` 和中间件链 |
| 02 | [协议转换](02-protocol-translation.zh-CN.md) | Handler 门面、Factory / Translator / Quirks、上游转发边界 |
| 03 | [端点调度](03-endpoint-scheduling.zh-CN.md) |端点候选、批量选择、显式回退、运行时评分 |
| 03a | [调度概述](03a-schedule-overview.zh-CN.md) | 调度模块快速参考与入门指南（数据流、包职责、装配点） |
| 04 | [限速](04-rate-limiting.zh-CN.md) |用户侧RPM/RPS预扣、TPM后扣、端点配额 |
| 05 | [计量计费](05-metering-billing.zh-CN.md) |内容记录、使用事件、指标/跟踪、下游计费边界 |
| 06 | [可插拔基础设施](06-pluggable-infra.zh-CN.md) | DB、Redis、Kafka、OTel、预算和审核的注入点 |
| 07| [配置](07-configuration.zh-CN.md) |网关配置架构、环境变量覆盖、验证规则 |
| 08 | [可观测性](08-observability.zh-CN.md) |日志记录、指标、跟踪、使用事件、内容日志可观察性合同 |
| 09| [路由策略](09-routing-policy.zh-CN.md) |虚拟模型、确定性约束、策略缓存、控制台 API |
| 10 | [策略执行](10-policy-enforcement.zh-CN.md) | 可插拔决策、作用域优先级、安全审计、旧版兼容性 |

## 架构亮点

- 数据平面为`cmd/gateway`；还提供单独的 `cmd/console` 控制平面二进制文件（管理 API，由 `internal/console` 支持）。业务数据通过直接 SQL 进行管理，控制台是管理数据的可选附加方式 — 数据平面从不依赖于它。
- SQL 中的目录、端点、API 密钥、订阅和配额策略数据；网关启动在验证结果架构之前应用版本化架构更改。
- 网关依赖 Redis 进行 M6 速率限制和调度程序冷却。
- SQL写入通过[repo进程内TTL LRU缓存](./06-pluggable-infra.zh-CN.md#8-repo-cache-deployer-sql--gateway-data-propagation)（默认30秒）传播到网关，而不是每个请求查询相同的表；数据平面是100%只读的，因此TTL窗口就足够了。
- 面向客户端的入口覆盖 OpenAI Chat、Anthropic Messages、OpenAI Responses、Images、Audio 和 Embeddings；Gemini 仅作为上游协议支持，不作为客户端入口公开。
- `internal/protocol` 既提供 Handler 门面，也承载供应商 Factory / Session 实现（HTTP 层工厂）和端点级 Quirks DSL；协议结构转换位于 `internal/translator`，用量提取位于 `internal/usage`。消费者只依赖 `protocol.Handler` / `protocol.Lookup`，不对 Factory 做类型断言。
- 所有中间件连接均使用接口选项模式（与 otelgin v0.68.0 一致）；参见[06§6](./06-pluggable-infra.zh-CN.md#6-middleware-options)和[01§10](./01-request-pipeline.zh-CN.md#10-middleware-assembly-contract-aligned-with-otelgin-v0680)。

## 维护约定

- 当更改 `internal/requeststate.State`、中间件顺序、适配器/转换器接口、模式或配置字段时，请在同一更改中更新此目录。
- 示例代码仅说明关键合约；它不需要逐字匹配实现，但字段名称、组件边界和错误语义必须准确。
- 添加新的缓存存储库包装器、中间件选项或指标时，将其注册到 06 / 07 / 08 的相应部分中。

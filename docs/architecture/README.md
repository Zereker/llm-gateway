# Architecture

本目录记录 `llm-gateway` 的目标架构约定。项目仍在设计阶段，代码实现以本文档为准；若实现方案需要改变目标边界，应先更新本目录再改代码。

## 文档列表

| # | 文档 | 主题 |
|---|------|------|
| 00 | [overview](00-overview.md) | 目标系统边界、组件分层、请求生命周期 |
| 01 | [request-pipeline](01-request-pipeline.md) | `domain.RequestContext` 与 middleware 链路 |
| 02 | [protocol-translation](02-protocol-translation.md) | Handler facade、Factory / Translator / Quirks、上游转发边界 |
| 03 | [endpoint-scheduling](03-endpoint-scheduling.md) | endpoint 候选、批内选择、显式 fallback、runtime scoring |
| 03a | [schedule-overview](03a-schedule-overview.md) | schedule 模块速查 / 上手伴读（数据流、各包职责、装配点） |
| 04 | [rate-limiting](04-rate-limiting.md) | 用户侧 RPM/RPS 前扣、TPM 后扣、endpoint quota |
| 05 | [metering-billing](05-metering-billing.md) | 内容记录、Usage Event、Metrics / Trace、下游计费边界 |
| 06 | [pluggable-infra](06-pluggable-infra.md) | DB、Redis、Kafka、OTel、预算和审核的注入点 |
| 07 | [configuration](07-configuration.md) | gateway 配置 schema、环境变量覆盖、校验规则 |
| 08 | [observability](08-observability.md) | 日志、指标、trace、Usage Event、Content Log 观测契约 |

## 目标实现重点

- 唯一入口是 `cmd/gateway`（数据面）；本仓库不带控制平面，业务数据走 SQL 直接管理。
- catalog、endpoint、API key、订阅、quota policy 等都是 SQL schema 表；gateway 启动期跑 `infra.Migrate` 建表 + `repo.CheckSchema` 防御性校验。
- gateway 依赖 Redis 执行 M6 限流、scheduler cooldown。
- SQL 写入 → gateway 数据传播走 [repo 进程内 TTL LRU 缓存](./06-pluggable-infra.md#8-repo-缓存deployer-sql--gateway-数据传播)
  （默认 30s），不直连同库每请求查表；data plane 是 100% 只读，TTL 足够。
- 客户端入口覆盖 OpenAI Chat、Anthropic Messages、OpenAI Responses、Images、Audio、Embeddings 路由；Gemini 当前作为上游协议支持，不暴露 Gemini 客户端入口。
- `pkg/protocol` 整包既是 Handler facade，也持有 vendor Factory / Session（HTTP 层工厂）+ endpoint-level quirks DSL；协议 shape 转换在 `pkg/translator`，usage 提取在 `pkg/usage`。消费侧只看 `protocol.Handler` / `protocol.Lookup`，不 type-assert Factory。
- 所有 middleware 装配走 interface-Option pattern（对位 otelgin v0.68.0），详见
  [06 §6](./06-pluggable-infra.md#6-middleware-options) 与 [01 §10](./01-request-pipeline.md#10-middleware-装配契约otelgin-v0680-对齐)。

## 维护约定

- 修改 `pkg/domain.RequestContext`、middleware 顺序、adapter/translator 接口、schema 或配置项时，同步更新本目录。
- 示例代码只说明关键契约，不要求逐字复制实现；字段名、组件边界、错误语义必须准确。
- 不要把未实现的远期蓝图写成已实现能力；未来功能按本目录目标边界逐步实现。
- 新接 repo cached wrapper / 新加 middleware Option / 新加 metric 时按 06 / 07 / 08 的对应小节同步登记。

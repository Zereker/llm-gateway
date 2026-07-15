[English](CHANGELOG.md) | [简体中文](CHANGELOG.zh-CN.md)

# 变更日志

本文记录用户可见的变更。项目从首个正式 Tag 开始遵循语义化版本。

## [Unreleased]

## [0.1.0] - 2026-07-15

首次公开发布。

### 新增

- OpenAI 兼容的 Chat Completions、Responses、Embeddings API，以及 Anthropic
  兼容的 Messages API。
- 多供应商上游、协议转换、Endpoint 级 Quirks、能力过滤、weighted/P2C 选择、
  冷却、重试和显式跨模型 fallback。
- 基于规则的 Virtual Model，包括确定性约束、延迟/成本目标、dry-run 评估和
  可解释路由决策。
- API Key 认证、账号订阅、分层配额、可插拔策略执行、请求脱敏、明确的流式响应
  模式、安全审计元数据和内容日志。
- 位于 `/api/v1` 的独立版本化 Console 控制面，包括 Web UI、Admin/Viewer 角色、
  Endpoint/Key/Policy/Pricing 管理和写操作审计。
- MySQL Schema 生命周期、Redis 运行时状态、File/Kafka 用量事件、Prometheus
  指标、OTel/slog Trace、Helm 部署资产、一条命令 Quickstart 与可复现性能基准。
- 带校验和的 Release 压缩包、版本化 Gateway/Console 容器镜像，以及内嵌的
  `-version` 构建信息。

### 兼容性边界

- `v0.1.0` 建立首个公开 HTTP、配置、Schema、用量事件、指标和 reason code
  契约，详见[公共契约](architecture/11-public-contracts.zh-CN.md)。
- 采用 `v0.1.0` 前必须重建 pre-release 数据库。从本版本开始，已合入的
  Migration 文件不可修改；后续 Schema 演进只能新增编号 Migration。

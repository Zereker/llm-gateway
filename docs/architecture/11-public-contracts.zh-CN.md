[English](11-public-contracts.md) | [简体中文](11-public-contracts.zh-CN.md)

# 公共契约

本文定义运维人员、API 客户端和下游消费者可以依赖的接口。内部 Go
package 名称不属于公共契约。

## HTTP API 版本

数据平面在 `/v1` 下提供兼容接口：

- `POST /v1/chat/completions`
- `POST /v1/responses`
- `POST /v1/embeddings`
- `POST /v1/messages`，使用 Anthropic 兼容请求结构

控制平面的管理接口统一位于 `/api/v1`。鉴权角色不进入 URL：`/api/v1`
是版本边界，Bearer 鉴权决定调用者是 Admin 还是 Viewer。Console UI `/`
和运维探针 `/healthz` 不是 API 资源。不提供未版本化的 `/admin/*` 管理接口。

## 控制平面输入契约

Mutation 请求必须使用 `Content-Type: application/json`，允许
`charset=utf-8` 等参数。请求体必须：

- 只包含一个 JSON 值；
- 不超过 1 MiB；
- 只使用目标请求类型声明的字段；
- 在格式错误、超限、存在尾随值或未知字段时返回 `invalid_json`。

不支持的媒体类型返回 HTTP 415 和 `unsupported_media_type`。文档声明的
query 字段同样严格：错误的 ID、布尔值或 limit 返回 `invalid_argument`，
不会被静默忽略或改变查询含义。审计列表 limit 范围为 1 到 1000。

## 错误结构

数据平面错误使用兼容网关结构，并包含稳定机器码、错误类别、request ID
和 trace ID：

```json
{
  "error": {
    "code": "model_not_found",
    "message": "model not found: example",
    "class": "invalid",
    "request_id": "req_...",
    "trace_id": "..."
  }
}
```

控制平面统一使用更小的稳定结构：

```json
{
  "error": {
    "code": "endpoint_invalid",
    "message": "endpoint failed validation",
    "details": {"reasons": ["invalid_url"]}
  }
}
```

客户端只能依据 `error.code` 分支，不能依赖面向人的 message。结构化诊断
放在 `error.details` 下；handler 不得向 error 对象添加临时同级字段。
其中不得出现密钥或原始 Prompt 内容。

## 配置契约

Gateway 和 Console 的 YAML decoder 都拒绝未知字段。已经移除或拼写错误的
配置会阻止启动，不会被静默忽略。环境变量仅覆盖文档声明的密钥和连接信息，
不用于重新定义行为策略。

以前为空的 Gateway `paths` 节点不属于契约。文件目标由对应功能自己持有，
例如 `usage_events.file.path` 和 `content_log.file.path`。

## Schema、事件、指标与 reason code

- MySQL schema 历史从 `internal/infra/migrations/000001_base.sql` 开始。
  已合并的 migration 文件不可修改；演进只能新增编号文件。
- Usage 记录携带 `schema_version: usage.v1`。破坏性事件变更必须使用新的
  schema version 和新的下游 topic/文件契约。
- Prometheus 名称和有界 label 集合属于机器契约，并与随项目发布的 Dashboard
  和告警规则一起检查。
- 路由 reason code 是有界且不含敏感信息的值。Header 选择的具体模型回退
  使用候选来源 `fallback_header` 和 reason `fallback_accepted`；即使调用者为
  虚拟模型发送了会被忽略的回退 header，路由决策也保留策略自身的 reason。

## 变更策略

首次发布前，直接删除过时的预发布接口，不保留别名。首次 tag 发布后，
破坏性的 HTTP 或事件变更必须启用新版本；schema 变更必须新增不可变 migration。
每项公共契约变更都必须包含测试以及同步的中英文文档。

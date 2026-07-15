[English](08-observability.md) | [简体中文](08-observability.zh-CN.md)

# 08.可观察性

本文档定义了网关日志、指标、跟踪、使用事件和内容日志的可观察性契约。

可观测性数据分为三类：用于故障排除和调度反馈的运行时指标；计费事实的使用事件；用于审核和重播的内容日志。三者在物理上是独立的，仅通过 `request_id` / `trace_id` 关联。

## 1. 常用字段

跨请求路径的所有日志、跨度、指标事件和异步事件应尽可能包含以下内容：

|领域|描述 |
|------|------|
| `request_id` |网关请求ID；来自标头或由 M1 | 生成
| `trace_id` / `span_id` |追踪上下文 |
| `account_id` |主账户/计费主题 |
| `sub_account_id` |子账户/运营商 |
| `api_key_id` |凭证ID；原始 API 密钥从未被记录 |
| `service_id` |模型服务 ID |
| `model` |请求的型号 |
| `routed_model` |实际路由模型；在跨模型回退方面与 `model` 不同
| `vendor` |端点供应商|
| `endpoint_id` |实际终点|
| `protocol` |客户端协议|
| `endpoint_protocol` |端点上游协议|
| `error_code` |稳定的机器码 |
| `error_class` |行为分类|
| `attempt_role` | `primary` 或 `fallback`，用于跨模型回退统计信息 |

高基数字段一定不能随便变成 Prometheus 标签。默认情况下，`request_id`、`trace_id` 和 `api_key_id` 仅进入日志、跟踪和异步事件，而不进入常规指标标签。

## 2. 日志

日志使用结构化 JSON。请求路径上的代码必须调用 `slog.InfoContext` / `WarnContext` / `ErrorContext`；禁止直接调用无上下文 `slog.Info` / `Error` / `Warn` 。

推荐活动：

|活动 |级别 |描述 |
|-------|-------|------|
| `request.start` |调试|要求进入；默认情况下在生产中禁用 |
| `request.end` |信息 |请求已完成；包括状态、延迟、用量摘要 |
| `auth.reject` |警告| API密钥/订阅/模型服务拒绝|
| `ratelimit.reject` |警告|客户端 RPM/RPS 拒绝 |
| `schedule.decision` |调试|端点选择结果及排除原因 |
| `upstream.error` |警告/错误 |上行呼叫失败|
| `usage.publish.error` |错误|使用事件发布失败 |
| `content_log.drop` |警告|由于背压而导致内容日志丢失 |

日志不得记录 API 密钥、解密的端点机密或完整的请求/响应正文。当需要记录内容时，请使用内容日志通道。

<a id="3-metrics"></a>
## 3. 指标

指标名称使用 `llm_gateway_` 前缀。直方图存储桶是实现配置的，默认情况下应涵盖毫秒到分钟级别的请求。

|指标|类型 |标签|描述 |
|------|------|--------|------|
| `llm_gateway_http_requests_total` |counter | `method`、 `route`、 `status`、 `error_class` | HTTP 请求总数 |
| `llm_gateway_http_request_duration_seconds` |直方图| `method`、`route`、`status`、`model`、 `routed_model` |网关端到端延迟|
| `llm_gateway_response_ttft_seconds` |直方图| `model`, `routed_model`, `vendor` |用于流响应的第一个客户端可见响应块的时间
| `llm_gateway_invoker_requests_total` |counter | `vendor`、`endpoint_id`、`model`、`endpoint_protocol`、 `result`, `error_class` |上游请求总数 |
| `llm_gateway_invoker_duration_seconds` |直方图| `vendor`、 `endpoint_id`、 `model`、 `result`、 `error_class` |上行呼叫延迟|
| `llm_gateway_selector_attempts_total` |counter | `model`、`routed_model`、`vendor`、`endpoint_id`、 `attempt_role`, `result`, `error_class` |端点尝试统计 |
| `llm_gateway_selector_candidates` |直方图| `model`, `stage` |候选项数；阶段是列表/资格/冷却/配额|
| `llm_gateway_selector_endpoint_selected_total` |counter | `endpoint_id`, `vendor`, `model` |端点选择 |
| `llm_gateway_selector_endpoint_call_total` |counter | `endpoint_id`, `vendor`, `model`, `outcome`, `class` |端点尝试结果 |
| `llm_gateway_selector_cooldown_enter_total` |counter | `endpoint_id`, `vendor`, `class` |成功过渡到冷却|
| `llm_gateway_scheduling_duration_seconds` |直方图| `model`, `attempts` |调度过滤/挑选/报告的总时间|
| `llm_gateway_eligibility_duration_seconds` |直方图| `model` |资格筛选时间|
| `llm_gateway_ratelimit_decisions_total` |counter | `scope`, `dimension`, `result` |客户端和端点速率限制决策|
| `llm_gateway_ratelimit_charge_total` |counter | `dimension`, `result` | TPM 充电后结果 |
| `llm_gateway_ratelimit_fail_open_total` | counter | `scope`, `dimension` | 显式 fail-open 事件 |
| `llm_gateway_tpm_overflow_total` |counter | `layer`, `dimension` | TPM 后充电后超出配置上限的次数 |
| `llm_gateway_policy_cache_requests_total` |counter | `layer`, `result` |配额策略缓存命中/未命中/错误 |
| `llm_gateway_policy_decisions_total` |counter | `stage`, `action` |低基数输入/输出策略决策；规则和策略 ID 仅保留在审核中 |
| `llm_gateway_policy_enforcement_total` |counter | `stage`, `action`, `result` |执行结果（`allowed`、`denied`、`applied`、`failed`）内容或策略 ID |
| `llm_gateway_usage_tokens_total` |counter | `model`, `routed_model`, `vendor`, `direction` |代币使用； `direction` 是 `input` / `output` （不是 `total`，以避免总和加倍）； `source` / `confidence` 不是指标标签 - 保存在使用事件和日志中以控制基数 |
| `llm_gateway_usage_publish_total` |counter | `backend`, `result` |使用事件发布结果 |
| `llm_gateway_content_log_publish_total` |counter | `backend`, `result`, `sampled` |内容日志发布结果 |
| `llm_gateway_outbox_buffer_size` |仪表 | `backend` |当前异步Outbox缓冲区 |
| `llm_gateway_outbox_publish_duration_seconds` |直方图| `driver`, `result` |Outbox发布延迟 |
| `llm_gateway_outbox_dropped_total` |counter | `driver`, `reason` |丢弃的Outbox事件计数 |
| `llm_gateway_outbox_dlq_total` |counter | `driver`, `result` | DLQ写入结果|
| `llm_gateway_endpoint_misconfigured_total` |counter | `vendor`, `reason` |启动时端点配置完整性检查|
| `llm_gateway_request_aborted_by_shutdown_total` |counter | `route` |请求因关闭超时而中断 |
| `llm_gateway_repo_cache_total` |counter | `table`, `result` |回购 TTL LRU 命中统计数据； `result` ∈ `hit\|miss\|error`; `table` ∈ `api_keys\|model_services\|endpoints_list\|endpoints_id\|quota_policies\|subscriptions` |
| `llm_gateway_repo_sql_load_total` |counter | `table`, `result` | repo直接SQL查询统计信息； `result` ∈ `ok\|error\|not_found` |

存储库缓存指标实现：`internal/repo/cache_metrics.go`（指标接口）+ `internal/app/gateway/repo_metrics.go`（舞会计数器适配器）。

当指标用于运行时评分时，Scheduler 不会直接读取 Prometheus；它应该阅读 `EndpointStatsStore` 中的 EMA / 滑动窗口摘要。 Metrics 是可观察层； `EndpointStatsStore` 是内部调度程序状态。

<a id="4-tracing"></a>
## 4. 追踪

OTel属性优先选择`gen_ai.*`和HTTP semconv标准；当标准字段缺失时，使用
`llm_gateway.*`。

LLM相关属性：

|语义 |属性 |
|------|-----------|
|操作| `gen_ai.operation.name` |
|请求的型号 | `gen_ai.request.model` |
|实际路由模型| `gen_ai.response.model` |
|供应商 | `gen_ai.system` |
|输入标记| `gen_ai.usage.input_tokens` |
|输出标记 | `gen_ai.usage.output_tokens` |
|总代币 | `gen_ai.usage.total_tokens` |
| TTFT（流媒体）| `gen_ai.response.ttft_ms` |
|端点 ID | `llm_gateway.endpoint.id` |
|账户 ID | `llm_gateway.account.id` |
|子账户 ID | `llm_gateway.sub_account.id` |
| API 密钥 ID | `llm_gateway.api_key.id` |
|服务编号 | `llm_gateway.service.id` |
|请求 ID | `llm_gateway.request_id` |
|持续时间（毫秒）| `llm_gateway.duration_ms` |
|错误代码 | `llm_gateway.error.code` |
|错误类别 | `llm_gateway.error.class` |
|调度程序尝试| `llm_gateway.scheduler.attempt` |

HTTP semconv 属性（由 M1 根跨度自动写入，与 otelgin v0.68.0 对齐）：

|语义 |属性 |笔记|
|------|-----------|------|
| HTTP 方法 | `http.request.method` | `GET` / `POST` 等 |
|路由模板| `http.route` | `c.FullPath()`，不是实际的URL，以避免高基数|
|状态码| `http.response.status_code` |由 M1 在 `c.Next()` 之后撰写 |
|网址方案| `url.scheme` | `http` / `https` |
| 客户端 IP | `client.address` | gin `c.ClientIP()` |
|用户代理 | `user_agent.original` | |

跨度行为：

- M1根跨度名称由`SpanNameFormatter(c)`决定，默认为`{METHOD} {route}`（与
  奥特尔金）；当路由不匹配时，它会回退到 `{METHOD}`。可以通过注入自定义函数
  `WithSpanNameFormatter`（例如，按模态重写）。
- M1根跨度种类= `SpanKindServer`；通过 `WithTraceContextPropagators` 注入的传播器
  提取上游traceparent。
- 错误状态：`c.Next()`之后，M1检查`rc.Error`；如果非空，则调用 `span.SetStatus(codes.Error, ...)`
  并写入 `llm_gateway.error.{code,class}`； HTTP 5xx 也会设置 Error； 4xx 保持未设置状态
  （与 otelgin / HTTP semconv 一致）。
- `gin.Context.Errors` 通过 `span.RecordError` 同步到跨度事件。

建议跨度结构：

```text
gateway.request                     (M1, span name = "POST /v1/chat/completions")
  auth.lookup                        (M2)
  envelope.parse                     (M3)
  budget.check                       (M4, optional)
  catalog.resolve                    (M5)
  moderation.check                   (M8, optional)
  ratelimit.reserve                  (M6 pre-side)
  dispatch.request                   (M7, internal/dispatch)
    dispatch.attempt                 (per attempt; attrs: model / endpoint / verdict)
      upstream.call                  (internal/invoker)
      usage.extract                  (inside translator)
    dispatch.fallback (event)        (triggered by Switch action)
  ratelimit.charge_tpm               (M6 post-side, runs after c.Next())
  tracing.commit                     (M10)
    usage.publish
    content_log.publish
```

`dispatch.request` 是调度程序的根跨度；属性包括 `dispatch.model` /
`dispatch.group` / `dispatch.attempt_cap` / `dispatch.outcome` /
`dispatch.routed_model` / `dispatch.attempts` / `dispatch.http_code`。
每次尝试都会打开一个 `dispatch.attempt` 子跨度，其属性包括 `attempt.model` /
`attempt.index` / `attempt.candidates` / `attempt.eligible` /
`endpoint.id` / `endpoint.vendor` / `endpoint.protocol` /
`verdict.stage` / `verdict.class` / `verdict.http_code` / `verdict.reason`。
回退模型切换通过 `dispatch.request` 上的 `dispatch.fallback` 事件进行标记
（有效负载：`{from, to}`）。

上面列出的跨度是 `gateway.request`（兄弟）下按时间顺序排列的子跨度集；没有强制要求嵌套层次结构 - 实现可以根据实际范围决定哪些跨度是父/子跨度。例如，如果 `usage.extract` 实际上包裹在 `upstream.call` 内，请将其设为子跨度，但不要仅仅为了“看起来整洁”而人为地嵌套跨度。 `ratelimit.charge_tpm` / `usage.publish` 发生在 `c.Next()` （M6 后侧 / M10）之后，并且应该是 的直接子代`gateway.request`，不是 `upstream.call` 的子级。

在流式响应中，`upstream.call`跨度涵盖从第一个块到最后一个块； TTFT 记录为 `gen_ai.response.ttft_ms` 属性。

M8 通过共享的 `AuditTracer` 将每个经过验证的策略决策写入为
结构化 `policy_decision` 事件。它在本地缓冲决策并刷新
在 `c.Next()` 返回后，它们处于 M8 的后处理器阶段。其有效载荷为
`policy.AuditRecord`；原始内容、突变替换字节和引擎错误
原因在结构上不存在。该记录包含发动机动作和
网关执行结果。 M10 不解释或转发策略状态。

## 5. 使用事件

使用事件是计费事实通道，不承载大型实体。默认 Kafka 主题：

```text
billing.usage.recorded.v1
```

DLQ主题：

```text
billing.usage.recorded.v1.dlq
```

主题由 **domain.entity.event.version** 命名，与生产服务名称解耦 - 下游计费/对账/配额消费者按业务域订阅，并且产生使用事件的多个服务仍然写入同一主题（请参阅[05-metering-billing §5](./05-metering-billing.zh-CN.md#5-usage-outbox)）。

分区键为`account_id`；丢失时，它会回落到 `request_id`。有效负载使用 JSON 请求信封：

```json
{
  "schema_version": "usage.v1",
  "event_id": "01J...",
  "usage": {
    "input": 128,
    "output": 256,
    "total": 384,
    "raw": {},
    "source": "upstream",
    "estimator": "",
    "confidence": "exact",
    "meta": {
      "account_id":          "acct_abc",
      "sub_account_id":      "sub_001",
      "api_key_id":          "ak_xxx",
      "model":               "gpt-4o",
      "vendor":              "openai",
      "endpoint_id":         "12345",
      "service_id":          "svc_gpt4o",
      "model_service_id":    12345,
      "service_update_time": "2026-04-18T09:00:00Z",
      "request_id":          "req_...",
      "trace_id":            "4bf9...",
      "start_time":          "2026-05-16T09:59:57Z",
      "end_time":            "2026-05-16T10:00:00Z",
      "ttft_ms":             320,
      "total_latency":       2800
    }
  },
  "created_at": "2026-05-16T10:00:00Z"
}
```

`request_id` / `trace_id` 仅出现在 `usage.meta` 内部 - 在请求信封顶层不重复（以避免双写不一致）。 `created_at` 是Outbox将事件排入队列的时间；实际请求时间由 `usage.meta.start_time` / `usage.meta.end_time` 给出。破坏架构更改最好应该移至新主题，例如`billing.usage.recorded.v2`，正在经历双写入和消费者切换。主题名称中的 `.v1` 是主题级物理隔离，与请求信封的 `schema_version` 是独立的机制。

## 6. 内容日志

内容日志记录请求/响应内容，其中可能包含 PII，并且必须与使用事件分开保存。默认禁用。

仅支持 `none` / `file` 输出后端。网关故意**不**嵌入 Kafka / S3 / Loki 等的生产者。内容日志是一个日志记录/审计通道，下游通常有多个接收器（S3 归档 + Loki 搜索 + Kafka 内容安全后审查 + 训练数据反馈）；让网关本身处理多接收器传送会将所有这些下游可用性耦合到主请求路径。正确的结构：

```text
gateway ──→ content.jsonl ──→ fluent-bit / vector ──┬─→ S3 / OSS
                                                    ├─→ Loki / ES
                                                    ├─→ Kafka topic (content safety post-review)
                                                    └─→ ...
```

文件轮换/压缩/清理由外部 logrotate 或日志收集器处理（Fluent-bit 的尾部输入支持索引节点跟踪），而不是在网关进程内部处理。添加/调整接收器只需要更改 Fluent-bit 配置，无需发布网关。有关详细信息，请参阅 [05-计量-计费§2](./05-metering-billing.zh-CN.md#2-content-log)。

完整的驱动程序和采样/反压配置模式位于[07-configuration §2 `content_log`](./07-configuration.zh-CN.md#2-gatewayyaml)。本节仅列出对可观察性有意义的字段：

|配置|默认|描述 |
|------|------|------|
| `driver` | `none` | `none` 完全禁用它； `file` 写入本地 JSONL |
| `sample_rate` | `1.0` |采样比例|
| `backpressure` | `drop_oldest` |缓冲区满时的策略；可以是 `drop_newest` / `block`； `block` 需要设置 `block_timeout` |
| `buffer_size` | `1024` |异步队列大小 |

内容日志事件必须携带 `request_id` / `trace_id` / `account_id` / `api_key_id` / `model` / `endpoint_id`，但不需要与使用事件同步发布。内容日志故障不会影响业务响应，也不会影响使用事件。

<a id="7-error-response"></a>
## 7. 错误响应

所有中止退出都使用统一的响应：

```json
{
  "error": {
    "code": "ratelimit.exceeded",
    "message": "rate limit exceeded",
    "class": "capacity",
    "details": {},
    "request_id": "req_...",
    "trace_id": "4bf9..."
  }
}
```

`code`是稳定的机器码； `class`为内部行为分类； HTTP 状态是协议响应，不能替代 `class`。`details` 可能仅包含安全诊断字段 — 没有正文、机密或原始敏感上游信息。

## 8. 警报建议

推荐的第一版警报：

- `5xx` 率持续上升。
- `upstream.error_class=transient|capacity`持续上涨。
- 非零 `usage.publish.result=failed`。
- `content_log.drop`持续上涨。
- Redis / DB 健康检查失败。
- 在较长一段时间内将符合资格的候选项安排为 0。

警报基于指标；故障排除跳转到跟踪/日志；计费调节使用使用事件。

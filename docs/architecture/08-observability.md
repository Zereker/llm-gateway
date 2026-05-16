# 08. Observability

本文件定义 gateway 的日志、指标、trace、Usage Event 和 Content Log 的观测契约。

观测数据分三类：运行指标用于排障和调度反馈；Usage Event 用于计费事实；Content Log 用于审计和回放。三类数据物理独立，只通过 `request_id` / `trace_id` 关联。

## 1. 公共字段

所有请求路径的日志、span、metric event 和异步事件都应尽量携带：

| 字段 | 说明 |
|------|------|
| `request_id` | 网关请求 ID；来自 header 或 M1 生成 |
| `trace_id` / `span_id` | tracing 上下文 |
| `account_id` | 主账号 / 计费主体 |
| `sub_account_id` | 子账号 / 操作者 |
| `api_key_id` | 凭证 ID，不记录 API key 原文 |
| `service_id` | model service ID |
| `model` | 请求模型 |
| `routed_model` | 实际路由模型；跨 model fallback 时不同于 `model` |
| `vendor` | endpoint vendor |
| `endpoint_id` | 实际 endpoint |
| `protocol` | client protocol |
| `native_protocol` | upstream protocol |
| `error_code` | 稳定机器码 |
| `error_class` | 行为分类 |
| `attempt_role` | `primary` 或 `fallback`，用于统计跨 model fallback |

高基数字段不能随意进入 Prometheus label。`request_id`、`trace_id`、`api_key_id` 默认只进入日志、trace 和异步事件，不作为常规 metric label。

## 2. 日志

日志使用结构化 JSON。请求路径必须调用 `slog.InfoContext` / `WarnContext` / `ErrorContext`，禁止直接调用不带 context 的 `slog.Info` / `Error` / `Warn`。

推荐事件：

| event | level | 说明 |
|-------|-------|------|
| `request.start` | debug | 请求进入，默认生产可关闭 |
| `request.end` | info | 请求完成，包含 status、latency、usage summary |
| `auth.reject` | warn | API key / subscription / model service 拒绝 |
| `ratelimit.reject` | warn | 用户侧 RPM/RPS 拒绝 |
| `schedule.decision` | debug | endpoint 选择结果和剔除原因 |
| `upstream.error` | warn/error | 上游调用失败 |
| `usage.publish.error` | error | usage event 发布失败 |
| `content_log.drop` | warn | content log 因 backpressure 被丢弃 |

日志不得记录 API key、解密后的 endpoint secret、完整 request/response body。需要记录内容时走 Content Log 通道。

## 3. Metrics

指标命名使用 `llm_gateway_` 前缀。Histogram bucket 由实现配置，默认应覆盖毫秒级到分钟级请求。

| 指标 | 类型 | Labels | 说明 |
|------|------|--------|------|
| `llm_gateway_http_requests_total` | counter | `method`, `route`, `status`, `error_class` | HTTP 请求总量 |
| `llm_gateway_http_request_duration_seconds` | histogram | `method`, `route`, `status`, `model`, `routed_model` | 网关端到端延迟 |
| `llm_gateway_upstream_requests_total` | counter | `vendor`, `endpoint_id`, `model`, `native_protocol`, `result`, `error_class` | 上游请求总量 |
| `llm_gateway_upstream_duration_seconds` | histogram | `vendor`, `endpoint_id`, `model`, `result`, `error_class` | 上游调用延迟 |
| `llm_gateway_scheduler_attempts_total` | counter | `model`, `routed_model`, `vendor`, `endpoint_id`, `attempt_role`, `result`, `error_class` | endpoint attempt 统计 |
| `llm_gateway_scheduler_candidates` | histogram | `model`, `stage` | 候选数量，stage 为 list/eligible/cooldown/quota |
| `llm_gateway_scheduling_duration_seconds` | histogram | `model`, `attempts` | 调度 filter / pick / report 总耗时 |
| `llm_gateway_eligibility_duration_seconds` | histogram | `model` | 资格过滤耗时 |
| `llm_gateway_ratelimit_decisions_total` | counter | `scope`, `dimension`, `result` | 用户侧和 endpoint 侧限流判断 |
| `llm_gateway_ratelimit_charge_total` | counter | `dimension`, `result` | TPM 后扣结果 |
| `llm_gateway_ratelimit_fail_open_total` | counter | `scope`, `dimension` | 显式 fail-open 次数 |
| `llm_gateway_tpm_overflow_total` | counter | `layer`, `dimension` | TPM 后扣后超过配置上限次数 |
| `llm_gateway_policy_cache_requests_total` | counter | `layer`, `result` | quota policy cache hit/miss/error |
| `llm_gateway_usage_tokens_total` | counter | `model`, `routed_model`, `vendor`, `direction` | token usage，`direction` 取 `input` / `output`（不再设 `total`，避免 sum 翻倍）；`source` / `confidence` 不进 metric label，保留在 Usage Event 和日志中以控基数 |
| `llm_gateway_usage_publish_total` | counter | `backend`, `result` | Usage Event 发布结果 |
| `llm_gateway_content_log_publish_total` | counter | `backend`, `result`, `sampled` | Content Log 发布结果 |
| `llm_gateway_outbox_buffer_size` | gauge | `backend` | 异步 outbox 当前 buffer |
| `llm_gateway_outbox_publish_duration_seconds` | histogram | `driver`, `result` | outbox 发布耗时 |
| `llm_gateway_outbox_dropped_total` | counter | `driver`, `reason` | outbox 丢弃事件数 |
| `llm_gateway_outbox_dlq_total` | counter | `driver`, `result` | DLQ 写入结果 |
| `llm_gateway_endpoint_misconfigured_total` | counter | `vendor`, `reason` | 启动期 endpoint 配置完整性检查 |
| `llm_gateway_request_aborted_by_shutdown_total` | counter | `route` | shutdown 超时中断请求 |

指标用于 Runtime Scoring 时，Scheduler 不直接读取 Prometheus；应读取 `EndpointStatsStore` 中的 EMA / 滑窗摘要。Metrics 是观测层，`EndpointStatsStore` 是调度内部状态。

## 4. Tracing

OTel attribute 优先使用 `gen_ai.*` 语义约定；没有标准字段时使用 `llm_gateway.*`。

基础 attributes：

| 语义 | Attribute |
|------|-----------|
| operation | `gen_ai.operation.name` |
| 请求模型 | `gen_ai.request.model` |
| 实际路由模型 | `gen_ai.response.model` |
| vendor | `gen_ai.system` |
| input tokens | `gen_ai.usage.input_tokens` |
| output tokens | `gen_ai.usage.output_tokens` |
| total tokens | `gen_ai.usage.total_tokens` |
| endpoint id | `llm_gateway.endpoint.id` |
| account id | `llm_gateway.account.id` |
| sub account id | `llm_gateway.sub_account.id` |
| api key id | `llm_gateway.api_key.id` |
| service id | `llm_gateway.service.id` |
| error code | `llm_gateway.error.code` |
| error class | `llm_gateway.error.class` |
| scheduler attempt | `llm_gateway.scheduler.attempt` |

建议 span 结构：

```text
gateway.request
  auth.lookup
  catalog.resolve
  ratelimit.reserve
  schedule.pick
  upstream.call
  usage.extract
  ratelimit.charge_tpm
  usage.publish
  content_log.publish
```

上列 span 名是 `gateway.request` 下按时间顺序的子 span 集合（兄弟关系），不强制嵌套层级；实现可按实际作用域决定哪些 span 互为父子。例如 `usage.extract` 实际包裹在 `upstream.call` 内时，把它作为子 span 即可，但不要为了"看起来整齐"人为嵌套。`ratelimit.charge_tpm` / `usage.publish` 发生在 `c.Next()` 之后（M6 post-side / M10），应是 `gateway.request` 直接子节点而非 `upstream.call` 的子节点。

Streaming 响应中，`upstream.call` span 覆盖首包到尾包；TTFT 记录为 span event 或 attribute。

## 5. Usage Event

Usage Event 是计费事实通道，不承载大 body。默认 Kafka topic：

```text
llm-gateway.usage
```

DLQ topic：

```text
llm-gateway.usage.dlq
```

partition key 使用 `account_id`；缺失时退化为 `request_id`。payload 使用 JSON envelope：

```json
{
  "schema_version": "usage.v1",
  "event_id": "01J...",
  "request_id": "req_...",
  "trace_id": "4bf9...",
  "usage": {
    "input": 128,
    "output": 256,
    "total": 384,
    "raw": {},
    "source": "upstream",
    "estimator": "",
    "confidence": "exact",
    "meta": {}
  },
  "created_at": "2026-05-16T10:00:00Z"
}
```

`created_at` 是 outbox 入队时间；请求发生时间以 `usage.meta.start_time` / `usage.meta.end_time` 为准。破坏性 schema 变更优先切新 topic，例如 `llm-gateway.usage.v2`，并经历双写和 consumer 切换。

## 6. Content Log

Content Log 记录 request / response 内容，可能包含 PII，必须与 Usage Event 分离。默认关闭。

默认 Kafka topic：

```text
llm-gateway.content
```

driver 与采样 / backpressure 完整配置 schema 见 [07-configuration §2 `content_log`](./07-configuration.md#2-gatewayyaml)。本节只列对观测有意义的语义字段：

| 配置 | 默认 | 说明 |
|------|------|------|
| `sample_rate` | `1.0` | 采样比例 |
| `backpressure` | `drop_oldest` | buffer 满时策略；可配 `drop_newest` / `block`，`block` 必须设 publish timeout |
| `buffer_size` | `1024` | 异步队列大小 |

Content Log 事件必须带 `request_id` / `trace_id` / `account_id` / `api_key_id` / `model` / `endpoint_id`，但不要求与 Usage Event 同步发布。Content Log 失败不影响业务响应，也不影响 Usage Event。

## 7. Error Response

所有 abort 出口使用统一响应：

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

`code` 是稳定机器码；`class` 是内部行为分类；HTTP status 是协议响应，不替代 `class`。`details` 只能包含安全诊断字段，不放 body、secret 或上游原始敏感信息。

## 8. 告警建议

第一版建议告警：

- `5xx` rate 持续升高。
- `upstream.error_class=transient|capacity` 持续升高。
- `usage.publish.result=failed` 非零。
- `content_log.drop` 持续升高。
- Redis / DB 健康检查失败。
- scheduler eligible candidates 长时间为 0。

告警基于 metrics，排障跳转到 trace/log，计费核对使用 Usage Event。

# 07 — Roadmap

本文定义开源版的分阶段路线图：从 v0.1 MVP 到 v1.0 生产就绪。每个阶段定义**包含什么 / 不含什么 / 验收标准**。

> **阅读前**：00-06 全部 ——本路线图是它们的实施切片。

## 1. 设计原则

| # | 原则 | 含义 |
|---|------|------|
| R1 | **接口先行** | 每个阶段先冻结接口，再写实现；后续阶段补默认实现，不改接口 |
| R2 | **零依赖可跑** | v0.1 起就能 `go run ./cmd/gateway` 起来跑通，不需要 Redis / Kafka / etcd |
| R3 | **能力增量** | 每个 minor 版本只加能力，不破坏 v0.1 的最小用例 |
| R4 | **基础设施抽象稳定** | [06] 的接口签名在 v0.5 后冻结；后续添加默认实现不改接口 |
| R5 | **演进规则纸面化** | 每份文档第"演进规则"章节就是 PR 检查清单 |

## 2. v0.1 — MVP（约 3-4 周）

**目标**：贡献者 clone 后 `go run` 即可跑通"OpenAI 客户端 → 单 Adapter → OpenAI 上游"的最小端到端流程。

### 包含

- 包结构骨架（按 [01] 第 1 节）
- `domain.RequestContext` 数据结构 + `middleware.GetRequestContext / TryGet / Attach` helper
- 10 个 middleware 全部空壳 + M1 / M2 / M3 / M5 / M9 / M10 完整实现（M1 trace、M2 APIKey 鉴权、M3 envelope 解析、M5 ModelService loader、M9 panic 兜底、M10 Tracing 落 metric）
- M5 ModelService：从单文件 YAML 加载（无 ConfigStore Watch）
- M7 Schedule：单 endpoint 直连，无 RetryExecutor / Filter 链（直接调 `adapter.Factory.NewSession` → BuildRequest → http.Do → Feed → Finalize）
- 1 个 Adapter：`pkg/adapter/openai/`（OpenAI 协议 → OpenAI 上游，identity translator）
- 默认基础设施（inline 到现有包，无新子包）：
  - `middleware.NewAPIKeyProvider(map[string]domain.UserIdentity)` — APIKey 文件 / 内存
  - `middleware.AlwaysPassGate{}` — 永远放行
  - `middleware.DefaultDetector + DefaultParser` — 按 URL 路径识别协议
  - `middleware.NewModelServiceProvider(configStore)` — config 文件 backed
  - `config.FileStore` — 单文件 YAML + fsnotify 热加载
  - `usage.FileOutbox` — JSONL append
  - `trace.SlogTracer` — stdlib slog
- **运维端点**（cmd/gateway 装配时配）：
  - `GET /healthz` — liveness（始终 200）
  - `GET /readyz` — readiness（registry 已注册 + ConfigStore 已加载）
  - `GET /metrics` — Prometheus scrape
- **请求保护**（M0 阶段或 gin 启动时配）：
  - 请求体大小限制（默认 10 MB，可配）
  - 网关级 timeout（默认 60s，可配；与 Adapter 上游 timeout 独立）
  - Graceful shutdown（SIGTERM / SIGINT → drain in-flight requests → 退出）
- `cmd/gateway/main.go` 完整可运行
- `docker-compose.yml` 提供（可选 Redis / Postgres，但默认全用本地实现）
- `examples/` 目录：curl 示例 + `apikeys.yaml` 模板 + `models.yaml` 模板

### 不含

- M4 Budget 实际逻辑（仅 AlwaysPass）
- M6 Limit 实际逻辑（接口存在但 `NoOpChecker`）
- M8 ContentModeration（nil）
- M7 RetryExecutor / 多 endpoint Filter 链
- 第二个 Adapter（Anthropic 等）
- Translator 实际实现（仅 identity）
- ParamSpec / Classifier 自定义
- Cooldown / HealthChecker
- PricingSpec / 计价聚合（仅 Usage 落本地日志）

### 验收

| # | 验收项 | 验证方法 |
|---|-------|---------|
| V1 | `go build ./...` 通过 | CI |
| V2 | `go test ./...` 通过（≥ 70% 覆盖率，针对 domain / middleware/M1/M2/M3/M5/M9 / adapter/openai） | CI |
| V3 | `go run ./cmd/gateway` 启动成功（默认配置） | 手测 |
| V4 | curl 一个 OpenAI 请求，得到上游响应 | 手测；上游可用 OpenAI 真账号或 vLLM |
| V5 | `tail -f /tmp/usage.log` 能看到一条 JSONL Usage 事件 | 手测 |
| V6 | 不带 Authorization 请求返回 401 | 手测 |
| V7 | panic 时返回 500 + log 含 stack | 单元测试 |
| V8 | `curl /healthz` 返回 200；`curl /readyz` 在 ConfigStore 未就绪时返回 503 | 手测 |
| V9 | `curl /metrics` 返回 Prometheus 格式（含 `ai_gateway.http.request_duration_ms` 等） | 手测 |
| V10 | 请求体超过限制 → 返回 413 Payload Too Large | 手测（`curl --data-binary @big.json`） |
| V11 | 请求超时 → 返回 504 Gateway Timeout（不阻塞、不泄漏 goroutine）| 集成测试 |
| V12 | SIGTERM 后 in-flight 请求完成、新请求拒绝、进程在 30s 内退出 | 手测 |

## 3. v0.5 — 完整能力（约 6-8 周）

**目标**：所有 middleware 和接口的实际实现都到位；多 Adapter；多 endpoint 调度 + 重试；三层限流；价格 + 计量管道。

### 包含

- M4 Budget 完整实现（`Checker` 接口）+ `inmemory` 默认实现
- M6 Limit 完整：`domain.LimitSpec` + `Checker` + 三层 AND + Lua 脚本（`cache/memory` 和 `cache/redis` 两套）
- M7 完整 RetryExecutor：CooldownManager + HealthChecker + Filter 链（`Cooldown / Group / Health / WeightedRandom`，**不含** PrefixCache / Busy）
- M8 ContentModeration 实现（默认 NoOp，可选 OpenAI moderation）
- 第二、第三个 Adapter：`anthropic`、`google_gemini`（含 Translator）
- Translator：`anthropic_to_openai`、`openai_to_anthropic`、`gemini_to_openai`、`openai_to_gemini`
- ParamSpec + Validator + 三种未知参数模式
- Classifier 接口与 OpenAI / Anthropic 自定义实现
- TokenExtractor 完整：`openai_compat / anthropic / google_gemini` 三个
- PricingSpec + 默认 Calculator（无 CEL）
- `model_service_spec_history` SQLite 实现 + 双写事务
- ConfigStore 多实现：`file` (fsnotify) / `etcd` / `sqlite`
- M10 Tracing：完整 metric 命名规约 + Prometheus exporter
- 配置示例：`examples/full-config/`（多模型 + 多 endpoint + 限流 + 调度 profile）

### 不含

- L3 跨模型 fallback（接口预留，不强制）
- PrefixCacheScheduler / BusyFilter（仅 SelfHosted；接口预留）
- CEL 表达式计价（接口预留）
- Kafka EventBus（接口存在；默认仍用 file）
- 流处理器 (Flink / Beam) 离线聚合（不在 Go 项目内；外部消费 file / Kafka）
- OpenTelemetry Tracer
- Image / TTS / Task / Embedding modality 完整支持（接口预留，仅 Chat 实现）

### 验收

| # | 验收项 | 验证方法 |
|---|-------|---------|
| V1 | 三个 Adapter 都能跑通 chat 请求 | 集成测试 |
| V2 | Anthropic 客户端 → OpenAI 上游（跨协议）正常 | 集成测试 |
| V3 | 限流三层全部生效（用户 / 模型 / endpoint）| 单元 + 集成 |
| V4 | 上游 5xx 触发 L1 retry + L2 fallback | 集成测试（fake upstream）|
| V5 | Cooldown 阈值后 endpoint 被隔离 | 单元测试 |
| V6 | Group 隔离：reserved 用户走 reserved endpoint | 集成测试 |
| V7 | 改 ConfigStore 中的限流配置秒级生效 | 集成测试（fsnotify / etcd Watch）|
| V8 | 价格变更后历史请求按旧价（验证 history 表查询）| 集成测试 |
| V9 | 全套 metric 在 `/metrics` 端点暴露 | 手测 + Grafana 看板示例 |
| V10 | 测试覆盖率 ≥ 80% | CI |

## 4. v1.0 — 生产就绪（约 10-12 周）

**目标**：生产可部署；高可用；可观测；可扩展。

### 包含

- L3 跨模型 fallback（开关默认 off）
- PrefixCacheScheduler（一致性哈希 + 主选/次选）
- BusyFilter（KV cache rate / queue depth；需要 Adapter 实现 `BusyMetricProvider`）
- CEL 表达式计价（`cel.Compile` 验证 + Calculator 内嵌 CEL）
- Kafka EventBus 实现 + 部署文档
- 流处理器示例：`examples/flink/` 或 `examples/beam/` 一份典型作业（Source → Dedup → Enrich → Price → Window → Sink）
- OpenTelemetry Tracer + 部署示例
- Image / Task modality 完整 Adapter 实现（至少一个示例：OpenAI Image / OpenAI Sora）
- HTTP/2 + 流式优化（buffer pool）
- 全套 Prometheus 告警规则示例（`examples/prometheus/alerts.yaml`）
- 多副本部署示例：`examples/k8s/` Helm chart
- 安全加固：API Key 加密存储、请求体大小限制、超时控制

### 不含

- 多协议代理（gRPC 入口 / WebSocket）
- 嵌入式管理 UI（`cmd/admin` 仅提供 REST API；UI 留给生态）
- 自带 Embedding / RAG 框架
- 多模型 ensemble / chain（每请求只路由一个 endpoint）

### 验收

| # | 验收项 | 验证方法 |
|---|-------|---------|
| V1 | 高并发压测（≥ 5000 QPS）持续 1 小时无内存泄漏 | benchmark |
| V2 | 所有 domain.ErrorClass 在 chaos 测试下行为符合契约 | chaos 测试（fake upstream 注入各种错） |
| V3 | Kafka EventBus + Flink 示例跑通 end-to-end 计价 | 集成测试 |
| V4 | OTel trace 能在 Jaeger 中看到完整 span 树 | 手测 |
| V5 | Helm install 后健康探针正常、metric 暴露 | k8s 集成测试 |
| V6 | 安全扫描（SAST + dep audit）无高危 | CI |
| V7 | 文档完整：00-07 + usage 指南 + provider 接入指南 + ops 指南 | review |

## 5. 不在路线图内

明确**不做**的事，避免设计膨胀：

- ❌ Prompt 工程框架（不是 LangChain）
- ❌ 模型推理服务（不是 vLLM / TGI）
- ❌ RAG / 向量检索（不是 LlamaIndex）
- ❌ 应用层业务逻辑（不是 BFF）
- ❌ 多模型 ensemble / chain（每请求路由一个 endpoint）
- ❌ Web UI（cmd/admin 仅 REST API）
- ❌ 自带 LLM evaluation / 质量打分

## 6. 版本号策略

| 版本段 | 含义 |
|-------|------|
| `0.x.y` | API 不稳定；minor 之间允许破坏性变更（对应 v0.1 / v0.5）|
| `1.0.0` | API 稳定；遵循 semver |
| `1.x.0` | 新功能 / 新 Adapter / 新基础设施实现；向后兼容 |
| `1.x.y` | bug fix / 安全更新 |
| `2.0.0` | 破坏性变更（接口签名 / 配置 schema 等）；前置 deprecation 周期 ≥ 一个 minor 版本 |

## 7. 文档版本与发布节奏

| 版本 | 文档动作 |
|------|---------|
| 任意 PR | 修改代码必须同步修改对应 architecture/ 文档 |
| minor release | 更新本文档第 2-4 节"包含 / 不含"清单与验收标准 |
| major release | review 全套 docs；过时章节标 `Deprecated` |

## 8. 出口标准（每阶段通用）

- ✅ 所有验收项通过
- ✅ CI 绿（lint + test + build）
- ✅ CHANGELOG 更新
- ✅ 文档与代码一致（手工 review）
- ✅ `examples/` 中的示例能运行
- ✅ 至少一个端到端 demo 录屏 / 截图

## 9. 演进规则

- **修改路线图**：本文档第 2-4 节同步；如改变 v0.1 / v0.5 / v1.0 边界需在 PR 描述说明取舍
- **新增大功能**：评估归入哪个版本；若未在路线图内需先讨论是否扩展路线图
- **删除功能**：触发版本号策略中的破坏性变更规则

# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## 项目概览

llm-gateway 是一个 Go 实现的 LLM 推理网关：对外提供 OpenAI / Anthropic 兼容协议，对下路由到多家上游（OpenAI、Anthropic、Gemini、vLLM 等）。架构与契约的唯一真源在 `docs/architecture/00-overview.md` ~ `07-roadmap.md`，**改主链路代码前先读对应章节**。

## 单进程

仓库只有 `cmd/gateway`（数据平面，:8080）：

- 启动期跑 `infra.Migrate` 建表 + `repo.CheckSchema` 防御性校验
- 处理 `/v1/*` 流量；middleware 链 M1-M10
- 业务数据（model_services / endpoints / api_keys / pricing / quota_policies /
  subscriptions / accounts）**直接 SQL 插入维护**——本仓库不带控制平面。
  repo 层用进程内 TTL LRU 缓存（默认 30s），SQL 改完 ≤ TTL 看到新值。

启动顺序：**docker stack → gateway**。`data_key`（endpoints.auth 列加密 KEK）
要跟 SQL 插入时用的加密一致。

## 常用命令

```sh
make stack              # 起 mysql + redis + redpanda 容器
make stack-clean        # 停容器并删数据卷（彻底重置）

make test               # 单元测试；SQL 测试在没设 MYSQL_DSN 时 skip
make test-integration   # 起 stack + 串行（-p 1）跑全测试，含 SQL / outbox

make build              # 编译 gateway + mockupstream 到 ./bin
make run-gateway        # 跑 gateway（启动期自跑 infra.Migrate）
make run-mockupstream   # 跑 mock 上游（调试用）

# 单测试用例（按包 / 按名称）
go test -run TestAuth ./pkg/middleware
MYSQL_DSN='root:@tcp(localhost:3306)/llm_gateway?parseTime=true&charset=utf8mb4' go test ./pkg/repo
```

`go test ./...` 是 CI 真相来源；Make 只是本地便捷。

## 关键架构概念

### Middleware 链（M1-M10）

请求流水线由 10 个 middleware 组成，**顺序在 `pkg/router/chat.go` 等模态文件里显式列出**，不要抽公共 helper。当前顺序：

```
M1 TraceContext → M10 Tracing → M9 Recover → M2 Auth   （pre-Envelope，挂在 group 上）
→ WithSourceProtocol（路径打标）→ M3 Envelope
→ M4 Budget → M5 ModelService → M6 Limit → M8 Moderation → M7 Schedule
```

M10 注册在 Recover **外层**但收尾逻辑跑在 post-`c.Next()`（洋葱返程）——所以
执行顺序上仍是"最后收尾"，但任何 abort（401/429/503）和已恢复的 panic 都逃不过
它的 metric / usage / 审计（挂链尾的旧版会被 abort 跳过）。

每个模态文件（`chat.go` / `image.go` / `audio.go` / `embedding.go`）**自己列**完整链。差异化预期会增加（如 chat 加 Moderator、image 加 multipart Parser），所以拒绝 DRY。

### RequestContext (P2)

跨 middleware 的请求级状态走 `*domain.RequestContext` typed struct，通过 `gin.Context.Set/Get` 传递。用 `middleware.GetRequestContext(c)` 取，**禁止**散落的 `c.Set("foo", ...)`。

### Protocol facade (P3 / P4)

- 端到端协议处理走 `pkg/protocol.Handler` facade；消费侧（dispatch / middleware / invoker）只看 `Handler` / `Lookup` 两个接口，**不直接接触** `Factory` / `Session` / `LookupFactory`。
- Handler 内部 = `Combine(Factory, translator.Translator)` + endpoint 级 `quirks.Rewriter`：
    - `pkg/protocol/<vendor>/`：vendor HTTP 层（URL / auth header / Content-Type）—— Factory + Session 实现，`init()` 调 `protocol.RegisterFactory("<vendor>", Factory{})`。
    - `pkg/translator/<src>_<dst>/`：协议 shape 转换（OpenAI ↔ Anthropic / OpenAI ↔ Gemini / identity 等），`init()` 调 `translator.Register(...)`。
    - `pkg/protocol/quirks`：endpoint 级 body + header 微调 DSL（存 `endpoints.quirks` JSON 列）；deployer 配置驱动，不在代码注册。
- 新增 vendor / translator：在子包 `init()` 里注册到 registry；`cmd/gateway/main.go` 用 blank import (`_ "..."`) 触发注册。**不要改主链路**。
- v0.7 起 `pkg/adapter` 已并入 `pkg/protocol`；老文档里的 `pkg/adapter/<vendor>/` 都是历史路径，现在在 `pkg/protocol/<vendor>/`。

### 客户端协议范围

Gateway 只暴露 OpenAI / Anthropic / OpenAI Responses 三种**客户端**入口。Gemini 仅作为**上游**支持——客户端用 OpenAI SDK 调，网关翻译到 Gemini。

### Pluggable infra (P5)

外部依赖全走接口：`BudgetGate` / `Moderator` / `Tracer` / `OutboxPublisher` / `ratelimit.Store` / `schedule.CooldownManager` / `repo.*Provider`。`cmd/gateway/main.go` 的 `build*` 函数是依赖注入装配点（按 `cfg.Driver` switch），**不认的 driver 一律 panic**（fail-fast 暴露配置错）。

### 错误分类 (P7)

`domain.ErrTransient / ErrRateLimit / ErrPermanent / ErrInvalid / ErrUnknown`。重试策略 + Cooldown 时长按类决定；新增错误处理时必须挂到这五类之一。

## 代码约定

- **路径前缀**：每条路由在自己的 `.POST` 调用里完整声明 `/v1/...`，**不**用 `engine.Group("/v1")`。读 chat.go 第一眼看到完整 URL。
- **`X-Gateway-*` header**：所有 gateway 自定义 header 用此前缀，与 vendor / 客户端 header 区分。客户端覆盖参数（timeout / max_attempts / fallback_models）只能比 cfg 默认**更严**，解析失败静默 fallback。
- **配置 driver 路径**：所有可插拔实现都在 yaml 里走 `driver:` 字段（`alwayspass` / `inmemory` / `slog` / `otel` / `file` / `kafka` / `none` / `openai` 等），cmd/*/main.go 的 `build*` 函数 switch 到具体实现。
- **Endpoint 凭证加密**：`endpoints.auth` 列用 AES-256-GCM 加密；KEK 走 `cfg.DataKey`（hex-encoded 32 字节）；deployer 用 SQL 插入加密密文时也要用同一个 KEK。

## 文档与需求

- 架构与接口契约：`docs/architecture/00-overview.md` ~ `07-roadmap.md`，PR 改主链路必须同步改对应文档。
- **需求文档**（技术方案 / 使用文档 / 测试文档 / 上线单）按用户全局规范统一写到 Obsidian `~/Documents/Obisdian/notebook/需求池/{需求名}/`，**不要**在仓库 `docs/` 下新建需求类文档。

## Git

- 提交信息**不**带 `Co-Authored-By` 行（用户全局规范）。
- 严禁 `git push --force` / `git reset --hard` / `git rebase` 等会改写远程历史的操作；修正已推送的提交只能用 `git revert` + 新 commit。

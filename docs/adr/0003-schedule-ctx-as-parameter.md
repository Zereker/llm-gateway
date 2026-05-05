# 0003. `Selection` 不持有 `context.Context`

* **Status**: Proposed
* **Date**: 2026-05-05
* **Author**: zhanghaojie.114

## Context

`pkg/schedule/scheduler.go:74` 的 `defaultSelection` 把 `context.Context` 存为 struct 字段：

```go
type defaultSelection struct {
    ctx            context.Context
    cfg            Config
    req            *Request
    allCands       []*domain.Endpoint
    epAttempts     map[int64]int
    attempts       int
    maxAttempts    int
    decisions      []Decision
    current        *domain.Endpoint
    pendingRetryEp *domain.Endpoint
    currentModel   string
    fallbacks      []string
}
```

`ctx` 在 `BeginSelection(ctx, req)` 时被记录，后续 `Pick()` / `Report()` / `advanceFallback()` 都用 `s.ctx`：

```go
// pkg/schedule/scheduler.go:158
cands, err := s.req.LoadFallback(s.ctx, next, s.req.Group)

// pkg/schedule/scheduler.go:204
_ = s.cfg.Cooldown.Mark(s.ctx, ep.ID, result.Class)
```

### 为什么这是个问题

1. **违反 Go stdlib 明确指引**：`context` 包文档第一句：
   > Do not store Contexts inside a struct type; instead, pass a Context explicitly to each function that needs it.
   理由是 ctx 跟生命周期绑定（cancel / deadline），存进 struct 后 ctx 的生命周期跟 struct 不一致就会 stale。

2. **M7 driver loop 实际跨多个生命周期**：
   - `BeginSelection` 时拍的是请求初始 ctx（来自 `c.Request.Context()`）。
   - middleware 链中 M6 / M8 / M7 内部可能给 `rc.Ctx` 做 `WithTimeout` / `WithCancel` 加修饰（看 `pkg/middleware/timeout.go:33` / `auth.go:51`）。
   - 客户端断开会 cancel 外层 ctx，但 `s.ctx` 是早期快照，**看不到**这个 cancel——`advanceFallback` 仍然会发起 `LoadFallback` 调用，做无用功且可能挂着上游连接。
   - 或者反过来：external code 给 ctx 注入 logger / span 后过来的请求，schedule 内部不感知后注入的修饰。

3. **跟 `pkg/upstream.Sender` 的形态不一致**：`Sender.Send(ctx, ep, env, body)` 已经是"参数 ctx"模式，新写的代码已经对齐 stdlib 约定。schedule 是老代码，跟新形态不一致。

4. **Selection 在测试中难以模拟"中途 cancel"场景**：要测"客户端断开后 driver loop 是否优雅停止"，得伪造一个 BeginSelection 之后才 cancel 的 ctx——当前形态下 schedule 收不到这个信号。

### 现有引用面

`Selection` 接口的方法签名：

```go
// pkg/schedule/types.go
type Selection interface {
    Pick() *domain.Endpoint
    Report(ep *domain.Endpoint, result Result)
    Decisions() []Decision
    Done()
}
```

外部调用方只有 `pkg/middleware/schedule.go` 的 driver loop。改动影响面小、可控。

## Options Considered

### Option A: `Pick(ctx)` / `Report(ctx, ep, result)` 接收参数

```go
type Selection interface {
    Pick(ctx context.Context) *domain.Endpoint
    Report(ctx context.Context, ep *domain.Endpoint, result Result)
    Decisions() []Decision
    Done()
}
```

`BeginSelection` 仍接 ctx（用于初次 Filter 链运行 / Cooldown 查询），但不 cache。`defaultSelection` 删 `ctx` 字段。

- **正面**：对齐 Go 约定；driver loop 自然把当次 ctx 传进去；测试可在不同 Pick 调用间用不同 ctx。
- **负面**：API 微破坏（Selection 接口方法签名变了，所有实现 + 调用方要改）；driver loop 多写 `c.Request.Context()` 一次 / 调用。

### Option B: `Selection` 改成无状态，把状态搬到 caller

`Scheduler` 暴露纯函数 `Pick(ctx, candidates, exclude, ...)`。

- **正面**：极纯粹的"无状态选路服务"；所有状态在 caller 端可见。
- **负面**：把 epAttempts / pendingRetryEp / fallback 状态机都暴露给 middleware；middleware 层会变复杂。已经在前面 ADR 思路里讨论过 schedule 是"选路决策器"，状态机封在它里面是合理的。

### Option C: 保持现状

存 ctx 不动。

- **正面**：0 工作量。
- **负面**：违反 stdlib 约定 + 跟 Sender 形态不一致 + 客户端 cancel 检测不到。技术债持续累积。

## Decision

**采纳 Option A**。

理由：
- 最小改动同时彻底解决问题。
- API 破坏面小（Selection 接口 + 一个 driver loop）。
- 跟 Sender 形态对齐。
- Option B 把状态机拆出去得不偿失，schedule 包当前的状态封装是合理的。

## Consequences

### Positive

- `defaultSelection.ctx` 字段删除，schedule 包不再持有 ctx。
- 客户端断开 → 外层 ctx cancel → 下次 `Pick(ctx)` / `Report(ctx)` 进来的就是 cancelled ctx → `LoadFallback` / `Cooldown.Mark` 立即返回错误 → driver loop 自然终止。
- Selection 测试可注入 cancellable ctx 模拟"中途断开"场景。
- 跟 `pkg/upstream.Sender` 形态完全一致，新人读代码不疑惑。

### Negative / Trade-offs

- `Selection` 接口签名变化是**破坏性 API change**——任何外部包实现 `Selection` 都要改。但当前仅 `defaultSelection` 一个实现，影响面极小。
- driver loop 每次调 Pick / Report 都要传 ctx（多 1 个参数）；可读性影响微小。

### Migration Path

**阶段 1：改接口与实现（单 commit）**
1. `pkg/schedule/types.go` 修改 `Selection` 接口：
   ```
   Pick(ctx context.Context) *domain.Endpoint
   Report(ctx context.Context, ep *domain.Endpoint, result Result)
   ```
   （`Decisions()` / `Done()` 不接 ctx，跟纯访问器一致。）
2. `pkg/schedule/scheduler.go`：
   - `defaultSelection` 删除 `ctx` 字段。
   - `Pick` / `Report` / `advanceFallback` 全部接 ctx 参数；`advanceFallback` 改成 `advanceFallback(ctx)`。
   - `BeginSelection(ctx, req)` 保留 ctx 入参，但只用于"初次构造 Selection"——构造完毕不存。
3. `pkg/middleware/schedule.go` driver loop 修改：
   ```
   for {
       ep := sel.Pick(rc.Ctx)
       ...
       sel.Report(rc.Ctx, ep, outcome.ToScheduleResult())
   }
   ```
4. `go test ./pkg/schedule/... ./pkg/middleware/... ./pkg/upstream/... ./pkg/router/...` 验证。

**回退**：单 commit 实现，发现回归直接 `git revert`。

### 风险评估

- **低风险**：Selection 接口当前只在仓库内部被实现 / 调用；没有外部 SDK 依赖它。
- **测试覆盖**：当前 `pkg/schedule/` 没专门 test 文件（看 `find` 结果）；改动后建议补 `scheduler_test.go` 覆盖：
  - BeginSelection → cancel ctx → Pick 立即返 nil
  - Pick 期间 ctx 取消 → Cooldown.Mark 收到 cancelled ctx
  - LoadFallback 跨 model 场景下 ctx 仍然透传

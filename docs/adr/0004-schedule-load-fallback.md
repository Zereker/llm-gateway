# 0004. `LoadFallback` 装配位置

* **Status**: Proposed
* **Date**: 2026-05-05
* **Author**: zhanghaojie.114

## Context

之前的重构（commit `aa6800d`）把候选 endpoint 拉取从 `schedule.Config.Candidates`（启动期注入）移到 `Request.Candidates`（per-request 传入）。L3 跨 model fallback 也跟着迁移：

```go
// pkg/schedule/types.go
type Request struct {
    ...
    Candidates []*domain.Endpoint
    LoadFallback func(ctx context.Context, model, group string) ([]*domain.Endpoint, error)
    FallbackModels []string
    ...
}
```

`LoadFallback` 是个 per-request 字段，但**实际**每个请求传的都是同一个值——`pkg/middleware/schedule.go:73`：

```go
req := &schedule.Request{
    ...
    Candidates:   cands,
    LoadFallback: deps.Endpoints.ListForModel,  // method value，每请求都一样
}
```

`deps.Endpoints` 是装配期注入的 `repo.EndpointReader`，整个 gateway 进程生命周期不变。每个请求都把同一个 method value 塞进 Request，是结构性冗余。

### 为什么这是个问题

1. **概念错位**：`Request` 应该承载"per-request 的真实输入"——model / group / TPMCost / PrefixKey 这些都是**每请求都不同**的数据。`LoadFallback` 是依赖（dependency），不是数据（data）。把依赖塞 Request 字段是 DI 反模式。

2. **缺乏隔离单测**：`schedule.New` 不接 LoadFallback，所以 schedule 包级别测试 advanceFallback 必须每个用例自己造 `Request{LoadFallback: ...}`。装配后 LoadFallback 应该在 `New(Config{...})` 时注入，跟 `Cooldown` / `Filters` 一样。

3. **跟 `Candidates` 不对称**：`Candidates` 必须 per-request（因为不同请求查不同 model 的候选）；`LoadFallback` 是 hot path 重复消费的依赖。两个字段同放 Request 模糊了"哪个是数据 / 哪个是依赖"。

4. **对未来 L4+ fallback 策略的限制**：如果将来要加"按 vendor 黑名单 fallback"或"基于 latency 的动态 fallback model"，这些策略需要更多依赖（latency reader / vendor blacklist 等）。继续往 Request 里塞依赖会让 Request 越来越胖，且每个请求重复传。

## Options Considered

### Option A: `LoadFallback` 移到 `schedule.Config`

```go
type Config struct {
    Filters      []Filter
    Cooldown     CooldownManager
    LoadFallback func(ctx context.Context, model, group string) ([]*domain.Endpoint, error)
    MaxAttempts  int
    MaxPerEndpoint int
}
```

`schedule.New(cfg)` 时注入；`defaultScheduler` 持有；`advanceFallback` 用 `s.cfg.LoadFallback`。`Request` 只留 `Candidates` + `FallbackModels`（model 名列表，仍是 per-request 数据）。

- **正面**：依赖 / 数据分离；schedule 包可独立单测 LoadFallback；middleware 装配代码减少一行。
- **负面**：API 微破坏（`schedule.Config` 加字段、`Request` 删字段）；nil LoadFallback 等价于"L3 关闭"语义要明文化。

### Option B: 把 `LoadFallback` 抽成接口

```go
type FallbackLoader interface {
    Load(ctx context.Context, model, group string) ([]*domain.Endpoint, error)
}

type Config struct {
    ...
    Fallback FallbackLoader  // nil = L3 关闭
}
```

`repo.EndpointReader` 自动满足该接口（method value 跟 interface signature 同形）。

- **正面**：接口比 func 更"OO"；多个 Fallback 策略可不同实现（如 cached / filtered）。
- **负面**：单方法 interface 在 Go 习惯里通常用 func type 替代（参考 `http.HandlerFunc`）；这条增加一层抽象但收益不明显。

### Option C: 保持现状（per-request 字段）

- **正面**：0 工作量。
- **负面**：DI 反模式；每请求重复传引用；测试难写。

### Option D: 让 `Scheduler` 接 `EndpointReader` 整个接口

```go
type Config struct {
    ...
    Endpoints repo.EndpointReader
}
```

`Scheduler` 知道 EndpointReader 全部方法（List / PickForModel / GetByID）。

- **正面**：一个依赖到位；未来加 L4 fallback 策略不用扩 Config。
- **负面**：schedule 包反向依赖 repo（违反 ADR 0001 的分层目标——schedule 不该知道 SQL）；EndpointReader 接口也太宽（schedule 只需要 ListForModel）。

## Decision

**采纳 Option A**（`LoadFallback` 移到 `schedule.Config`，func type 不抽 interface）。

理由：
- 解决"依赖塞数据"反模式，零成本。
- Func type 比单方法 interface 在 Go 里更习惯（参考 stdlib `http.HandlerFunc` / `sort.Slice`）。
- Option D 让 schedule 反向依赖 repo，跟 ADR 0001 的方向冲突。
- API 破坏面极小（仅 `schedule.Config` + `Request` + `pkg/middleware/schedule.go` 一处装配代码）。

### 设计细节

```go
// pkg/schedule/types.go
type Request struct {
    Model               string
    Group               string
    TPMCost             uint32
    MaxAttemptsOverride int
    Candidates          []*domain.Endpoint  // 主 model 候选
    FallbackModels      []string            // 仅 model 名列表（pure data）
    PrefixKey           []byte
    // 删除 LoadFallback 字段
}

// pkg/schedule/scheduler.go
type Config struct {
    Filters      []Filter
    Cooldown     CooldownManager
    LoadFallback func(ctx context.Context, model, group string) ([]*domain.Endpoint, error)  // 新增
    MaxAttempts  int
    MaxPerEndpoint int
}

func New(cfg Config) Scheduler {
    // LoadFallback 为 nil 等价于 L3 完全关闭（即便 Request.FallbackModels 非空也不切换）
    ...
}

// pkg/middleware/schedule.go 装配
deps.Scheduler 应在 cmd/gateway/main.go 装配时把 deps.Endpoints.ListForModel 传进去
```

## Consequences

### Positive

- `Request` 只承载 per-request 数据；schedule 的依赖 / 数据分离干净。
- `defaultSelection.advanceFallback` 用 `s.cfg.LoadFallback`，不再访问 `s.req.LoadFallback`——Selection 的数据访问局限在"候选 + attempts"，更清晰。
- 单测 schedule 时构造 Config 一次性配齐依赖，不需要每个 Request 重复填。

### Negative / Trade-offs

- `cmd/gateway/main.go` 的 `schedule.New` 装配代码多一行 `LoadFallback: repo.NewSQLEndpointReader(sqldb).ListForModel`——但同时 middleware 装配少一行（不再需要 `Endpoints` 字段供 LoadFallback）。
- 真要在不同请求用不同 `LoadFallback` 实例（极端边缘场景，例如多租户拿不同 endpoint reader）—— 当前装配不支持。但这种场景很少；真出现可加 `Request.LoadFallbackOverride` 兜底。

### Migration Path

**阶段 1：单 commit 完成**

1. `pkg/schedule/scheduler.go`：
   - `Config` 加 `LoadFallback` 字段。
   - `defaultScheduler` 持有 `cfg`（已经持有）。
   - `defaultSelection.advanceFallback` 改用 `s.cfg.LoadFallback`，删除对 `s.req.LoadFallback` 的引用。
   - `BeginSelection` 不再需要从 `req` 拿 LoadFallback；逻辑保持不变。
2. `pkg/schedule/types.go`：从 `Request` 删除 `LoadFallback` 字段；docstring 同步更新。
3. `cmd/gateway/main.go` 装配：
   ```
   Scheduler: schedule.New(schedule.Config{
       Filters: ...,
       Cooldown: ...,
       LoadFallback: repo.NewSQLEndpointReader(sqldb).ListForModel,
       MaxAttempts: ..., MaxPerEndpoint: ...,
   }),
   ```
   `middleware.ScheduleDeps.Endpoints` 字段保留（middleware 仍需要它拉主 model 候选）；`Sender` / `Endpoints` / `Scheduler` 三个依赖各管各的。
4. `pkg/middleware/schedule.go`：从 `schedule.Request{}` 字面量中删除 `LoadFallback: deps.Endpoints.ListForModel`。
5. `pkg/router/router_test.go` 同步装配修改（`schedule.New(schedule.Config{... LoadFallback: stubEPProvider{}.ListForModel ...})`）。
6. `go test ./...` 验证。

**回退**：单 commit；发现问题直接 `git revert`。

### 风险评估

- **极低风险**：`Selection` 接口签名不变（不影响外部实现）；只动 `Config` / `Request` 字段。
- **测试覆盖**：现有 `pkg/router/router_test.go` 已覆盖 schedule 装配路径；改动后跑全套即可。

### 与 ADR 0003 协同

ADR 0003 改 `Selection.Pick(ctx)` / `Report(ctx)` 接口；本 ADR 0004 改 `Config` 加 LoadFallback。两条改动**不冲突**，可独立或合并实施：
- 独立：先 0003 后 0004 / 反之均可。
- 合并：单 commit 同时改 `Selection` 接口 + `Config` 字段，两组改动落地一次。

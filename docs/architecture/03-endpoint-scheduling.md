# 03 — Endpoint Scheduling

本文定义端点选择层：`Scheduler` 接口、`Filter` 链、`RetryExecutor` 三层降级、`CooldownManager` 错误隔离、`HealthChecker` 双轨健康检查，以及 `PrefixCacheScheduler` 主动亲和。

> **阅读前**：[01-request-pipeline](01-request-pipeline.md) 的 M7 Schedule 契约；[02-protocol-translation](02-protocol-translation.md) 的 `adapter.Adapter` 与 `domain.ErrorClass`。

## 1. 范围与目标

**范围**：从 `rc.ModelService` 已加载、`rc.LimitSpec` 已构建之后，到选出最终 `rc.Endpoint` 并通过 `Adapter` 拿到响应（含失败时的重试、降级、隔离）。

**目标**：

| # | 目标 | 成功判据 |
|---|------|---------|
| G1 | 任意单 endpoint 失败可自动恢复 | 用户层错误外，瞬时 5xx / 429 / Timeout 99% 能通过重试或 fallback 成功 |
| G2 | Endpoint 形态差异一等公民 | 自部署（含主动健康检查、KV-aware 调度、prefix cache）vs 第三方厂商（仅被动失败计数）在同一调度链路内透明分流 |
| G3 | 失败 endpoint 自动隔离 | 错误分类 → cooldown 时长分级 → 短期不再选 |
| G4 | 限流副作用剔除 | 调度期纯 read-only，扣减只在响应成功后由 [04] Limit 模块执行 |
| G5 | 单/多上游统一 | 一条主链路；endpoint 池可混合形态，按 form 分支能力 |
| G6 | 调度决策可追溯 | 每请求一份完整 `domain.SchedulingDecision` trace（候选 → 过滤 → 打分 → 选中 → 重试） |
| G7 | 策略可配置 | 每模型一份 `Profile`，开关 prefix-cache / busy / RPM / TPM / RPS 调度器 |

## 2. 设计原则

| # | 原则 | 含义 |
|---|------|------|
| S1 | **EndpointForm 一等公民** | `EndpointForm` 派生自 `Endpoint.Capabilities`，决定能否启用 prefix-cache、busy、主动 probe 等需要内部 metric 的调度器 |
| S2 | **三段式 ScheduleI** | `Filter`（淘汰）→ `Score`（打分）→ `Select`（加权随机）三阶段；新调度器按此接口接入，无 switch |
| S3 | **预检 read-only** | 调度期不写 Redis 限流桶，不增 INCR / 不更新计数；扣减由 [04] Limit 模块在响应成功后执行 |
| S4 | **L1 / L2 / L3 三层降级** | L1 同 endpoint 重试 → L2 换 endpoint（同 group 同 model） → L3 换模型（可选） |
| S5 | **Cooldown 按错误类分级** | `domain.ErrorClass` 决定 cooldown 时长；短失败短 cooldown，永久失败长 cooldown |
| S6 | **双轨健康检查** | `FormSelfHosted`：主动 probe + 被动 fail count；`FormVendor`：仅被动 |
| S7 | **每模型一份调度 Profile** | 通过 `ConfigStore` 下发；改 profile 秒级生效，无需发版 |
| S8 | **决策可追溯** | 每次调度产出完整 `domain.SchedulingDecision`（候选数 / 过滤原因 / 选中 endpoint / 重试链），写入 `rc.SchedulingDecision`，由 M10 落 trace |

## 3. 数据结构

### 3.1 domain.Endpoint

```go
// pkg/schedule/endpoint.go
package schedule

import "encoding/json"

// Endpoint 是 ConfigStore 下发的单个上游接入点。
type Endpoint struct {
    ID       string          // 全局唯一
    Vendor   string          // 与 adapter.Vendor 对应
    URL      string          // 上游 base URL
    APIKey   string          // 凭证（运行时按需脱敏 / 存到 secret store）
    Group    string          // 与 domain.UserIdentity.Group 匹配；默认 "default"
    Model    string          // 该 endpoint 服务的模型名（与 ModelService.Model 对齐）
    Weight   int             // 加权随机的基础权重；> 0
    RPM      int             // endpoint 层每分钟请求数硬上限
    TPM      int             // endpoint 层每分钟 token 硬上限
    RPS      int             // endpoint 层每秒请求数硬上限

    // 能力声明：决定 form 与可用调度器
    Capabilities EndpointCapabilities

    Extra json.RawMessage // 厂商专有配置，Adapter 自行解析
}

type EndpointCapabilities struct {
    SelfHosted          bool   // true → FormSelfHosted；false → FormVendor
    KVMetricEndpoint    string // 自部署 KV / 队列深度 metric 抓取地址（空表示无）
    HealthProbeEndpoint string // 自部署主动 probe 地址（空表示无）
    PrefixCacheEnabled  bool   // 该 endpoint 是否参与 prefix cache 一致性哈希
}

type EndpointForm int

const (
    FormVendor     EndpointForm = iota // 第三方厂商（OpenAI、Anthropic、AWS Bedrock 等）
    FormSelfHosted                     // 自部署（vLLM、Ollama、SGLang 等内部可观测的部署）
)

func (e *Endpoint) Form() EndpointForm {
    if e.Capabilities.SelfHosted {
        return FormSelfHosted
    }
    return FormVendor
}
```

> `Capabilities.SelfHosted` 由配置直接声明，**不从 Vendor 字符串猜测**。这样开源后用户可声明任意厂商为"自部署"（配套补 KV metric endpoint 即可启用 busy / prefix-cache 调度）。

### 3.2 domain.SchedulingDecision（调度决策 trace）

```go
// pkg/schedule/decision.go
package schedule

import "time"

type Decision struct {
    Model            string         // rc.ModelService.Model
    UserGroup        string         // rc.Identity.Group
    CandidatesInitial int           // LoadEndpoints 后的数量
    CandidatesFinal   int           // 所有 Filter 后剩余数量
    Selected         *Endpoint      // 首次选中的 endpoint（nil 表示无可用）
    Filters          []FilterRecord // 每个 Filter 的产出
    Attempts         []Attempt      // 实际请求尝试链（含 retry / fallback）
    DurationMs       int64          // 调度本身耗时（不含上游耗时）
}

type FilterRecord struct {
    Name      string   // "CooldownFilter" / "GroupFilter" / "HealthFilter" / ...
    Removed   []string // 被淘汰的 endpoint ID 列表
    Reason    string   // 一行说明
    Preferred string   // PrefixCacheScheduler 等"打分倾向"产出（可选）
}

type Attempt struct {
    Index      int           // 第几次尝试（1 起）
    EndpointID string
    Outcome    string        // "success" / "retry" / "fallback" / "fail"
    LatencyMs  int64
    ErrorClass string        // domain.ErrorClass.String()，成功时为空
    Started    time.Time
}
```

## 4. Scheduler 接口

```go
// pkg/schedule/scheduler.go
package schedule

import (
    "context"

    "github.com/zereker/llm-gateway/pkg/domain"
)

// Scheduler 是调度链路的入口；输入候选池 + 上下文，输出一个 endpoint。
//
// 调用方（RetryExecutor）在 fallback 时通过 excluded 参数排除已尝试过的 endpoint。
type Scheduler interface {
    Pick(ctx context.Context, in PickInput) (*Endpoint, *Decision, error)
}

type PickInput struct {
    Identity     domain.UserIdentity
    ModelService *domain.ModelServiceSnapshot
    Excluded     map[string]struct{} // 已尝试过的 endpoint ID（L2 fallback 用）
    PromptHash   string              // M3 / M5 预计算的 prompt 前 N 字符 hash（PrefixCacheScheduler 用）
    Profile      *Profile            // 该 model 的调度 profile（ConfigStore 下发）
}
```

## 5. Filter 链

每个 Filter 实现统一接口：

```go
// pkg/schedule/filter.go
package schedule

import "context"

// Filter 三段式契约：Filter（淘汰）/ Score（打分）/ Select（加权随机）。
// 大多数 filter 只实现 Filter；PrefixCache 等需打分；最终 weightedRandom 做 Select。
type Filter interface {
    Name() string
    Filter(ctx context.Context, in PickInput, eps []*Endpoint) (kept []*Endpoint, FilterRecord)
}

type Scorer interface {
    Filter
    Score(ctx context.Context, in PickInput, eps []*Endpoint) []ScoredEndpoint
}

type ScoredEndpoint struct {
    Endpoint *Endpoint
    Score    float64 // 越大越优先（影响加权随机）
}
```

### 5.1 默认 Filter 链（顺序固定）

```
[0] LoadEndpoints           — 从 ConfigStore 拉 ModelService.Endpoints；按 Form 打标
[1] CooldownFilter          — 排除处于 cooldown 中的 endpoint
[2] GroupFilter             — identity.Group × Endpoint.Group 匹配
[3] HealthFilter            — 自部署：主动 probe + 被动 fail count；厂商：仅被动
[4] PrefixCacheScheduler    — 仅 FormSelfHosted；prompt hash → 一致性 ring → 主选 + 次选
[5] BusyFilter              — 仅 FormSelfHosted；KV cache rate / queue depth 过载过滤
[6] LimitReadFilter         — 读 [04] LimitChecker 当前 endpoint 层使用率（read-only）
[7] WeightedRandomSelect    — 按 Endpoint.Weight × PrefixCache 评分加权随机
```

> Filter 顺序在代码中通过 `Profile.FilterChain []string` 配置；默认 Profile 给出上述顺序。

### 5.2 各 Filter 详细契约

#### CooldownFilter

```go
type CooldownFilter struct {
    Manager CooldownManager
}

func (f *CooldownFilter) Filter(ctx context.Context, _ PickInput, eps []*Endpoint) ([]*Endpoint, FilterRecord) {
    var kept []*Endpoint
    var removed []string
    for _, ep := range eps {
        if f.Manager.IsCooldown(ep.ID) {
            removed = append(removed, ep.ID)
            continue
        }
        kept = append(kept, ep)
    }
    return kept, FilterRecord{Name: "CooldownFilter", Removed: removed, Reason: "in cooldown"}
}
```

#### GroupFilter

```go
type GroupFilter struct{}

func (GroupFilter) Filter(_ context.Context, in PickInput, eps []*Endpoint) ([]*Endpoint, FilterRecord) {
    var kept []*Endpoint
    var removed []string
    for _, ep := range eps {
        if ep.Group != in.Identity.Group {
            removed = append(removed, ep.ID)
            continue
        }
        kept = append(kept, ep)
    }
    return kept, FilterRecord{Name: "GroupFilter", Removed: removed, Reason: "group mismatch"}
}
```

> **PTU / Reserved 隔离**：通过 `domain.UserIdentity.Group="reserved"` × `Endpoint.Group="reserved"` 自然实现；无需代码 if 分支。

#### HealthFilter

```go
type HealthFilter struct {
    Checker HealthChecker
}

// 内部按 ep.Form() 分流：
// - FormSelfHosted：查主动 probe 结果 AND 被动 fail count
// - FormVendor：仅被动 fail count
```

#### PrefixCacheScheduler（仅 FormSelfHosted 生效）

```go
// 一致性哈希 ring：每个 Profile 维护一份；ring 节点 = 该模型下所有 SelfHosted endpoint
//
// Pick 时：
//   1. PromptHash 在 ring 上定位 → 主选 endpoint
//   2. 若主选已被前面 Filter 淘汰 → 顺时针下一个
//   3. Score 阶段给主选加权（默认 +0.5），影响最终加权随机概率
type PrefixCacheScheduler struct {
    rings *RingCache // per-model 缓存的一致性哈希 ring
}

func (s *PrefixCacheScheduler) Score(_ context.Context, in PickInput, eps []*Endpoint) []ScoredEndpoint {
    // ...
}
```

**PromptHash 计算**（在 M3 Envelope 后或 M5 ModelService 后注入到 `rc.Extras["prompt_hash"]`）：

```go
hash := sha256.Sum256([]byte(prompt[:min(32, len(prompt))]))
promptHash := hex.EncodeToString(hash[:4]) // 8 字符 hex
```

> Profile 可调整 `PrefixHashLength`（默认 32 字符前缀）。

#### BusyFilter（仅 FormSelfHosted 生效）

读取 endpoint 的 KV cache 占用率 / pending queue 深度，超阈值（如 0.9）则淘汰。
KV metric 由 `Endpoint.Capabilities.KVMetricEndpoint` 配置；Adapter 实现 `BusyMetricProvider` 接口暴露。

```go
type BusyMetricProvider interface {
    GetKVRate(ctx context.Context, ep *Endpoint) (float64, error)
    GetQueueDepth(ctx context.Context, ep *Endpoint) (int, error)
}
```

#### LimitReadFilter

```go
type LimitReadFilter struct {
    Checker ratelimit.Checker
}

// 调用 Checker.PeekEndpoint(ep.ID) 拿当前使用率（read-only），过 0.95 即淘汰
```

#### WeightedRandomSelect

最后一步。输入 `[]ScoredEndpoint`（默认 score = 0），权重 = `Endpoint.Weight × (1 + score)`。
使用 `crypto/rand` 或带种子的 `math/rand` 加权随机。

## 6. RetryExecutor

```go
// pkg/schedule/retry_executor.go
package schedule

import (
    "context"

    "github.com/gin-gonic/gin"

    "github.com/zereker/llm-gateway/pkg/domain"
)

// RetryExecutor 是 M7 Schedule 的执行体：选 endpoint → 调 Adapter → 失败决定 retry / fallback。
type RetryExecutor interface {
    Run(c *gin.Context, rc *domain.RequestContext) error
}

type Executor struct {
    Scheduler Scheduler
    Adapters  AdapterFactory // 按 Vendor 取 Adapter 工厂
    Cooldown  CooldownManager
    Health    HealthChecker
    Policy    Policy
}

type Policy struct {
    MaxAttemptsPerEndpoint  int  // L1 retry 上限（默认 2，含首次共 3 次）
    MaxTotalAttempts        int  // 跨 endpoint 总上限（默认 5）
    Backoff                 BackoffStrategy
    AllowCrossModelFallback bool // L3 开关（默认 false）
}

type BackoffStrategy struct {
    InitialMs int     // 默认 100
    MaxMs     int     // 默认 5000
    Factor    float64 // 默认 2.0
    Jitter    float64 // 默认 0.2
}
```

### 6.1 主循环

```go
func (e *Executor) Run(c *gin.Context, rc *domain.RequestContext) error {
    excluded := map[string]struct{}{}
    var lastErr *domain.AdapterError

    for total := 0; total < e.Policy.MaxTotalAttempts; {
        ep, dec, err := e.Scheduler.Pick(rc.Ctx, PickInput{
            Identity:     rc.Identity,
            ModelService: rc.ModelService,
            Excluded:     excluded,
            PromptHash:   getPromptHash(rc),
            Profile:      getProfile(rc),
        })
        rc.SchedulingDecision = dec

        if err != nil || ep == nil {
            // 全部 endpoint 已 cooldown 或被淘汰
            rc.Error = lastErr
            if rc.Error == nil {
                rc.Error = &domain.AdapterError{Class: domain.ErrRateLimit, HTTPStatus: 429, Message: "no available endpoint"}
            }
            return rc.Error
        }

        rc.Endpoint = ep
        adapter := e.Adapters.Get(ep.Vendor)()
        rc.Adapter = adapter

        for attempt := 0; attempt < e.Policy.MaxAttemptsPerEndpoint && total < e.Policy.MaxTotalAttempts; attempt, total = attempt+1, total+1 {
            usage, _, callErr := callAdapter(c, rc, adapter, ep)
            if callErr == nil {
                rc.Usage = usage
                appendAttempt(rc, attempt, ep, "success", 0)
                return nil
            }
            lastErr = callErr
            appendAttempt(rc, attempt, ep, classifyOutcome(callErr.Class), callErr.HTTPStatus)

            // L1 retry：仅 Transient / RateLimit 短退避
            if !shouldRetrySameEndpoint(callErr.Class) {
                break
            }
            sleepBackoff(e.Policy.Backoff, attempt)
        }

        // L2 fallback：累计该 endpoint 失败计数；超阈值进 cooldown
        e.Cooldown.OnFailure(ep.ID, lastErr.Class)
        excluded[ep.ID] = struct{}{}

        // 永久错误（401/403/404/Invalid）不进入下一轮 fallback
        if lastErr.Class == domain.ErrInvalid {
            rc.Error = lastErr
            return lastErr
        }
    }

    rc.Error = lastErr
    return lastErr
}
```

### 6.2 L3 跨模型 fallback（可选）

当 `Policy.AllowCrossModelFallback = true` 且当前 model 的所有 endpoint 都 fallback 失败：

```go
fallbackModel := rc.ModelService.Spec.FallbackModels[i]  // 配置在 ModelService.SpecDetail
// 重写 rc.ModelService 为 fallback model，重启主循环
```

> 默认 false。每对可互切的 model 必须显式配置（防止意外跨 model 路由破坏成本估算）。

## 7. CooldownManager

```go
// pkg/schedule/cooldown.go
package schedule

import (
    "context"
    "time"

    "github.com/zereker/llm-gateway/pkg/domain"
)

type CooldownManager interface {
    OnFailure(epID string, class domain.ErrorClass)
    IsCooldown(epID string) bool
    Clear(epID string)
}

// DefaultManager 用 KV 存储（详见 [06] Cache 接口）记录 fail_count 和 cooldown 标记。
type DefaultManager struct {
    Store        Store // 抽象 KV（Redis / 内存）
    AllowedFails int   // 默认 3
    Durations    map[domain.ErrorClass]time.Duration
}

// 默认 Durations
// domain.ErrTransient:  60 * time.Second
// domain.ErrRateLimit:  30 * time.Second
// domain.ErrPermanent:  300 * time.Second
// domain.ErrUnknown:    60 * time.Second
// domain.ErrInvalid:    0 (不进 cooldown)

func (m *DefaultManager) OnFailure(epID string, class domain.ErrorClass) {
    if class == domain.ErrInvalid {
        return
    }
    cnt := m.Store.Incr(failKey(epID), 5*time.Minute) // 5 分钟内累计
    if cnt >= int64(m.AllowedFails) {
        m.Store.Set(cooldownKey(epID), "1", m.Durations[class])
        m.Store.Del(failKey(epID))
    }
}

func (m *DefaultManager) IsCooldown(epID string) bool {
    return m.Store.Exists(cooldownKey(epID))
}
```

> `Store` 接口在 [06] 定义；默认实现支持 Redis（多实例共享）和内存（单实例 / 测试）。

## 8. HealthChecker

```go
// pkg/schedule/health.go
package schedule

import "context"

type HealthChecker interface {
    IsHealthy(ctx context.Context, ep *Endpoint) bool
}
```

### 8.1 自部署：双轨

```go
type SelfHostedChecker struct {
    Prober      Prober      // 主动 probe（HTTP GET 到 ep.HealthProbeEndpoint）
    FailCounter Store       // 被动 fail count（与 CooldownManager 共享）
    StaleAfter  time.Duration // 主动 probe 多久未更新视为"不健康"
}

func (c *SelfHostedChecker) IsHealthy(ctx context.Context, ep *Endpoint) bool {
    last, ok := c.Prober.LastResult(ep.ID)
    if !ok || time.Since(last.At) > c.StaleAfter {
        return false // 长期无 probe 结果 → 不健康
    }
    if !last.Healthy {
        return false
    }
    // 被动 fail count 在 CooldownFilter 已处理；这里不重复
    return true
}
```

`Prober` 是独立 goroutine，对所有 `FormSelfHosted` endpoint 周期性 GET：

```go
type Prober interface {
    Start(ctx context.Context)
    LastResult(epID string) (Result, bool)
}

type Result struct {
    Healthy bool
    At      time.Time
    Latency time.Duration
}
```

### 8.2 厂商：仅被动

```go
type VendorChecker struct{}

func (VendorChecker) IsHealthy(_ context.Context, _ *Endpoint) bool {
    return true // 被动 fail count 在 CooldownFilter 已处理
}
```

## 9. Profile 配置

```go
// pkg/schedule/profile.go
package schedule

type Profile struct {
    EnablePrefixCache  bool
    EnableBusy         bool
    EnableRPSScheduler bool
    EnableTPMScheduler bool
    EnableRPMScheduler bool
    PrefixHashLength   int
    GroupStrict        bool
    FilterChain        []string // 自定义 Filter 顺序；空时用默认
    RetryPolicy        Policy
}

// DefaultProfile 给新模型用的默认 profile
var DefaultProfile = Profile{
    EnablePrefixCache:  true,
    EnableBusy:         true,
    EnableRPSScheduler: true,
    EnableTPMScheduler: true,
    EnableRPMScheduler: true,
    PrefixHashLength:   32,
    GroupStrict:        true,
    RetryPolicy: Policy{
        MaxAttemptsPerEndpoint: 2,
        MaxTotalAttempts:       5,
        Backoff: BackoffStrategy{InitialMs: 100, MaxMs: 5000, Factor: 2.0, Jitter: 0.2},
    },
}
```

每模型 `Profile` 通过 `ConfigStore` 下发（详见 [06]）；改 profile 秒级生效。

## 10. AdapterFactory（与 [02] 衔接）

```go
// pkg/schedule/adapter_factory.go
package schedule

import "github.com/zereker/llm-gateway/pkg/adapter"

// AdapterFactory 抽象"按 Vendor 取出 Adapter 工厂"。
// 默认实现是 adapter.Get；测试可注入 mock。
type AdapterFactory interface {
    Get(vendor string) adapter.Factory
}

type DefaultAdapterFactory struct{}

func (DefaultAdapterFactory) Get(vendor string) adapter.Factory {
    return adapter.Get(vendor)
}
```

## 11. 数据流时序

### 11.1 正常路径（一次成功）

```
M7 Schedule.Run:
  1. Scheduler.Pick → ep1 (Decision: 8 候选 → 1 选中, prefix-cache hit)
  2. RetryExecutor: callAdapter(ep1)
     ├─ adapter.Init / BuildURL / BuildHeaders / TransformRequest
     ├─ http.Do → 200
     └─ session.Feed* + Finalize → usage
  3. rc.Usage = usage; appendAttempt(success)
  4. Cooldown 不触发；excluded 为空
  返回成功，M9 Recover 不写错；M10 Tracing 发 Usage 事件
```

### 11.2 失败路径：L1 同 endpoint 重试

```
1. Scheduler.Pick → ep1
2. RetryExecutor:
   attempt 0: callAdapter(ep1) → Timeout (domain.ErrTransient)
              shouldRetrySameEndpoint(Transient) = true
              sleep(backoff)
   attempt 1: callAdapter(ep1) → 200
   appendAttempt(0=retry, 1=success)
   返回成功
```

### 11.3 失败路径：L2 切 endpoint

```
1. Scheduler.Pick → ep1
2. attempt 0..1: callAdapter(ep1) → 5xx 两次
3. Cooldown.OnFailure(ep1, Transient)；excluded.add(ep1)
4. 主循环继续：Scheduler.Pick(excluded={ep1}) → ep2
5. callAdapter(ep2) → 200
   appendAttempt(0=retry, 1=fallback, 2=success)
```

### 11.4 失败路径：全组 cooldown

```
循环 N 次后所有 endpoint 都进入 excluded
Scheduler.Pick → nil（或返回 domain.ErrRateLimit）
若 Policy.AllowCrossModelFallback = true：尝试 fallback model
否则：rc.Error = domain.ErrRateLimit (429)
```

### 11.5 限流模型层超限场景

```
M6 Limit 已写入 rc.Extras["service_blocked"] = true
RetryExecutor 主循环检查这个标志：
  if rc.Extras["service_blocked"] && len(reservedEndpoints) > 0:
    优先尝试 reserved group 的 endpoint（如果用户有权限）
  else:
    继续按 default group 走 fallback；全部 cooldown 后返回 429
```

## 12. 可观测性

```
scheduler.endpoint.selected_total{ep_id, model, user_group}
scheduler.endpoint.filtered_total{ep_id, filter_name, reason}
scheduler.endpoint.call_total{ep_id, status}
scheduler.endpoint.call_latency_ms{ep_id, quantile}
scheduler.endpoint.fail_count{ep_id, error_class}
scheduler.endpoint.cooldown_enter_total{ep_id, error_class}

scheduler.retry.attempts_total{model, outcome}        # outcome=success_first / success_retry / fail
scheduler.fallback.cross_endpoint_total{model}
scheduler.fallback.cross_model_total{model}           # L3
scheduler.decision_duration_ms{quantile}
scheduler.prefix_cache.hit_rate
```

trace 字段：完整 `domain.SchedulingDecision` JSON 写入 `rc.SchedulingDecision`，由 M10 落出。

## 13. 测试矩阵

| # | 场景 | 预期 |
|---|------|-----|
| S1 | 正常 1 endpoint 1 次成功 | Decision 含 1 attempt success |
| S2 | 5xx + 重试成功 | Decision 含 2 attempts (1 retry, 1 success) |
| S3 | 5xx + 重试失败 + L2 fallback 成功 | excluded.add(ep1)；ep2 success |
| S4 | 全组都 5xx | rc.Error = domain.ErrTransient (502) |
| S5 | 全组 cooldown | Pick 返回 nil；rc.Error = 429 |
| S6 | 401 立即不重试 | attempt 0 失败 → 直接 L2 |
| S7 | 400 客户端错误 | 不重试，rc.Error = domain.ErrInvalid |
| S8 | Group 不匹配 | GroupFilter 全淘汰，rc.Error = 429 |
| S9 | PrefixCache 主选被淘汰 | 顺时针次选；trace 中 preferred ≠ selected |
| S10 | BusyFilter 阈值过滤 | 高 KV rate endpoint 被淘汰 |
| S11 | RetryPolicy 总尝试数 = 5 | 不超过；超过仍失败返回最后错误 |

## 14. 演进规则

- **新增 Filter**：实现 `Filter` 或 `Scorer` 接口；在 `Profile.FilterChain` 中插入相应位置；本文档第 5 节注册新 Filter
- **新增错误类**：在 `domain.ErrorClass` 加常量；同步更新 [02] 第 8 章和本文档 `Cooldown.Durations`
- **修改默认 Profile**：本文档第 9 节同步更新；评估对存量模型的影响
- **修改 RetryExecutor 主循环**：必须有完整测试覆盖（第 13 节矩阵 + 边界）
- **新增 Endpoint 字段**：本文档第 3.1 节同步；评估 ConfigStore schema 兼容性（详见 [06]）

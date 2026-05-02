# 04 — Rate Limiting

本文定义限流层：`limit.Spec` 数据结构、`limit.Checker` 接口、三层 AND 级联、`BuildSpec` 四级查询链，以及与计量 `Usage` 的对接（预检 + 后置 Consume 双阶段）。

> **阅读前**：[01-request-pipeline](01-request-pipeline.md) 的 M6 Limit 与 M10 Tracing 契约；[02](02-protocol-translation.md) 的 `errs.RateLimit`；[03](03-endpoint-scheduling.md) 的 endpoint Filter 链。

## 1. 范围与目标

**范围**：从 M6 Limit 进入到响应成功后由 M10 Tracing 调 `Consume` 的整条限流链路。
不含：限流配置如何下发（详见 [06] ConfigStore）；底层存储（Redis / 内存抽象在 [06]）。

**目标**：

| # | 目标 | 成功判据 |
|---|------|---------|
| G1 | 三层限流全部在主链路生效 | 用户层 / 模型层 / endpoint 层都同步前置 |
| G2 | 多租户公平 | 单租户超限不影响其他租户；reserved group 独占资源 |
| G3 | 与 Usage 同源 | 限流扣减输入 = `usage.Usage`；不再独立解析 token |
| G4 | 超售可控 | 用户配额总和允许 > 模型容量；模型层兜底 + 监控 |
| G5 | 配置秒级生效 | 通过 [06] ConfigStore Watch 推送；无需发版 |
| G6 | 副作用剔除 | 调度期 read-only；扣减只在响应成功后做一次 |

## 2. 设计原则

| # | 原则 | 含义 |
|---|------|------|
| L1 | **三层 AND 级联** | 用户层 AND 模型层（仅 default group）AND endpoint 层；任一超限即拒 |
| L2 | **预检 + 后置 Consume 双阶段** | M6 调 `CheckReadOnly`（不写）；M10 调 `Consume(usage)`（按真实用量扣三层桶）|
| L3 | **Group 决定路径** | `identity.User.Group` × `Endpoint.Group` 配置匹配，自然实现 reserved / default 隔离；无 if 分支 |
| L4 | **模型层共享桶** | 同模型所有 default 用户共享同一个 Redis key，争抢式分配；reserved group 不接触此 key |
| L5 | **超售允许 + 兜底监控** | 不做"用户配额总和 ≤ 模型容量"校验；模型层硬上限是真实容量保护；超售比 metric 持续观察 |
| L6 | **四级查询链** | 阈值优先级：apikey > user > model_service.default > 硬编码兜底 |
| L7 | **Usage 多维** | `Spec` 三层都基于 `usage.Usage` 多维（Input/Output/Reasoning + Details）扣减；新维度自动参与 |

## 3. 数据结构

### 3.1 limit.Spec

```go
// internal/limit/spec.go
package limit

// Spec 是 M6 Limit 为本次请求构建的三层阈值快照。
type Spec struct {
    User     LayerSpec   // 用户层（按 UserID / APIKeyID 维度）
    Service  *LayerSpec  // 模型层（仅 identity.Group == "default" 时非 nil）
    Endpoint *LayerSpec  // endpoint 层（M7 选定 endpoint 后由 [03] 填，本层在 M6 时仅占位）

    // 来源标识
    UserSource    Source // 该层阈值的来源（用于 trace / debug）
    ServiceSource Source
}

type LayerSpec struct {
    RPM   int64 // 每分钟请求数；0 = 不限
    TPM   int64 // 每分钟 token 数（含 input + output + reasoning + details 总和）
    RPS   int64 // 每秒请求数；0 = 不限
    Extra map[string]int64 // 可选维度，如 audio_seconds_per_min
}

type Source int

const (
    SourceHardcoded Source = iota // 代码兜底默认值
    SourceModelDefault            // ModelService.SpecDetail 中的默认值
    SourceUser                    // 用户级配置
    SourceAPIKey                  // API Key 级配置
)
```

### 3.2 limit.Checker 接口

```go
// internal/limit/checker.go
package limit

import (
    "context"

    "github.com/zereker-labs/ai-gateway/internal/identity"
    "github.com/zereker-labs/ai-gateway/internal/modelservice"
    "github.com/zereker-labs/ai-gateway/internal/scheduling"
    "github.com/zereker-labs/ai-gateway/internal/usage"
)

type Checker interface {
    // BuildSpec 为本次请求构建三层阈值（不含 Endpoint 层；endpoint 选定后由 [03] 单独 read 检查）
    BuildSpec(id identity.User, ms *modelservice.Snapshot) *Spec

    // CheckReadOnly 三层预检（用户层 + 模型层）。read-only，不写桶。
    // Endpoint 层不在此处检查（在 [03] 的 LimitReadFilter 内做）。
    CheckReadOnly(ctx context.Context, spec *Spec, id identity.User, ms *modelservice.Snapshot) CheckResult

    // PeekEndpoint 给 [03] LimitReadFilter 用：读 endpoint 层当前使用率。
    PeekEndpoint(ctx context.Context, ep *scheduling.Endpoint) EndpointUsage

    // Consume 响应成功后由 M10 调用。按真实 Usage 扣三层桶。
    Consume(ctx context.Context, spec *Spec, id identity.User, ep *scheduling.Endpoint, u *usage.Usage) error
}

type CheckResult struct {
    UserBlocked    bool   // 用户层超限 → M6 直接 abort 429
    ServiceBlocked bool   // 模型层超限 → 不 abort，写 rc.Extras["service_blocked"]，让 [03] 决定 fallback
    Reason         string
}

type EndpointUsage struct {
    RPMUsed int64 // 当前分钟已用
    RPMCap  int64
    TPMUsed int64
    TPMCap  int64
    RPSUsed int64
    RPSCap  int64
}
```

## 4. BuildSpec 四级查询链

```go
func (c *DefaultChecker) BuildSpec(id identity.User, ms *modelservice.Snapshot) *Spec {
    spec := &Spec{}

    // 用户层：APIKey > User > ModelService.default > Hardcoded
    if userLayer, src := c.lookupUser(id, ms); userLayer != nil {
        spec.User = *userLayer
        spec.UserSource = src
    } else {
        spec.User = c.hardcoded.User
        spec.UserSource = SourceHardcoded
    }

    // 模型层：仅 default group 走
    if id.Group == "default" {
        if svcLayer := c.lookupService(ms); svcLayer != nil {
            spec.Service = svcLayer
            spec.ServiceSource = SourceModelDefault
        }
    }

    // Endpoint 层：M7 选定 endpoint 后由 [03] 内的 LimitReadFilter 单独读
    return spec
}

func (c *DefaultChecker) lookupUser(id identity.User, ms *modelservice.Snapshot) (*LayerSpec, Source) {
    if l := c.store.GetAPIKeyLimit(id.APIKeyID, ms.ServiceID); l != nil {
        return l, SourceAPIKey
    }
    if l := c.store.GetUserLimit(id.UserID, ms.ServiceID); l != nil {
        return l, SourceUser
    }
    if l := c.store.GetServiceDefaultUserLimit(ms.ServiceID); l != nil {
        return l, SourceModelDefault
    }
    return nil, SourceHardcoded
}
```

## 5. Redis Key Schema

| 层 | Key 模式 | 维度 | 语义 |
|---|---------|------|------|
| 用户层 | `rl:user:{user_id_or_apikey}:{service_id}:{window}` | 每用户一桶 | 防单用户挤占 |
| 模型层 | `rl:service:{service_id}:default:{window}` | 全局单桶（default group 共享）| 防整体超模型容量 |
| Endpoint 层 | `rl:endpoint:{endpoint_id}:{window}` | 每 endpoint 一桶 | 防打爆上游 |

`{window}` 由 `WindowKey(now)` 计算，按算法不同：

```go
// 默认 Fixed Window
func WindowKey(now time.Time, granularity time.Duration) string {
    return strconv.FormatInt(now.Truncate(granularity).Unix(), 10)
}
// RPM: granularity = time.Minute → "1714723200"
// RPS: granularity = time.Second → "1714723245"
```

> **重要**：模型层 key 不含 `user_id`；所有 default group 用户**共用同一桶**，争抢式分配。reserved group 用户的请求路径根本不接触此 key（M6 内 `if id.Group == "default"` 判断后才查）。

### 5.1 Lua 脚本（原子 INCRBY + EXPIRE）

```lua
-- limit_check.lua
-- KEYS[1] = bucket key
-- ARGV[1] = capacity（阈值）
-- ARGV[2] = increment（本次预扣量；CheckReadOnly 时 = 0，Consume 时 = usage 值）
-- ARGV[3] = ttl_seconds
-- 返回：{current, blocked}  blocked = 1 表示超限

local current = redis.call('GET', KEYS[1])
if current == false then current = 0 else current = tonumber(current) end

local cap = tonumber(ARGV[1])
local incr = tonumber(ARGV[2])

if cap > 0 and current + incr > cap then
    return {current, 1}
end

if incr > 0 then
    local new = redis.call('INCRBY', KEYS[1], incr)
    redis.call('EXPIRE', KEYS[1], tonumber(ARGV[3]))
    return {new, 0}
end

return {current, 0}
```

**调用方式**：
- `CheckReadOnly`：`incr = 0`，仅读 + 比较，不写
- `Consume`：`incr = 实际 Usage 值`，原子 INCR + EXPIRE

### 5.2 Store 接口（[06] 中定义）

```go
// internal/limit/store.go
package limit

import "context"

type Store interface {
    EvalLimit(ctx context.Context, key string, cap, incr int64, ttlSec int64) (current int64, blocked bool, err error)

    // 配置查询：从 ConfigStore 加载并缓存（详见 [06]）
    GetAPIKeyLimit(apiKeyID, serviceID string) *LayerSpec
    GetUserLimit(userID, serviceID string) *LayerSpec
    GetServiceDefaultUserLimit(serviceID string) *LayerSpec
    GetServiceLimit(serviceID string) *LayerSpec        // 模型层硬上限
    GetEndpointLimit(endpointID string) *LayerSpec      // endpoint 层硬上限
}
```

> 默认实现支持 Redis（多实例共享）和内存（单实例 / 测试），切换走 [06] Cache 抽象。

## 6. 数据流

### 6.1 default group 用户

```
M6 Limit:
  spec = checker.BuildSpec(rc.Identity, rc.ModelService)
  // spec.User = APIKey 配置；spec.Service = Model 默认；spec.Endpoint = nil（待 [03] 填）

  result = checker.CheckReadOnly(ctx, spec, id, ms)
  // 内部：
  //   evalLimit("rl:user:alice:gpt-4:current_minute", spec.User.RPM, 0, 60)
  //   evalLimit("rl:service:gpt-4:default:current_minute", spec.Service.RPM, 0, 60)
  //   两者均通过 → result = {UserBlocked: false, ServiceBlocked: false}

  rc.LimitSpec = spec
  c.Next()

[03] LimitReadFilter（M7 内）:
  for ep in candidates:
    u = checker.PeekEndpoint(ctx, ep)
    if u.RPMUsed/u.RPMCap > 0.95: 淘汰

[03] 选中 ep1 → callAdapter → 200 → Usage

M10 Tracing:
  spec.Endpoint = LayerSpec{RPM: ep1.RPM, TPM: ep1.TPM, RPS: ep1.RPS}
  checker.Consume(ctx, spec, id, ep1, rc.Usage)
  // 内部三层 INCRBY：
  //   incr 用户层：input + output + reasoning + sum(details) tokens
  //   incr 模型层：同上
  //   incr endpoint 层：同上 + RPM/RPS 各 +1
```

### 6.2 reserved group 用户（如 PTU 客户）

```
M6 Limit:
  spec.Service = nil  （因为 id.Group != "default"）
  CheckReadOnly:
    evalLimit("rl:user:bob:gpt-4:current_minute", ..., 0, 60)
    模型层跳过
  通过

M7 Schedule:
  GroupFilter 只保留 ep.Group == "reserved" 的 endpoint
  → 选中 reservedEp1

M10 Tracing:
  Consume: 仅扣用户层 + endpoint 层；模型层不动
```

### 6.3 用户层超限

```
M6 Limit:
  CheckReadOnly → UserBlocked = true
  metric.Inc(RateLimitCheckTotal, "layer", "user", "result", "blocked")
  abort(c, 429, "user rate limit exceeded")
```

### 6.4 模型层超限（仅 default group）

```
M6 Limit:
  CheckReadOnly → ServiceBlocked = true
  metric.Inc(RateLimitCheckTotal, "layer", "service", "result", "blocked")
  rc.Extras["service_blocked"] = true
  c.Next()  // 继续走 M7

M7 Schedule:
  RetryExecutor 检查 rc.Extras["service_blocked"]：
  - 如果用户有 reserved group 权限 → 优先尝试 reserved endpoint（需要业务侧授权）
  - 否则按 default group 走 fallback；全部 cooldown 后返回 429
```

> **设计权衡**：模型层超限不直接 429，是为了允许"模型层硬上限达到，但部分 endpoint 还有空闲"时仍能服务（fallback 内的 endpoint 层硬上限更细粒度）。

### 6.5 Endpoint 层硬上限

```
[03] LimitReadFilter:
  PeekEndpoint(ep1) → RPMUsed/RPMCap = 0.97
  ep1 被淘汰

如果所有 endpoint 都打满：
  Scheduler.Pick → nil → RetryExecutor 返回 errs.RateLimit (429)
```

## 7. 配置 Schema

通过 [06] `ConfigStore` 下发；本节定义 schema。

```go
// internal/limit/config.go
package limit

// LimitConfig 是 ConfigStore 中限流相关的所有配置的统一形态。
// 各字段独立放置在不同 ConfigStore 路径下，按需 Watch / 加载。
type LimitConfig struct {
    // /ratelimit/apikey/{api_key_id}/{service_id}
    APIKey map[string]map[string]LayerSpec

    // /ratelimit/user/{user_id}/{service_id}
    User map[string]map[string]LayerSpec

    // /ratelimit/service/{service_id}
    Service map[string]ServiceLimits
}

type ServiceLimits struct {
    Model       LayerSpec  // 模型层硬上限
    DefaultUser LayerSpec  // 该模型下用户层默认值（四级查询链的第三级）
}

// /ratelimit/endpoint/{endpoint_id} 在 scheduling.Endpoint.RPM/TPM/RPS 中
```

```yaml
# 示例 ConfigStore key/value
/ratelimit/apikey/ak_123/svc_gpt4o:
  RPM: 600
  TPM: 100000
/ratelimit/user/alice/svc_gpt4o:
  RPM: 200
  TPM: 30000
/ratelimit/service/svc_gpt4o:
  Model:
    RPM: 10000
    TPM: 5000000
  DefaultUser:
    RPM: 60
    TPM: 10000
```

## 8. Hardcoded 兜底

```go
// internal/limit/hardcoded.go
package limit

var DefaultHardcoded = struct {
    User LayerSpec
}{
    User: LayerSpec{
        RPM: 60,    // 1 req/sec
        TPM: 10000, // 10K tokens/min
        RPS: 5,
    },
}
```

> 仅当四级查询链全 miss 时使用；正常运行时应永远命中前三级。

## 9. 超售监控

```
ratelimit.oversell_ratio{service}
  = sum_active_users(user_layer.TPM) / model_layer.TPM
  告警阈值：> 5 持续 1 小时

ratelimit.rejection_rate{service, layer}
  = blocked_count / total_count
  告警阈值：layer=user > 0.5%；layer=service > 1%；layer=endpoint > 3%
```

## 10. 可观测性

```
ratelimit.check_duration_ms{layer, quantile}
ratelimit.consume_duration_ms{layer, quantile}
ratelimit.check_total{layer, result}            # result=pass / blocked
ratelimit.bucket_value{layer, key_type}         # 当前桶值（不带具体 key 防高基数）
ratelimit.config_reload_total{source}           # 配置重载次数
ratelimit.oversell_ratio{service}
ratelimit.rejection_rate{service, layer}
```

trace 字段（写入 `rc.LimitSpec`，由 M10 落 trace）：

```
limit.spec.user.rpm / .tpm / .rps
limit.spec.user.source           # SourceAPIKey / User / ModelDefault / Hardcoded
limit.spec.service.rpm / .tpm    # 仅 default group 有
limit.check.user_used_pct
limit.check.service_used_pct
```

## 11. 测试矩阵

| # | 场景 | 预期 |
|---|------|-----|
| L1 | 用户层未超 | CheckResult.UserBlocked = false；通过 |
| L2 | 用户层超 RPM | UserBlocked = true；M6 abort 429 |
| L3 | 用户层超 TPM | 同上 |
| L4 | 模型层超（default 用户）| ServiceBlocked = true；M6 不 abort，rc.Extras 写入 |
| L5 | 模型层超（reserved 用户）| 跳过模型层检查，通过 |
| L6 | Endpoint 层超 | [03] LimitReadFilter 淘汰该 endpoint；可能触发 fallback |
| L7 | Consume 写三层 | Redis 三个 key 都 INCRBY；TTL 60s |
| L8 | Consume reserved 用户 | 仅写用户层 + endpoint 层 |
| L9 | BuildSpec 四级查询 | APIKey 命中优先；APIKey 缺失退 User；User 缺失退 ModelDefault；全 miss 用 Hardcoded |
| L10 | 超售比 metric | 配置 sum > 模型容量时，metric 反映正确 |
| L11 | Lua 脚本原子性 | 并发 100 个 Consume 同 key，最终值 = 100 × incr |

## 12. 演进规则

- **新增维度**（如 `audio_seconds_per_min`）：在 `LayerSpec.Extra` 加；Consume 时同步扣减；Spec 加可选阈值；本文档第 3.1 节同步
- **新增分组**（如 "premium"）：identity / endpoint 配置加新值；M6 不需改代码（已通过 group 字段路径分流）；本文档第 6 节加场景
- **修改 Lua 脚本**：必须保证原子性；测试覆盖并发场景；版本化部署（同时支持新旧）
- **修改 BuildSpec 查询顺序**：本文档第 4 节同步；评估对存量配置的影响
- **新增 Source**：`Source` 加常量；本文档第 3.1 节同步；trace label 同步

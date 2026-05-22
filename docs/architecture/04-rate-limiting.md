# 04 — Rate Limiting

本文记录限流设计目标。限流分两类：

1. **用户侧 quota**：account / API key / model 维度，M6 处理。
2. **Endpoint quota**：上游 endpoint 自身容量，M7 选中 endpoint 后处理。

限流 bucket 只服务流控，不作为计费账本。计费以 usage outbox 为准。

## 1. 设计原则

- M6 只处理用户侧 quota，不混入 endpoint quota。
- M7 只对最终选中的 endpoint 做 endpoint quota reserve，不在候选 filter 阶段扣所有 endpoint。
- 用户侧 RPM / RPS 在请求前 reserve。
- TPM 不请求前预扣，不读取 `max_tokens`，只在响应后按真实 usage 后扣；它是事后计数器，不保证请求开始前的 token 上限。
- 不返回 `X-RateLimit-*` headers；限流状态不作为客户端契约暴露。
- Redis 是生产唯一 Store；多副本 gateway 必须共享计数器。

## 2. M6 用户侧限流

M6 位于 M5 ModelService 之后、M7 Schedule 之前。

流程：

```text
identity = rc.Identity
model = rc.ModelService.Model

rules = load account quota policy + api key quota policy
buckets = build user RPM/RPS buckets by additive policy
ReserveBatch(buckets)
if denied:
    return 429 + Retry-After

c.Next()

usage = rc.Usage  # M7 thin adapter 从 dispatch.Outcome 写回；nil 表示未提取到 usage
if usage != nil:
    build user TPM buckets by additive policy
    ChargeBatch(TPM buckets with cost = usage.Total)
```

RPM / RPS 是前扣：cost 固定为 1，请求开始前已知，用于挡住请求洪峰。

TPM 是后扣：请求已经完成后才知道真实 token，因此不做 pre-reserve。TPM 后扣失败不改变本次响应。用户侧 ReserveBatch 默认不读取 TPM bucket，所以 TPM 超限本身不会拒绝后续请求；它用于事后观测、报表和运营告警。需要强 token 上限的业务应配置更严格的 RPM/RPS 或另行引入显式的 TPM soft-check 方案。

## 3. 数据来源

身份来自 M2：

```go
type UserIdentity struct {
    AccountID             string
    SubAccountID          string
    APIKeyID              string
    Group                 string
    ExternalUser          bool
    AccountQuotaPolicyID  *int64
    APIKeyQuotaPolicyID   *int64
}
```

quota policy 来自 SQL 表 `quota_policies`，`rule_json` 解析为：

```go
type PolicyRule struct {
    Default  *QuotaConfig           `json:"default,omitempty"`
    PerModel map[string]QuotaConfig `json:"per_model,omitempty"`
}
```

示例：

```json
{
  "default": {"rpm": 60, "tpm": 100000},
  "per_model": {
    "gpt-4o": {"rpm": 10, "tpm": 30000}
  }
}
```

## 4. Additive 语义

`PolicyRule.PickRulesAdditive(model)` 同时返回：

- default rule，scope 为 `*`
- 命中的 per-model rule，scope 为当前 model

两者都存在时会同时消耗。这样 per-model 是 default 的子上限，避免“命中 per-model 后绕过总量上限”。

主账号层和 API key 层也会同时消耗；两层策略彼此独立、叠加生效。RPM/RPS 这类前扣 bucket 任一超限则整批拒绝；TPM bucket 是后扣计数器，不参与请求前拒绝。

## 5. Redis Store 接口

目标接口：

```go
type Store interface {
    ReserveBatch(ctx context.Context, buckets []Bucket) (allowed bool, violated *BucketViolation, err error)
    ChargeBatch(ctx context.Context, buckets []Bucket) ([]BucketChargeResult, error)
    SnapshotBatch(ctx context.Context, buckets []Bucket) ([]BucketState, error)
}
```

`ReserveBatch` 用于请求前限流，是多 key 原子 all-or-nothing；任一 bucket 超限则整批不写入，调用方返回 429 或换 endpoint。算法使用 sliding window counter，避免 fixed window 边界 2 倍突刺。

`ChargeBatch` 用于响应后记账，必须写入实际 usage；即使写入后超过上限，也不能拒绝已经完成的响应。返回值可标记哪些 bucket 已经 over limit，供日志、metric 和运营告警使用。

`SnapshotBatch` 是 read-only，用于后续 endpoint quota / 可观测场景读取当前状态，不产生扣减副作用。

## 6. Bucket 命名

M6 用户侧 bucket：

```text
rl:quota:<layer>:<subject>:<scope>:<dim>
```

字段含义：

- `layer`：`account` 或 `apikey`
- `subject`：主账号 pin 或 api_key_id
- `scope`：`*` 或实际 model
- `dim`：`rpm`、`tpm`、`rps`

示例：

```text
rl:quota:account:default:*:rpm
rl:quota:account:default:gpt-4o:tpm
rl:quota:apikey:ak_alice:*:rps
```

M7 endpoint 侧 bucket：

```text
rl:endpoint:<endpoint_id>:<dim>
```

endpoint quota 不暴露给客户端，只用于保护上游容量。

## 7. TPM 后扣

不再使用请求体估算 TPM：

- 不读 `max_tokens`。
- 不使用全局默认输出 token。
- 不做 `input_chars / 4 + max_tokens` 预扣。
- 不因为估算过大而提前拒绝请求。

M7 thin adapter 把 `dispatch.Outcome.Usage` 写回 `rc.Usage` 后，M6 post-side 按真实值写入用户侧 TPM：

```text
usage = rc.Usage
cost = usage.Total
ChargeBatch(TPM buckets, cost)
```

如果 `usage == nil`，本次请求不扣 TPM bucket。该情况应通过 usage extractor / translator 覆盖率逐步减少。

TPM 后扣的取舍：

- 优点：不会因预估过大错杀正常请求；实现更简单；不依赖客户端 `max_tokens`。
- 缺点：并发高时可能超出 TPM 上限，且超限不自动阻断后续请求；这是明确取舍，计费仍以 usage outbox 为准。

如果 `ChargeBatch` 发现写入后超过 TPM 上限，必须记录 `llm_gateway_tpm_overflow_total{layer,dimension}`，供运营观察“后扣 token 已超过配置上限”的次数。

## 8. Redis 故障行为

Redis 是限流和 cooldown 的生产依赖，启动期连不上直接 fail-fast。运行期故障按调用点区分：

| 调用点 | 默认行为 | 说明 |
|--------|----------|------|
| M6 用户侧 `ReserveBatch` | fail-closed，返回 503 + `Retry-After` | 不能确认配额时不放行，避免绕过限流 |
| M6 用户侧 `ChargeBatch` | 不改变本次响应，记录错误 metric | 请求已经完成，不能再改响应 |
| M7 endpoint `ReserveBatch` | 当前 endpoint 视为不可用，尝试下一个 endpoint；全部失败则 503 | 避免把 Redis 抖动误判为上游成功 |
| M7 endpoint `SnapshotBatch` | 不影响调度，该 endpoint 视为无可读配额信息 | read-only filter 不能成为硬依赖 |
| cooldown set/report | 不影响本次响应，记录错误 metric | cooldown 是保护机制，不是响应成功的必要条件 |

可选 fail-open 只能作为显式配置，必须打 warn log 和 `llm_gateway_ratelimit_fail_open_total`，生产默认关闭。

## 9. Headers

不返回 `X-RateLimit-*` headers。

原因：

- 当前 quota 是 account / API key / model 多桶叠加，很难用一组 header 准确表达。
- endpoint quota 是上游容量，不是用户权益，不应暴露给客户端。
- 后扣 TPM 下，请求开始前无法准确给出 token remaining。

拒绝请求时只返回：

- HTTP 429
- `Retry-After`
- 错误 body 中包含超限维度和 bucket key，便于排查

## 10. Endpoint 限流

Endpoint quota 属于 M7。

目标流程：

```text
candidates = list + eligibility filter + cooldown filter
optional: SnapshotBatch(endpoint buckets) 做 read-only 过滤
ep = weighted/scored pick

ReserveBatch(endpoint RPM/RPS buckets for selected ep)
if denied:
    Scheduler.Report(ep, capacity)
    exclude ep
    pick next endpoint

call upstream

if usage != nil:
    ChargeBatch(endpoint TPM bucket, cost = usage.Total)
```

关键约束：

- 不在 filter 阶段对所有候选 endpoint reserve。
- 只有最终要尝试的 endpoint 才能扣 endpoint RPM/RPS。
- endpoint TPM 也按真实 usage 后扣。
- read-only 过滤只能使用 `SnapshotBatch`。

## 11. PolicyCache

`ratelimit.PolicyCache` 包装 middleware 定义的 `QuotaPolicyReader`：

- 默认 TTL 30 秒。
- 缓存预解析后的 `PolicyRule`。
- policy 不存在返回 `nil, nil`，表示该层不限。
- SQL 改 policy 后通过被动 TTL 传播：缓存项 30s 后自然过期重新加载。data plane 不
  设主动 invalidate 通道；业务表变更不需要秒级生效（详见
  [06 §8](./06-pluggable-infra.md#8-repo-缓存deployer-sql--gateway-数据传播)）。

## 12. 演进规则

- 修改 quota JSON schema 时，同步更新 domain/ratelimit quota 类型、repo row/mapper、示例配置和本文档。
- 新增限流维度时，必须定义 bucket key 规则、reserve cost 规则和拒绝语义。
- 不引入内存 Store 作为生产兜底；多副本 gateway 必须共享 Redis 计数器。
- endpoint quota 不允许在候选 filter 阶段产生扣减副作用。
- RPM/RPS 前扣使用 `ReserveBatch`；TPM 后扣使用 `ChargeBatch`，不能用 reserve 语义吞掉真实用量。
- TPM 不做预扣；任何重新引入 TPM 预估的方案都必须说明错杀风险和回滚策略。

[English](04-rate-limiting.md) | [简体中文](04-rate-limiting.zh-CN.md)

# 04 — 速率限制

本文档记录了速率限制的设计目标。速率限制分为两类：

1. **用户端配额**：账户/API key/模型维度，由M6处理。
2. **端点配额**：上游端点自身的容量，选择端点后由M7处理。

限速桶只起到流量控制的作用，不起到计费账本的作用。计费基于Outbox的用量。

## 1. 设计原则

- M6仅处理用户侧配额；它不混合端点配额。
- M7只为最终选择的端点预留端点配额；它不会在候选过滤阶段从所有端点中扣除。
- 用户侧RPM/RPS在请求之前保留。
- 请求前不预先扣除TPM，不读取`max_tokens`，仅根据实际用量在响应后扣除；它是一个事后计数器，不保证请求开始之前的令牌上限。
- 不返回 `X-RateLimit-*` 标头；速率限制状态不会作为客户端合约公开。
- Redis是生产中唯一的Store；多副本网关必须共享计数器。

## 2. M6用户侧限速

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

usage = rc.Usage  # M7 thin adapter writes back from dispatch.Outcome; nil means usage was not extracted
if usage != nil:
    build user TPM buckets by additive policy
    ChargeBatch(TPM buckets with cost = usage.Total)
```

RPM / RPS 是预先扣除的：成本固定为 1，在请求开始之前已知，用于阻止请求激增。

**预扣款不予退还（明确的权衡）**：RPM/RPS 预留是“漏斗计数”而不是“预留配额”——即使请求随后在网关侧失败（调度 503/所有上游下行/M8 审核拒绝），已预留的 1 个时隙**不会回滚**。原因：（1）滑动窗口计数器在窗口长度内的计数会自然过期，因此超计数是有界的并且具有自愈性； (2) 补偿性退款需要在中间件 + 调度的多层中精确地对每个故障路径上的预留/退款进行配对，包括恐慌恢复——配对错误反而会导致双倍退款，从而突破速率限制； (3) 速率限制的语义本质上是“进入系统的请求速率”而不是“成功响应的速率”——客户端不断达到 503 消耗 RPM 配额是预期行为，因为它可以防止该客户端免费重试泛洪。在计费需要基于成功的情况下，使用Outbox才是权威的，而不是速率限制桶。端点侧 RPM/RPS (§10) 同样不会退款。

TPM是事后扣除的：只有在请求完成后才知道真正的令牌计数，因此不进行预先保留。 TPM 扣除后失败不会更改当前响应。用户端`ReserveBatch`默认不读取TPM桶，所以超过TPM限制本身并不会拒绝后续请求；它用于事后观察、报告和操作警报。需要硬令牌上限的企业应配置更严格的 RPM/RPS 或单独引入显式 TPM 软检查方案。

## 3. 数据来源

身份来自M2：

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

配额策略来自SQL表`quota_policies`，其`rule_json`解析为：

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

## 4. 附加语义

`PolicyRule.PickRulesAdditive(model)` 返回两者：

- 默认规则，范围 `*`
- 匹配的每个模型规则，范围是当前模型

当两者都存在时，两者会同时被消耗。这样，每个模型就充当了默认的子限制，避免了“匹配每个模型绕过总体上限”的情况。

账户层和API key层也是同时消费的；两层策略相互独立并相加应用。对于 RPM/RPS 等预先扣除的桶，如果其中任何一个超过限制，则整个批次将被拒绝； TPM桶是扣后计数器，不参与预请求拒绝。

## 5.Redis存储接口

目标接口：

```go
type Store interface {
    ReserveBatch(ctx context.Context, buckets []Bucket) (allowed bool, violated *BucketViolation, err error)
    ChargeBatch(ctx context.Context, buckets []Bucket) ([]BucketChargeResult, error)
    SnapshotBatch(ctx context.Context, buckets []Bucket) ([]BucketState, error)
}
```

`ReserveBatch`用于预请求限速；它是一个多键原子全有或全无操作 - 如果任何存储桶超出其限制，则不会写入整个批次，并且调用者返回 429 或切换端点。该算法使用滑动窗口计数器，避免固定窗口边界处的 2x 突发。

`ChargeBatch`用于事后记账，必须写实际用途；即使写入结果超出限制，也不能拒绝已经完成的响应。返回值可以标记哪些存储桶已经超出限制，以用于日志记录、指标和操作警报。

`SnapshotBatch` 是只读的，用于后续端点配额/可观察性场景读取当前状态，无任何推演副作用。

**驱动程序选择**（`rate_limit.driver`，请参阅[07 §2](./07-configuration.zh-CN.md#2-gatewayyaml)）：`redis`（默认）通过Lua脚本在整个舰队范围内共享计数器，并且是唯一正确的选择多副本部署； `inmemory` 使用进程本地计数器实现相同的滑动窗口语义 - 然后对每个副本强制执行限制，因此它仅适用于单副本部署、本地开发和测试。两者实现相同的`ratelimit.Store`接口；切换驱动程序永远不会改变限制数学，只会改变计数器范围。

## 6. 存储桶命名

M6用户侧桶：

```text
rl:quota:<layer>:<subject>:<scope>:<dim>
```

字段含义：

- `layer`：`account` 或 `apikey`
- `subject`：主账户 pin 或 api_key_id
- `scope`：`*`或实际型号
- `dim`：`rpm`、`tpm`、`rps`

示例：

```text
rl:quota:account:default:*:rpm
rl:quota:account:default:gpt-4o:tpm
rl:quota:apikey:ak_alice:*:rps
```

M7端点侧桶：

```text
rl:endpoint:<endpoint_id>:<dim>
```

端点配额不暴露给客户端；它仅用于保护上游容量。

## 7. TPM 扣除后

不再使用从请求正文估计 TPM：

- `max_tokens` 未读取。
- 不使用全局默认输出令牌计数。
- `input_chars / 4 + max_tokens` 未进行预扣减。
- 请求不会因为估算太大而被提前拒绝。

M7瘦适配器将`dispatch.Outcome.Usage`写回`rc.Usage`后，M6的后侧使用真实值写入用户侧TPM：

```text
usage = rc.Usage
cost = usage.Total
ChargeBatch(TPM buckets, cost)
```

如果为 `usage == nil`，则不会为此请求扣除 TPM 存储桶。这种情况应该通过使用提取器/转换器覆盖来逐渐减少。

TPM扣除后的权衡：

- 优点：正常请求不会因为高估而被错误阻止；实现更简单；它不依赖于客户端的`max_tokens`。
- 缺点：高并发下可能会超出TPM限制，超过限制不会自动阻塞后续请求；这是一个明确的权衡——计费仍然基于Outbox的用量。

如果`ChargeBatch`发现写入导致超出TPM限制，则必须记录`llm_gateway_tpm_overflow_total{layer,dimension}`，以便操作观察“扣除后的令牌超出配置限制”的次数。

<a id="7a-redis-deployment-shape-limitations"></a>
## 7a。 Redis 部署形态限制

限速脚本是一个**多键EVAL，没有哈希标签**（`rl:quota:account:*`和`rl:quota:apikey:*`
落在不同的槽上）— **与 Redis Cluster 不兼容**；切换到它会导致第一批请求遇到 CROSSSLOT 错误，
M6 的故障关闭行为会将其放大为全面的 503。支持的部署结构：单实例/主副本 + Sentinel/
聚合的代理层（例如 Twemproxy 不起作用——只有支持跨键 EVAL 的代理才起作用）。实际迁移到集群
您需要首先将 `{account}` 哈希标签引入到存储桶键中，并按主题进行批量 EVAL - 这被记录为已知
演进项目；在此之前不要指向 Cluster。

此外，该脚本使用网关的本地时钟作为窗口边界（`ARGV = time.Now().Unix()`）：副本之间的时钟偏差
将导致 ≤ skew/window 范围内的过度接纳 — 对于 NTP 同步队列来说可以忽略不计。

## 8.Redis 故障行为

Redis 是一种用于速率限制和冷却的生产依赖项；如果启动时无法连接，则很快就会失败。根据调用站点的不同，运行时故障的处理方式也不同：

|致电网站 |默认行为 |笔记|
|--------|----------|------|
| M6用户侧`ReserveBatch` |失败关闭，返回 503 + `Retry-After` |无法确认配额时，不承认，避免绕过限速 |
| M6用户侧`ChargeBatch` |不更改当前响应，记录错误指标 |请求已完成，响应无法再更改 |
| M7端点`ReserveBatch` |当前端点被视为不可用，尝试下一个端点；如果全部失败则 503 |避免将 Redis 抖动误认为上游成功 |
| M7端点`SnapshotBatch` |不影响调度；端点被视为没有可读的配额信息 |只读过滤器一定不能成为硬依赖项 |
|冷却时间设置/报告|不影响当前响应，记录错误指标 |冷却时间是一种保护机制，并非成功应对的先决条件 |

可选的故障打开模式只能是显式配置；它必须发出警告日志和 `llm_gateway_ratelimit_fail_open_total`，并且在生产中默认关闭。

## 9. 标题

不返回 `X-RateLimit-*` 标头。

理由：

- 目前的配额是跨账户/API密钥/模型的多个桶的堆栈，很难用一组标头准确表达。
- 端点配额是上游容量，而不是用户权利，不应向客户端公开。
- 在扣除后的 TPM 下，无法准确给出请求开始前剩余的令牌。

拒绝请求时，仅返回以下内容：

- HTTP 429
- `Retry-After`
- 包含超出尺寸和桶键的错误主体，用于故障排除

## 10. 端点速率限制

端点配额属于M7。

目标流量：

```text
candidates = list + eligibility filter + cooldown filter
optional: SnapshotBatch(endpoint buckets) for read-only filtering
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

主要限制：

- 在过滤阶段不要为所有候选端点保留。
- 只有最终尝试的端点可以扣除端点RPM/RPS。
- 端点TPM也会根据实际用量进行事后扣除。
- 只读过滤只能使用`SnapshotBatch`。
- **保留释放**：如果尝试失败*在联系端点之前*
  （处理器查找未命中/调用构造失败），调度程序回滚
  通过 `Store.ReleaseBatch` 进行保留，因此配置差距不会默默地节流
  一个健康的终点。真正的上游响应 - 包括 429/5xx - 保持
  保留：我们确实向端点发送了请求，并进行了自我限制
  它的拒绝是预期的行为。 `ReleaseBatch` 减少当前值
  窗口（仅用于快速失败的路径，因此窗口边界漂移是
  可以忽略不计并且总是对调用者有利）。

## 11. 策略缓存

`ratelimit.PolicyCache` 包装了中间件定义的 `QuotaPolicyReader`：

- 默认 TTL 为 30 秒。
- 缓存预解析的`PolicyRule`。
- 如果该策略不存在，则返回 `nil, nil`，表示该层不受限制。
- SQL 更改策略后，通过被动 TTL 进行传播：缓存项在 30 秒后自然过期，并且
  重新加载。数据平面确实
  没有有效的失效通道；对业务表的更改不需要在几秒钟内生效（请参阅
  [06 §8](./06-pluggable-infra.zh-CN.md#8-repo-cache-deployer-sql--gateway-data-propagation) 了解详细信息）。

## 12.演进规则

- 修改配额 JSON 架构时，同步更新域/速率限制配额类型、存储库行/映射器、示例配置和本文档。
- 添加新的速率限制维度时，必须定义存储桶键规则、保留成本规则和拒绝语义。
- 不要引入内存存储作为生产回退；多副本网关必须共享 Redis 计数器。
- 端点配额在候选过滤阶段不得产生扣除副作用。
- RPM/RPS预扣使用`ReserveBatch`； TPM 推导后使用 `ChargeBatch` — 保留语义不得用于吞没真实用量。
- TPM不做预扣；任何重新引入TPM估计的方案都必须解释错误阻止请求的风险和回滚策略。

[English](09-routing-policy.md) | [简体中文](09-routing-policy.zh-CN.md)

# 可解释的虚拟模型路由

虚拟模型路由是调度之前的模型链解析阶段。一个
调用者请求一个稳定的名称，例如 `fast-chat`；网关解析一个
不可变的策略快照，评估确定性约束并提供
已订购的具体 `ModelService` 链到现有调度程序。

## 运行时边界

```text
request model
     |
     +-- concrete catalog hit --> subscription --> existing fallback header
     |
     `-- catalog miss --> effective routing policy --> constraints/subscriptions
                                                  |
                                                  `--> dispatch model chain
```

`internal/routingpolicy` 从不选择端点或调用上游。
`internal/dispatch` 仍然是唯一的重试/回退执行循环，并且
`internal/selector` 仍然一次为一个具体模型选择端点。
在每次尝试之前，dispatch 都会将请求信封的顶级 `model` 重写为
目前的具体候选项；面向调用方的虚拟名称保留在
模型路由决策和使用元数据。

## 策略形态

策略是存储在 `routing_policies.rule_json` 中的不可变版本：

```json
{
  "max_attempts": 2,
  "constraints": {
    "regions": ["cn-north"],
    "modalities": ["chat"],
    "allow_models": ["gpt-4o-mini", "local-llama"],
    "deny_models": []
  },
  "objectives": {
    "latency_weight": 2,
    "cost_weight": 1,
    "target_latency_ms": 500,
    "target_cost_microusd": 500,
    "estimated_input_tokens": 1000,
    "estimated_output_tokens": 500,
    "min_telemetry_samples": 5,
    "telemetry_max_age_seconds": 300,
    "exploration_permille": 50
  },
  "candidates": [
    {"model": "gpt-4o-mini", "weight": 100},
    {"model": "local-llama", "weight": 50, "regions": ["cn-north"]}
  ]
}
```

解析器在一个 SQL 中应用项目、账户、然后全局优先级
查询。项目策略会被控制台拒绝，直到经过身份验证并
RBAC 提供可信的项目身份。范围策略是完整的快照
并且不合并。

拒绝战胜允许。策略范围和候选项特定的区域/模式规则
相交。目录存在和账户订阅是强制性的。
如果没有目标，合格的候选项将按权重降序排列
配置顺序作为确定性的决胜局。 `max_attempts` 只能
收紧网关的全局尝试上限。

## 延迟和成本目标

硬约束、目录存在和账户订阅始终运行
优化前。客观评分不能使候选项不合格
有资格。对于每个符合条件的候选者，解析器计算有界分数：

```text
latency_score = clamp(target_latency / observed_latency, 0, 1) * success_rate
cost_score    = clamp(target_cost / estimated_cost, 0, 1)
total_score   = weighted_mean(latency_score, cost_score)
```

估计成本使用策略的固定输入/输出代币假设，并且
活动 `routing_cost_profiles` 快照。这些不可变的运营成本
配置文件故意与 `pricing_versions` 分开：路由从不
呼叫计费服务或评估客户定价、折扣或发票。
数据平面通过与以下相同的有界 TTL 缓存模式读取配置文件：
策略。

延迟和成功信号重用`selector.EndpointStatsStore`；路由
层为候选模型投影现有的每端点 EMA 快照
和请求组。它不会创建并行指标管道。少于
`min_telemetry_samples` 是 `missing_neutral`；遥测数据早于
`telemetry_max_age_seconds` 是 `stale_neutral`。还缺少成本配置文件
`missing_neutral`。每个中性信号得分为 `0.5`，并且在
决定，而不是默默地视为零或同类最佳。
收集这些 EMA 快照是由 `scoring.enabled` 控制的；当它是
关闭，延迟目标在成本评分时正确报告 `missing_neutral`
继续工作。多副本部署应使用 `scoring.driver: redis`
因此端点选择和模型路由都会看到相同的快照。

探索是确定性的。当请求ID散列成
`exploration_permille`，一名非领先合格候选项晋升。的
哈希值包括策略 ID/版本、账户和请求 ID，因此决策是
可从记录的输入中再现。探索从来不包括拒绝
候选项。候选权重和配置顺序保持确定性
客观得分后决胜局。

## 兼容性和失败

- 具体模型和模型别名保留其现有行为，并且不
  依赖于路由策略存储。
- `X-Gateway-Fallback-Models` 对于具体请求仍然有效。
- 虚拟请求的标头将被忽略，并且无法扩大策略。
- 缺少虚拟策略返回 `virtual_model_policy_not_found`。
- 空的合格集返回 `no_eligible_candidate`。
- 存储或格式错误的策略故障是故障关闭依赖项故障。

## 一致性和可观察性

数据平面缓存完整有效快照30秒及负数
查找5秒。控制台写入发布路由策略失效；
全局策略更改有意清除小型编译缓存，因为
它们可能会影响每个账户密钥。 TTL 是 Redis 发布/订阅时的回退
不可用。

每个请求记录一个有界的`ModelRoutingDecision`：请求的模型，
结果/原因、策略 ID/版本/范围以及接受/拒绝的候选项。
客观决策还记录信号源、观察到的延迟和
成功、样本时间戳/计数、成本配置文件 ID/版本、估计成本、
成分分数、总分以及探索是否选择了模型。
完整的决策是跟踪/审计元数据。指标
`llm_gateway_routing_decisions_total` 仅使用 `outcome`、`reason` 和
`scope_kind`；策略/账户/模型标识符不是指标标签。用途
元数据记录请求和路由模型以及策略 ID/版本/原因。

## 控制台API

- `GET /admin/routing-policies` 列出所有版本。
- `POST /admin/routing-policies` 验证并发布新的活动版本。
- `DELETE /admin/routing-policies/:policyID` 禁用活动版本。
- `POST /admin/routing-policies/dry-run` 评估综合账户、区域、
  模态、请求模型、决策关键和可选遥测快照
  无需调度上游流量。回应比较了每个候选项
  及其完整的分数解释。
- `GET /admin/routing-costs` 列出仅路由成本配置文件版本。
- `POST /admin/routing-costs` 发布了新的不可变主动成本版本。

写入由控制台的现有管理员角色和写入审核涵盖。

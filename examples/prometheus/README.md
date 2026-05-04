# examples/prometheus — 监控告警配置

ai-gateway 自带 `/metrics` endpoint（Prometheus exposition format）；本目录给出推荐 alert / dashboard 模板。

## 文件

| 文件 | 用途 |
|---|---|
| `alerts.yaml` | Alertmanager 告警规则（按可用性 / 延迟 / 限流 / cooldown / 计费分组） |

## 接入步骤

### 1. 让 Prometheus 抓 ai-gateway metrics

`prometheus.yml` 加 scrape target：

```yaml
scrape_configs:
  - job_name: ai-gateway
    metrics_path: /metrics
    scrape_interval: 15s
    static_configs:
      - targets: ['ai-gateway.internal:8080']
        labels:
          service: ai-gateway
```

### 2. 加载 alert rules

`prometheus.yml` 同一文件：

```yaml
rule_files:
  - /etc/prometheus/rules/ai-gateway-alerts.yaml
```

把本目录 `alerts.yaml` 复制到 `/etc/prometheus/rules/`，重启 prometheus（或 `kill -HUP`）让它重读规则。

### 3. Alertmanager 路由

`alertmanager.yml` 按 severity / team label 路由：

```yaml
route:
  routes:
    - match:
        team: ai-platform
      receiver: ai-platform-pd
      continue: true
    - match:
        severity: critical
      receiver: oncall-pd
```

## Metric 命名约定

ai-gateway 用 `ai_gateway.<component>.<name>` 命名（见 `pkg/metric/names.go`）；
落 Prometheus 时 `.` → `_`，所以这里 alert 表达式都是下划线版本。

label 维度：`vendor` / `model` / `class` / `endpoint_id` / `result` / `scope`，
按 metric 不同部分会出现。看 `pkg/metric/recorder.go` 的 Inc/Observe 调用确认。

## 自定义阈值

告警阈值是经验值，按业务调：

| Alert | 默认阈值 | 何时调 |
|---|---|---|
| UpstreamHighErrorRate | 10% / 5min | 容忍度低改 5% / 容忍度高改 20% |
| HttpLatencyP99High | 5000ms | 流式长输出场景拔到 30000ms |
| RateLimitHighRejection | 20% / 5min | 已知限流频繁场景调到 50% 减噪 |
| CooldownStorm | 5 ep/sec | endpoint 池小（< 10）时降到 2 |

## Dashboard

dashboard.json 留空（不同 Grafana 版本 schema 不同；以 alerts 为基础自己 panel）。
推荐 panel 维度：

- 总 RPS（按 vendor / model 分）
- 端到端 p50 / p99 latency
- error rate 按 ErrorClass 堆叠
- endpoint cooldown 状态时间线
- usage outbox publish lag
- cost 总览（按 model）

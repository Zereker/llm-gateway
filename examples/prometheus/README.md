# examples/prometheus — Monitoring & Alerting Configuration

llm-gateway ships its own `/metrics` endpoint (Prometheus exposition format); this directory provides recommended alert / dashboard templates.

## Files

| File | Purpose |
|---|---|
| `alerts.yaml` | Alertmanager alert rules (grouped by availability / latency / rate limiting / cooldown / billing) |

## Setup Steps

### 1. Have Prometheus scrape llm-gateway metrics

Add a scrape target to `prometheus.yml`:

```yaml
scrape_configs:
  - job_name: llm-gateway
    metrics_path: /metrics
    scrape_interval: 15s
    static_configs:
      - targets: ['llm-gateway.internal:8080']
        labels:
          service: llm-gateway
```

### 2. Load alert rules

In the same `prometheus.yml` file:

```yaml
rule_files:
  - /etc/prometheus/rules/llm-gateway-alerts.yaml
```

Copy `alerts.yaml` from this directory to `/etc/prometheus/rules/`, then restart prometheus (or `kill -HUP`) so it reloads the rules.

### 3. Alertmanager routing

Route in `alertmanager.yml` by severity / team label:

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

## Metric Naming Convention

llm-gateway names metrics as `llm_gateway.<component>.<name>` (see `pkg/metric/names.go`);
when landed in Prometheus, `.` becomes `_`, so the alert expressions here all use the underscore form.

Label dimensions: `vendor` / `model` / `class` / `endpoint_id` / `result` / `scope`,
which appear on different parts of a metric depending on context. Check the Inc/Observe calls in `pkg/metric/recorder.go` to confirm.

## Custom Thresholds

The alert thresholds are empirical values; tune them for your workload:

| Alert | Default Threshold | When to Adjust |
|---|---|---|
| UpstreamHighErrorRate | 10% / 5min | Lower to 5% for low tolerance / raise to 20% for high tolerance |
| HttpLatencyP99High | 5000ms | Raise to 30000ms for long streaming output scenarios |
| RateLimitHighRejection | 20% / 5min | Raise to 50% to reduce noise in scenarios with frequent, known rate limiting |
| CooldownStorm | 5 ep/sec | Lower to 2 when the endpoint pool is small (< 10) |

## Dashboard

dashboard.json is left empty (schema differs across Grafana versions; build your own panels based on the alerts).
Recommended panel dimensions:

- Total RPS (broken down by vendor / model)
- End-to-end p50 / p99 latency
- Error rate stacked by ErrorClass
- Endpoint cooldown status timeline
- Usage outbox publish lag
- Cost overview (by model)

# examples/prometheus — Monitoring & Alerting Configuration

llm-gateway ships a Prometheus `/metrics` endpoint. This directory contains a
provisioned dashboard, alert rules, and the scrape configuration used by the demo.

## Files

| File | Purpose |
|---|---|
| `alerts.yaml` | Alertmanager alert rules (grouped by availability / latency / rate limiting / cooldown / billing) |
| `dashboard.json` | Grafana dashboard for traffic, latency, TTFT, routing, token usage, and delivery health |
| `prometheus.yml` | Demo scrape and rule configuration |
| `grafana/provisioning/` | Auto-provisioned Prometheus datasource and dashboard provider |

## Run the complete stack

From the repository root:

```sh
make -C examples/demo observe
```

Open Prometheus at `http://localhost:9091` and Grafana at
`http://localhost:3000` (`admin` / `admin`). The demo pins its Prometheus and
Grafana images in `examples/demo/compose.yaml` for reproducibility.

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

Metric names use the Prometheus-native `llm_gateway_<component>_<name>` form
(see `internal/metric/names.go`).

Label dimensions: `vendor` / `model` / `class` / `endpoint_id` / `result` / `scope`,
which appear on different parts of a metric depending on context. Check the Inc/Observe calls in `internal/metric/recorder.go` to confirm.

## Custom Thresholds

The alert thresholds are empirical values; tune them for your workload:

| Alert | Default Threshold | When to Adjust |
|---|---|---|
| UpstreamHighErrorRate | 10% / 5min | Lower to 5% for low tolerance / raise to 20% for high tolerance |
| HttpLatencyP99High | 5s | Raise to 30s for long streaming output scenarios |
| RateLimitHighRejection | 20% / 5min | Raise to 50% to reduce noise in scenarios with frequent, known rate limiting |
| CooldownStorm | 5 ep/sec | Lower to 2 when the endpoint pool is small (< 10) |

Cost is intentionally not computed in Prometheus: pricing is mutable business
data and belongs in the metering pipeline. The dashboard focuses on operational
signals with bounded labels; do not add API keys, account IDs, request IDs, or
raw model input as metric labels.

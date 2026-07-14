# Reference Performance Baseline

✅ Regression check passed.

**Measured environment:** `linux/arm64` · Go go1.25.12 · 4 CPUs · concurrency 20

Commit `e28cd6296263` · generated 2026-07-14T09:51:26Z

## Gateway overhead

| Mode | Throughput | p95 latency | p95 TTFB |
|---|---:|---:|---:|
| nonstream | -15.798% | +7.734 ms | +7.147 ms |
| stream | -2.004% | +4.274 ms | +2.703 ms |

## Gateway resources

| Mode | Requests/s | p95 latency | Active streams | CPU | Peak RSS | Peak Go heap |
|---|---:|---:|---:|---:|---:|---:|
| nonstream | 790.907 | 29.621 ms | 0 | 0.120 s | 42.4 MiB | 11.8 MiB |
| stream | 140.765 | 146.345 ms | 20 | 0.320 s | 44.1 MiB | 12.8 MiB |

## Resilience

- ✅ `client_disconnect_cancels`
- ✅ `mid_stream_failure_detected`
- ✅ `slow_client_completes`

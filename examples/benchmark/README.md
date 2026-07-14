# Reproducible gateway benchmark

The benchmark compares the same deterministic LLM-like upstream directly and
through `llm-gateway`. The runner and upstream are versioned Go source in this
directory; no host load-generator version or real provider is involved.
Registry and Go proxy overrides are documented in the shared
[examples guide](../README.md#registry-configuration).

```sh
make benchmark
make benchmark-down
```

Each run prints a JSON report containing environment metadata and, for direct
and gateway paths:

- non-streaming and streaming throughput;
- p50/p95/p99 end-to-end latency;
- p50/p95/p99 time to first byte;
- error count and mean response bytes;
- slow-client completion, client cancellation, and upstream mid-stream failure
  checks.

The deterministic upstream waits 20 ms for non-streaming responses. Streams
wait 50 ms before the first chunk, then produce eight chunks 10 ms apart. This
makes the gateway delta visible independently of provider variance.

## Publishing a baseline

Capture the JSON together with the commit SHA, Docker/host CPU limits, and an
idle-machine note. Compare gateway-minus-direct values from the same run; do
not compare raw gateway latency between dissimilar hosts. A regression gate
should be introduced only after several CI runs establish variance bounds.

This first harness reports client-observed behavior. Container CPU and memory
should be captured with `docker stats` alongside a published result until the
project has a stable CI runner allocation.

# Reproducible gateway benchmark

**[View the rendered reference baseline →](baselines/README.md)**

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
- error count, error rate, mean response bytes, and peak active requests/streams;
- gateway-minus-direct throughput, latency, and first-byte overhead;
- Gateway CPU seconds, peak resident memory, and peak Go heap sampled from its
  Prometheus process metrics;
- slow-client completion, client cancellation, and upstream mid-stream failure
  checks.

The report is printed and written to `results/latest.json`, with a rendered
summary in `results/summary.md`. `make benchmark` then compares it with the
[machine-readable reference](baselines/reference.json). The versioned
[baseline summary](baselines/README.md) displays the same data on GitHub. Each
report records the measured runtime environment (`GOOS`, `GOARCH`, Go version,
available CPUs, and the configured concurrency); these values are not
hard-coded. A versioned baseline therefore retains the environment of the run
that produced it.
Any request error or resilience failure is fatal. The default performance gate
allows the larger of 20 ms or 100% of the baseline p95 overhead, plus 40
percentage points of throughput movement. This is intentionally tolerant of
shared CI runners.

The deterministic upstream waits 20 ms for non-streaming responses. Streams
wait 50 ms before the first chunk, then produce eight chunks 10 ms apart. This
makes the gateway delta visible independently of provider variance.

## Publishing a baseline

Use the lower-level targets when investigating or intentionally updating the
reference:

```sh
make -C examples/benchmark measure   # write results/latest.json
make -C examples/benchmark check     # compare with the checked-in baseline
make -C examples/benchmark baseline  # deliberately replace the baseline
```

The checked-in reference records its commit, Go version, architecture, CPU
count, concurrency, request count, and deterministic upstream delays. Compare
gateway-minus-direct values from the same run; raw latency and resource values
from dissimilar hosts are diagnostic rather than directly comparable. Tighten
the comparator flags only after a stable runner has established narrower
variance bounds.

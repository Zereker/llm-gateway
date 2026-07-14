# Examples

Each directory owns one runnable scenario and keeps its Dockerfile, Compose file,
configuration, and lifecycle commands together.

| Directory | Purpose |
|---|---|
| [`quickstart/`](quickstart/) | Self-contained product evaluation with an optional observability stack |
| [`local/`](local/) | MySQL, Redis, and Redpanda for host-based development |
| [`benchmark/`](benchmark/) | Reproducible direct-versus-gateway performance comparison |
| [`full-config/`](full-config/) | Production-shaped gateway configuration reference |
| `support/` | Shared test upstreams, seed programs, and black-box test helpers |

## Registry configuration

Runnable examples use Docker Hub by default, but do not require it. Override the
image registry, base-image registry, and Go module proxy independently. For
example, from a network in mainland China:

```sh
CONTAINER_REGISTRY=docker.m.daocloud.io \
BASE_IMAGE_REGISTRY=docker.m.daocloud.io/library \
GOPROXY=https://goproxy.cn,direct \
make -C examples/quickstart observe
```

The same variables work with the local and benchmark scenarios. The registry
must expose Docker Hub-compatible repository paths. A mirror operated by your
organization can be used instead of the example above.

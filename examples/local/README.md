# Local development environment

`examples/local/` owns the infrastructure and configuration used when Go processes run on
the host. It is not a deployment bundle and does not contain application images.

```text
examples/local/
├── compose.yaml       # MySQL, Redis, and Redpanda only
├── configs/           # gateway and console host-process configs
└── mysql-init/        # test database initialization
```

From the repository root:

```sh
make dev-up
make run-gateway
make run-console
make dev-clean
```

Gateway startup applies the versioned schema in `internal/infra` before serving.
MySQL and Redis are required; Redpanda is required only for Kafka-backed usage
delivery. Credentials in this directory are development-only.

Production templates and deployment assets live under [`deploy/`](../../deploy/).
The self-contained evaluator experience lives under
[`examples/quickstart/`](../quickstart/).
Registry and Go proxy overrides are documented in the shared
[examples guide](../README.md#registry-configuration).

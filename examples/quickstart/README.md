# Self-contained quickstart

This directory owns every quickstart-specific build and runtime asset. It does not
change the repository's production `Dockerfile` or root `Makefile`.

```sh
make up       # core stack and a verified gateway request
make observe  # core stack plus Prometheus and Grafana
make logs
make down
```

The stack uses fixed development credentials and the repository's cassette
fixtures; it never calls a real model provider. See the root README for the API
key and request example.

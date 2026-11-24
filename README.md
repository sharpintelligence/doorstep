# doorstep

A homelab deployment engine for Argo CD: queues and locks deploys, writes image digests to your GitOps repo, gates on health and metrics, and rolls back.

## Status

Early development. The binary currently provides `serve` (HTTP server with health endpoints) and `version`; the deployment engine is being built.

## Quickstart

```sh
mise install
just check
just build
bin/doorstep serve
curl localhost:8080/healthz
```

## Documentation

- [Architecture](docs/architecture.md)
- [Configuration](docs/configuration.md)
- [Development](docs/development.md)

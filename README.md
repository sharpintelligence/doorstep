# doorstep

A homelab deployment engine for Argo CD: queues and locks deploys, writes image digests to your GitOps repo, gates on health and metrics, and rolls back.

## Status

Early development. The binary currently provides `serve` (HTTP server with health endpoints, backed by Postgres), `migrate` and `version`; the deployment engine is being built.

## Quickstart

doorstep needs Postgres 15 or newer:

```sh
mise install
just check
just build
docker run -d --rm --name doorstep-pg -e POSTGRES_PASSWORD=doorstep -p 127.0.0.1:5432:5432 postgres:18-alpine
export DOORSTEP_DATABASE_URL='postgres://postgres:doorstep@127.0.0.1:5432/postgres?sslmode=disable'
bin/doorstep serve
curl localhost:8080/readyz
```

## Container image

Release images are published to `ghcr.io/sharpintelligence/doorstep` for `linux/amd64` and `linux/arm64`. Pull and deploy by digest, which each GitHub Release lists, rather than by tag:

```sh
docker pull ghcr.io/sharpintelligence/doorstep@sha256:<digest>
```

Images are signed and attested; see [Verifying an image](docs/releasing.md#verifying-an-image).

## Documentation

- [Architecture](docs/architecture.md)
- [Configuration](docs/configuration.md)
- [Database](docs/database.md)
- [GitHub App](docs/github-app.md)
- [Metrics](docs/metrics.md)
- [Development](docs/development.md)
- [Releasing](docs/releasing.md)
- [Decisions](docs/decisions/README.md)

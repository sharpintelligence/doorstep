# doorstep

A homelab deployment engine for Argo CD: queues and locks deploys, writes image digests to your GitOps repo, gates on health and metrics, and rolls back.

## Status

Early development. `doorstep serve` runs the deployment engine, the JSON API and a server-rendered web UI (apps, queue, locks, deploy history with lineage, gate results, deploy, cancel, rollback, lock and token administration) behind OIDC or GitHub sign-in, backed by Postgres. The CLI talks to the API: `deploy`, `cancel`, `rollback`, `block`, `unblock`, `blocks`, `lock`, `unlock` and `locks`; `migrate`, `token`, `session` and `config validate` work against the database or the configuration file directly.

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

## Install

To run doorstep on Kubernetes, use the kustomize base and example overlay described in [docs/install.md](docs/install.md).

## Container image

Release images are published to `ghcr.io/sharpintelligence/doorstep` for `linux/amd64` and `linux/arm64`. Pull and deploy by digest, which each GitHub Release lists, rather than by tag:

```sh
docker pull ghcr.io/sharpintelligence/doorstep@sha256:<digest>
```

Images are signed and attested; see [Verifying an image](docs/releasing.md#verifying-an-image).

## Documentation

- [Architecture](docs/architecture.md)
- [Installing on Kubernetes](docs/install.md)
- [Configuration](docs/configuration.md)
- [Authentication and roles](docs/authentication.md)
- [Database](docs/database.md)
- [HTTP API](docs/api.md)
- [Web UI](docs/ui.md)
- [Deployment engine](docs/engine.md)
- [Deploy queue](docs/queue.md)
- [GitHub App](docs/github-app.md)
- [Locks](docs/locks.md)
- [Rollback and blocks](docs/rollback.md)
- [Notifications](docs/notifications.md)
- [Metrics](docs/metrics.md)
- [Development](docs/development.md)
- [Releasing](docs/releasing.md)
- [Decisions](docs/decisions/README.md)

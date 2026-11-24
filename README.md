# doorstep

A homelab deployment engine for Argo CD: queues and locks deploys, writes image digests to your GitOps repo, gates on health and metrics, and rolls back.

## The problem

Small teams and homelabs that run Argo CD usually deploy by editing an image pin in a GitOps repository by hand, or by letting CI push to it. Nothing serialises two deploys of the same app, waits for CI to go green, checks that the new version is healthy before calling it done, or rolls back when it is not. Image tags are mutable, so what runs in the cluster is hard to trace back to what was tested.

## How it works

doorstep sits upstream of Argo CD. It observes merges and CI results on GitHub, queues each commit per app and environment, resolves the image tag to an immutable digest and commits that digest to your GitOps repository. Git is the only thing doorstep writes: Argo CD stays the only thing that applies manifests, and doorstep only reads its `Application` resources (plus pods, if you opt in to retry diagnostics). It waits for the Application to sync and become healthy, evaluates gates, and rolls back by re-pinning the previous digest.

```mermaid
flowchart TD
    merge["Merge + green CI"] --> intake["Intake: webhook or poll"]
    intake --> queue["Queue + locks: one deploy per app"]
    queue --> before["Before gates"]
    before -->|fail| failed(["Failed"])
    before -->|pass| resolve["Resolve tag to digest"]
    resolve --> pin["Commit the pin to the GitOps repo"]
    pin --> sync["Argo CD syncs the pin"]
    sync -->|not healthy, transient| sync
    sync --> after["After gates"]
    after -->|pass| succeeded(["Succeeded"])
    after -->|fail| failed
    failed -.->|optional| rollback["Automatic rollback: re-pin the previous digest"]
```

## Features

- Intake from [GitHub App webhooks and a polling safety net](docs/github-app.md), waiting for the required checks ([architecture](docs/architecture.md#merge-and-ci-detection)).
- Tag to digest resolution against any OCI registry ([architecture](docs/architecture.md#package-map)).
- A [per-app queue](docs/queue.md) with batching and urgent deploys, and [freeze locks](docs/locks.md) per app or global.
- A [pin writer](docs/gitops.md) for kustomize `images` or a YAML path, committing directly or through a pull request.
- A read-only [Argo CD watcher](docs/argocd.md).
- [Gates](docs/gates.md) before and after a deploy (checks, lock, schedule, health, soak, Prometheus query, validation Job, external, backup), mirrored to GitHub commit statuses.
- [Retry before rollback](docs/engine.md#retry-before-rollback) to tell transient failures from bad versions.
- Manual and opt-in automatic [rollback and blocks](docs/rollback.md), with [migration-aware releases](docs/rollback.md#migration-aware-releases).
- [Notifications](docs/notifications.md) through Slack, webhooks and GitHub deployments.
- [OIDC or GitHub sign-in, API tokens, roles and an audit log](docs/authentication.md).
- A [JSON API](docs/api.md), a [CLI](docs/configuration.md#doorstep-deployments-and-deployment) for deploys, history, rollbacks, blocks and locks, and a [server-rendered web UI](docs/ui.md).
- [Prometheus metrics](docs/metrics.md).
- A [kustomize install](docs/install.md) and [signed, attested multi-arch images](docs/releasing.md#verifying-an-image).

## Quick start

### Locally

doorstep needs Postgres 15 or newer.

```sh
mise install
just build
docker run -d --rm --name doorstep-pg -e POSTGRES_PASSWORD=doorstep -p 127.0.0.1:5432:5432 postgres:18-alpine
export DOORSTEP_DATABASE_URL='postgres://postgres:doorstep@127.0.0.1:5432/postgres?sslmode=disable'
bin/doorstep serve
```

`serve` applies pending migrations and listens on `:8080`. From another shell with the same `DOORSTEP_DATABASE_URL`:

```sh
curl localhost:8080/readyz
bin/doorstep token create --name dev --role admin
```

This gives you a running, empty instance and an API token. To add apps, `serve` also needs a GitHub App and a Kubernetes connection to read Argo CD; it refuses to start with apps configured but without them. See [configuration](docs/configuration.md) and [GitHub App](docs/github-app.md).

### On Kubernetes

The full walkthrough is in [docs/install.md](docs/install.md). In short:

1. Create the namespace and the Secrets (database URL, GitHub App private key, webhook secret).
2. Copy `deploy/kustomize/examples/homelab` into your GitOps repository and edit its `config.yaml`.
3. Replace the example image digest in its `kustomization.yaml` with a released digest.
4. Run `kubectl apply -k .`

## Container image

Images are published to `ghcr.io/sharpintelligence/doorstep` for `linux/amd64` and `linux/arm64` when a release is tagged. Pull and deploy by digest, which each GitHub Release lists, rather than by tag:

```sh
docker pull ghcr.io/sharpintelligence/doorstep@sha256:<digest>
```

Images are signed and attested; see [Verifying an image](docs/releasing.md#verifying-an-image).

## Documentation

Get started:

- [Installing on Kubernetes](docs/install.md)
- [MVP walkthrough](docs/mvp-walkthrough.md)
- [Configuration](docs/configuration.md)
- [GitHub App](docs/github-app.md)
- [Authentication and roles](docs/authentication.md)

Concepts:

- [Architecture](docs/architecture.md)
- [Deployment engine](docs/engine.md)
- [Deploy queue](docs/queue.md)
- [Locks](docs/locks.md)
- [Gates](docs/gates.md)
- [GitOps pin writer](docs/gitops.md)
- [Argo CD watcher](docs/argocd.md)
- [Rollback and blocks](docs/rollback.md)
- [Notifications](docs/notifications.md)

Reference:

- [HTTP API](docs/api.md)
- [Web UI](docs/ui.md)
- [Metrics](docs/metrics.md)
- [Database](docs/database.md)

Project:

- [Development](docs/development.md)
- [Releasing](docs/releasing.md)
- [Decisions](docs/decisions/README.md)
- [Contributing](CONTRIBUTING.md)
- [Security policy](SECURITY.md)
- [Code of conduct](CODE_OF_CONDUCT.md)

## Status

doorstep is feature-complete for v1 but pre-1.0: configuration and the API may still change in breaking ways before 1.0. It is maintained by one person and aimed at homelab-scale use. No release has been tagged yet.

## Contributing and security

See [CONTRIBUTING.md](CONTRIBUTING.md). Report vulnerabilities privately as described in [SECURITY.md](SECURITY.md); this project follows the [code of conduct](CODE_OF_CONDUCT.md).

## License

Apache-2.0. See [LICENSE](LICENSE).

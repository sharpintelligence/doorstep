# Architecture

This document describes how doorstep is put together: what it does, how a deployment flows through the system, how the Go packages are laid out and allowed to depend on each other, and which libraries it builds on. It grows as features land. Significant decisions are recorded as Architecture Decision Records under `docs/decisions`; this document is the current map, the ADRs explain why.

## Purpose and scope

doorstep is a deployment engine that sits upstream of [Argo CD](https://argo-cd.readthedocs.io/). It decides *what* gets deployed and *when*:

- it accepts deploy requests from webhooks, registry polling, its HTTP API and its CLI;
- it queues them and serialises them per application and environment, honouring manual freeze locks;
- it resolves image tags to immutable digests and commits those digests to the GitOps repository;
- it watches Argo CD until the commit is synced and healthy, evaluates gates (health, metrics, soak time) and rolls back by re-pinning the previous digest when a gate fails;
- it notifies people and records every transition in a queryable history.

doorstep never talks to workloads directly and never mutates Argo CD. Git is the only thing it writes to; Argo CD remains the only thing that applies manifests to the cluster. doorstep only observes Argo CD `Application` resources.

## Deployment flow

```
 intake (webhook | poll | API | CLI)
        │
        ▼
     queue ──► lock (one active deployment per app/environment, freeze locks)
                 │
                 ▼
     registry: resolve tag ─► digest
                 │
                 ▼
     gitops: commit pin (kustomize images | YAML path) via GitHub API
                 │
                 ▼
     argocd: wait until the Application is synced to that revision
                 │
                 ▼
     gates: health, metrics, soak
          │                 │
       pass                fail
          ▼                 ▼
     succeeded         rollback: re-pin previous digest
                            │
                            ▼
                   rolled_back | rollback_failed

 notify fires on every terminal state; history records every transition.
```

## Package map

There is one binary, `cmd/doorstep`. Everything else lives under `internal/`, so the module exposes no public Go API.

| Package | Responsibility | Status |
| --- | --- | --- |
| `cmd/doorstep` | `main`: wires OS signals, arguments, stdio and environment into `cli.Run` | built |
| `internal/cli` | cobra command tree built by a constructor; `serve` is the composition root | built |
| `internal/config` | configuration model, defaults, environment overrides and validation | built |
| `internal/logging` | builds the injected `*slog.Logger` (JSON or text, level) | built |
| `internal/buildinfo` | version from `-ldflags -X`, VCS revision/time/dirty flag and Go version from `runtime/debug` | built |
| `internal/server` | HTTP lifecycle: `http.Server` with timeouts, graceful shutdown, middleware, `/healthz`, `/readyz` and readiness checks | built |
| `internal/core` | domain types (`Deployment`, `Target`, `State`, `Event`, `ImageRef`, `Digest`, ...) and the state enum; imports nothing internal | planned |
| `internal/store` | Postgres via pgx, embedded goose migrations, repositories implementing engine ports | planned |
| `internal/queue` | durable work queue: `SELECT ... FOR UPDATE SKIP LOCKED` claims with leases and heartbeats | planned |
| `internal/locks` | per-target deployment locks and manual freeze locks | planned |
| `internal/history` | append-only transition log and its read models | planned |
| `internal/github` | GitHub App auth (RS256 JWT, cached installation tokens), small REST client, webhook signature verification | planned |
| `internal/intake` | turns GitHub webhooks, registry polls, API and CLI requests into queued deployments | planned |
| `internal/registry` | OCI tag to digest resolution | planned |
| `internal/gitops` | pin writer: kustomize `images` and YAML-path strategies, committed through the GitHub API | planned |
| `internal/argocd` | read-only `Application` watcher through the Kubernetes API | planned |
| `internal/gates` | health, metrics and soak gates | planned |
| `internal/rollback` | selects the previous good digest and re-pins it | planned |
| `internal/notify` | Slack, ntfy and generic webhook notifications | planned |
| `internal/engine` | deployment orchestration state machine; declares the ports it consumes | planned |
| `internal/api` | HTTP JSON API (`/api/v1`) handlers | planned |
| `internal/web` | server-rendered UI with `html/template` and embedded htmx | planned |
| `internal/auth` | hashed API tokens for CLI/CI, OIDC login for the UI | planned |
| `internal/metrics` | Prometheus metrics on a private registry and optional separate listener | planned |

Planned packages are created by the bead that implements them; there are no placeholder packages.

## Dependency rules

The design is ports and adapters.

- `core` imports nothing from `internal/`. Every other package may import it.
- `engine` declares the small interfaces it consumes (its ports) and depends only on `core` and those interfaces.
- Adapters (`store`, `queue`, `locks`, `history`, `registry`, `gitops`, `argocd`, `gates`, `rollback`, `notify`) implement engine ports. They never import `engine` or each other, with one exception: `gitops` and `intake` use `github`.
- `api` and `web` call `engine` for commands and `store` read models for queries. They contribute handlers; `server` owns the listener, middleware and health endpoints.
- `cli` `serve` is the only composition root: it constructs adapters, injects them into `engine`, and mounts handlers on `server`.
- No package-level mutable state, no `init()`, no `slog.SetDefault`. Dependencies, including the logger and clock, are passed through constructors. The single exception is the link-time `buildinfo.version` variable.
- `context.Context` is the first parameter of anything that does I/O or may block.
- Errors are wrapped with `%w` and inspected with `errors.Is` / `errors.As`.

## Engine ports

Illustrative signatures; the engine bead finalises them.

```go
type DeploymentStore interface {
	Create(ctx context.Context, d core.Deployment) (core.Deployment, error)
	Get(ctx context.Context, id core.DeploymentID) (core.Deployment, error)
	Transition(ctx context.Context, id core.DeploymentID, from, to core.State, ev core.Event) error
}

type Queue interface {
	Claim(ctx context.Context, workerID string, lease time.Duration) (core.Deployment, bool, error)
	Heartbeat(ctx context.Context, id core.DeploymentID, workerID string) error
	Release(ctx context.Context, id core.DeploymentID, workerID string) error
}

type Locker interface {
	Acquire(ctx context.Context, t core.Target, owner, reason string) (core.Lock, error)
	Release(ctx context.Context, t core.Target, owner string) error
	Get(ctx context.Context, t core.Target) (core.Lock, bool, error)
}

type DigestResolver interface {
	Resolve(ctx context.Context, ref core.ImageRef) (core.Digest, error)
}

type PinWriter interface {
	Pin(ctx context.Context, req core.PinRequest) (core.Commit, error)
}

type SyncWatcher interface {
	WaitSynced(ctx context.Context, t core.Target, revision string) (core.SyncStatus, error)
	Health(ctx context.Context, t core.Target) (core.Health, error)
}

type Gate interface {
	Name() string
	Evaluate(ctx context.Context, d core.Deployment) (core.GateResult, error)
}

type Notifier interface {
	Notify(ctx context.Context, ev core.Event) error
}

type Clock interface {
	Now() time.Time
}
```

`PinWriter` has two strategies: editing the `images` list of a `kustomization.yaml`, and setting a value at a YAML path (for Helm values files and plain manifests). Both preserve comments and formatting through the yaml v3 `Node` API.

## State machine and concurrency

Deployment state is persisted in Postgres:

```
queued → resolving → pinning → syncing → verifying → succeeded
                                              │
                                              └────► failed → rolling_back → rolled_back
                                                                     └─────► rollback_failed
```

Any non-terminal deployment can also become `cancelled` (operator action) or `superseded` (a newer deployment for the same target was queued before this one started pinning).

- Every transition is a compare-and-set on `(id, from_state)` and is recorded in history.
- Every step is idempotent and resumable: after a restart a worker re-claims the deployment and re-runs the current step. Pinning compares the desired digest with the file in Git before committing; watching keys on the commit SHA.
- Workers claim work with `SELECT ... FOR UPDATE SKIP LOCKED` and hold a lease column refreshed by heartbeat. An expired lease makes the row claimable again, so a crashed replica never wedges a target.
- `LISTEN/NOTIFY` is only a wake-up hint; workers also poll, so a missed notification costs latency, not correctness.
- A partial unique index enforces at most one active deployment per (application, environment).
- Manual freeze locks are rows in a `locks` table, visible and removable in the UI and CLI.

Postgres is the only stateful dependency. Running more than one replica is safe.

## Library choices

| Concern | Choice | Reason |
| --- | --- | --- |
| HTTP routing | `net/http` `ServeMux` with method and path patterns | Go 1.22+ routing covers methods and path parameters; middleware is `func(http.Handler) http.Handler` |
| Logging | `log/slog` | stdlib, structured, injectable; depguard bans other loggers |
| CLI | `github.com/spf13/cobra` | consistent help, flags and completion for many operator subcommands; built by constructors, no globals, no viper |
| Postgres | `github.com/jackc/pgx/v5` (`pgxpool`) | maintained, native `LISTEN/NOTIFY`, hand-written SQL, no ORM |
| Migrations | `github.com/pressly/goose/v3` Provider over `embed.FS` | forward-only SQL, no global state, session lock for concurrent starts; run by `doorstep migrate` and at `serve` startup |
| Config file | `go.yaml.in/yaml/v3` with `KnownFields(true)` | YAML is native to Kubernetes users; strict decoding catches typos; the Node API also serves comment-preserving GitOps edits |
| Kubernetes | `k8s.io/client-go` dynamic client and informer on `argoproj.io/v1alpha1` `Application` | avoids importing the argo-cd module and its dependency graph; only a few status fields are needed |
| Registry | `github.com/google/go-containerregistry` | `remote.Head` for tag to digest with proper registry auth |
| GitHub | hand-written REST client on `net/http`; RS256 JWT with `crypto/rsa`; HMAC with `crypto/hmac` | about six endpoints; small, auditable code instead of a large SDK |
| Metrics | `github.com/prometheus/client_golang` with a private registry | standard exposition, no global registry |
| Web UI | `html/template` and htmx vendored through `embed` | no CDN, no JavaScript build step, works offline |
| UI auth | `github.com/coreos/go-oidc` and `golang.org/x/oauth2`, plus hashed API tokens in Postgres | OIDC discovery and token verification are not hand-rolled |

## Security posture

- GitHub access is through a GitHub App: short-lived installation tokens, cached and refreshed before expiry, scoped to the repositories and permissions doorstep needs. Commits are attributed to and verified as the App.
- Pins are written through the GitHub Contents/Git Data API using the blob SHA for optimistic concurrency: no local clone, no git binary, no SSH keys, no writable volume.
- Webhook payloads are verified with HMAC-SHA256 and a constant-time comparison before they are parsed.
- Kubernetes RBAC is `get`, `list` and `watch` on Argo CD `Application` resources only. doorstep never patches or syncs Applications.
- Only image digests are pinned, never mutable tags.
- Secrets never appear inline in configuration: they come from environment variables or `*_file` paths (mounted Kubernetes Secrets).
- The HTTP server sets read-header, read, write and idle timeouts and a header size limit; panics are recovered and logged.
- CI pins every action by full commit SHA, runs with a read-only token and does not persist checkout credentials. Dependabot keeps pins and Go modules current.

## HTTP surface

One listener (default `:8080`) serves everything public:

| Path | Purpose | Status |
| --- | --- | --- |
| `GET /healthz` | liveness: always `200 {"status":"ok"}` while the process serves | built |
| `GET /readyz` | readiness: runs registered named checks (2 s timeout each, concurrently, results cached for 1 s); `200` when all pass, `503` when any fails, with each check reported only as `ok` or `fail`; `503 {"status":"shutting_down"}` once shutdown begins | built |
| `/api/v1/...` | JSON API | planned |
| `/webhooks/github` | GitHub App webhooks | planned |
| `/` | web UI | planned |

Readiness checks are registered by name in `server.Options.Checks`; the store bead adds a Postgres ping. `/readyz` is unauthenticated, so check errors (which can name hosts, users or databases) are only logged, never returned. Concurrent probes share one run of the checks, and the result is reused for one second, so probe traffic cannot multiply load on dependencies. `/metrics` is served on its own optional listen address (metrics bead) so it is not exposed through the public ingress.

On `SIGINT` or `SIGTERM` the server marks itself not ready and keeps serving for the shutdown delay (default 5 s), so `/readyz` answers `503 {"status":"shutting_down"}` while load balancers and Kubernetes endpoints stop routing to it. It then stops accepting connections and waits up to the shutdown timeout for in-flight requests before exiting. A second signal during shutdown terminates the process immediately.

## Testing strategy

- Standard library `testing` only (no testify), table-driven with `t.Run` and `t.Parallel`, `httptest` for HTTP.
- Engine logic is tested against hand-written fakes of its ports; no Postgres, GitHub or Kubernetes needed.
- Tests always run with the race detector and shuffled order (`just test`).
- Postgres integration tests read `DOORSTEP_TEST_DATABASE_URL` and call `t.Skip` when it is unset. Each test gets an isolated schema through a helper the store bead adds (`internal/store/storetest`).
- CI runs a `postgres:18-alpine` service (pinned by digest) and exports `DOORSTEP_TEST_DATABASE_URL`, so integration tests run on every pull request.

See [development.md](development.md) for the commands and [configuration.md](configuration.md) for runtime settings.

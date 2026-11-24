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
     succeeded         failed, and a rollback deployment is queued
                       at high priority: it re-pins the previous
                       digest through this same path

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
| `internal/core` | domain types (`Deployment`, `Target`, `State`, `Event`, `ImageRef`, `Digest`, ...), the state machine and shared errors such as `ErrNotFound`; imports nothing internal | built |
| `internal/store` | Postgres via pgx, embedded goose migrations, transactions, deployment creation and queue claims; see [database.md](database.md) | built |
| `internal/queue` | durable work queue: `SELECT ... FOR UPDATE SKIP LOCKED` claims with leases and heartbeats | planned |
| `internal/locks` | per-target deployment locks and manual freeze locks | planned |
| `internal/history` | append-only transition log and its read models | planned |
| `internal/github` | GitHub App auth (RS256 JWT, cached installation tokens), small REST client, webhook signature verification | built |
| `internal/github/githubtest` | fake GitHub API for tests: verifies App JWTs, serves installation lookup and token minting, authenticates extra handlers with installation tokens | built |
| `internal/intake` | turns GitHub webhooks, registry polls, API and CLI requests into queued deployments | planned |
| `internal/registry` | OCI tag to digest resolution through go-containerregistry, Docker `config.json` credentials matched per registry and path prefix | built |
| `internal/gitops` | pin writer: kustomize `images` and YAML-path strategies, committed through the GitHub API | planned |
| `internal/argocd` | read-only `Application` watcher through the Kubernetes API | planned |
| `internal/gates` | health, metrics and soak gates | planned |
| `internal/rollback` | selects the previous good digest and re-pins it | planned |
| `internal/notify` | Slack, ntfy and generic webhook notifications | planned |
| `internal/engine` | deployment orchestration state machine; declares the ports it consumes | planned |
| `internal/api` | HTTP JSON API (`/api/v1`) handlers | planned |
| `internal/web` | server-rendered UI with `html/template` and embedded htmx | planned |
| `internal/auth` | hashed API tokens for CLI/CI, OIDC login for the UI | planned |
| `internal/metrics` | Prometheus metrics on a private registry, typed recorder, lazy queue depth collector, dedicated listener | built |

Planned packages are created by the bead that implements them; there are no placeholder packages.

## Dependency rules

The design is ports and adapters.

- `core` imports nothing from `internal/`. Every other package may import it.
- `engine` declares the small interfaces it consumes (its ports) and depends only on `core` and those interfaces.
- Adapters (`store`, `queue`, `locks`, `history`, `registry`, `gitops`, `argocd`, `gates`, `rollback`, `notify`) implement engine ports. They never import `engine` or each other, with two exceptions: `gitops` and `intake` use `github`, and Postgres-backed adapters (`queue`, `locks`, `history`, and later `gates` persistence and `auth`) may import `store` for the connection handle, `InTx` and row helpers.
- `store` imports only `core`.
- `api` and `web` call `engine` for commands and `store` read models for queries. They contribute handlers; `server` owns the listener, middleware and health endpoints.
- `cli` `serve` is the only composition root: it constructs adapters, injects them into `engine`, and mounts handlers on `server`.
- No package-level mutable state, no `init()`, no `slog.SetDefault`. Dependencies, including the logger and clock, are passed through constructors. The single exception is the link-time `buildinfo.version` variable. The store is the one place that does not take the injected clock: row timestamps and lease expiry use Postgres `now()`, the only clock all replicas share ([database.md](database.md#clock)).
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

## Registry resolution

`internal/registry` turns a repository and tag into an immutable digest:

```go
resolver := registry.New(registry.Options{Credentials: registry.FileCredentials{Path: path}, UserAgent: ua})
digest, err := resolver.ResolveCommit(ctx, "ghcr.io/owner/app", commitSHA)
```

- Inputs are plain strings and the result is `registry.Digest` (`algorithm:hex`), so no go-containerregistry type leaks out. The engine bead adds a small adapter from `core.ImageRef` to `Resolve` and from `registry.Digest` to `core.Digest`.
- The repository must not carry a tag or digest, and the tag must be a non-empty, valid OCI tag. Nothing defaults to `latest`. `ResolveCommit` encodes the convention that images are tagged with the full commit SHA: it rejects anything but 40 lowercase hex characters with `ErrInvalidReference` before contacting the registry, and is what the engine calls. `Resolve` accepts any tag, which `doorstep image resolve` uses.
- It sends `HEAD /v2/<name>/manifests/<tag>` accepting OCI index, OCI manifest, Docker manifest list and Docker v2 manifest, and returns `Docker-Content-Digest`. A multi-architecture image therefore resolves to its index digest, which stays valid on every node architecture. HEAD is not counted against Docker Hub pull limits.
- Only when HEAD fails without a registry verdict (no digest header, `405`, other unexpected `4xx`) does it fall back to `GET` and hash the manifest body.
- Every call reads credentials, pings the registry, performs the auth handshake (anonymous, basic or bearer token exchange) and issues the request within `Options.Timeout` (default 30 s). There is no token or transport cache and no retry inside the adapter: resolves are low volume, the engine owns retry policy, and every step is idempotent. If polling volume ever demands it, go-containerregistry's `remote.Puller` can cache tokens per repository later.

Errors wrap a sentinel with `%w` when the registry gave a verdict:

| Sentinel | Cause | What callers do |
| --- | --- | --- |
| `ErrNotFound` | `404` on the manifest request (`MANIFEST_UNKNOWN`, `NAME_UNKNOWN`). A `404` from the `/v2/` ping or the token realm means the host is not a registry or is misconfigured, and stays unclassified | the image is not pushed yet: keep waiting for CI |
| `ErrDenied` | `401`/`403` after the auth handshake | surface it: usually missing or wrong credentials. GHCR and Docker Hub also answer this way for a private repository that does not exist yet, so the engine may keep waiting with a louder message; that policy belongs to the engine |
| `ErrInvalidReference` | bad repository, tag or commit SHA, rejected locally or by the registry with `400` `NAME_INVALID`/`TAG_INVALID` | never retry |
| none | network errors, timeouts, `429`, `5xx`, malformed responses | transient: retry with backoff |

Context cancellation and deadlines stay visible through `errors.Is(err, context.Canceled)` and `context.DeadlineExceeded`.

Credentials come from a Docker `config.json` (the `.dockerconfigjson` key of a `kubernetes.io/dockerconfigjson` Secret) parsed by a small parser in the package. Keys are `host[/path-prefix]` and the longest whole-segment prefix of `host/repository` wins, so each GitHub owner on `ghcr.io` can have its own token. `FileCredentials` re-reads the file on every lookup, so a rotated Secret is picked up without a restart. See [configuration.md](configuration.md#registry-credentials).

`serve` loads the credentials file once at startup only to fail fast on a missing or malformed file; nothing in `serve` resolves images yet. The engine bead wires a `Resolver` into `serve` with `Credentials: registry.FileCredentials{Path: cfg.Registry.CredentialsFile}`, the same source `doorstep image resolve` uses, so rotation works at runtime.

## State machine and concurrency

Deployment state is persisted in Postgres:

```
queued → resolving → pinning → syncing → verifying → succeeded
                                              │
                                              └────► failed
```

Any of `resolving`, `pinning`, `syncing` and `verifying` can also move to `failed` (an unresolvable tag or a GitHub error fails a deploy before verification). Any non-terminal deployment can become `cancelled` (operator action), and a `queued` or `resolving` deployment can become `superseded` (a newer deployment for the same target was queued before this one started pinning).

- Terminal states: `succeeded`, `failed`, `cancelled`, `superseded`. A terminal state has no outgoing transitions.
- Active states: `resolving`, `pinning`, `syncing`, `verifying`. At most one deployment per target is active.
- The legal transitions are a table in `core` (`State.CanTransitionTo`), not a switch.
- A rollback is a separate deployment of kind `rollback` with `rollback_of_id` set, queued at a higher priority, and it goes through the same states ([ADR 0002](decisions/0002-rollback-is-a-new-deployment.md)). When a deploy's gates fail, the engine moves it to `failed`, removes its queue entry and enqueues the rollback in one transaction. A deploy still in flight when a rollback is requested is `cancelled`.

- Every transition is a compare-and-set on `(id, from_state)` and is recorded in history.
- Every step is idempotent and resumable: after a restart a worker re-claims the deployment and re-runs the current step. Pinning compares the desired digest with the file in Git before committing; watching keys on the commit SHA.
- Workers claim work with `SELECT ... FOR UPDATE SKIP LOCKED` and hold a lease column refreshed by heartbeat. An expired lease makes the row claimable again, so a crashed replica never wedges a target. A claim only takes the head entry of a target whose deployment is `queued` or active, and never while another entry of that target holds a live lease. Lease expiry uses the database clock; see [database.md](database.md#claiming-work).
- `LISTEN/NOTIFY` is only a wake-up hint; workers also poll, so a missed notification costs latency, not correctness.
- A partial unique index enforces at most one active deployment per (application, environment).
- Manual freeze locks are rows in a `locks` table, visible and removable in the UI and CLI.

Postgres is the only stateful dependency. Running more than one replica is safe. The schema, migration policy and claim semantics are described in [database.md](database.md).

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
| Registry | `github.com/google/go-containerregistry` (`name`, `authn`, `v1/remote`, `remote/transport`) | `remote.Head` for tag to digest with proper registry auth: challenge parsing, token exchange, anonymous/basic/identity tokens and redirect handling are not hand-rolled. Its `DefaultKeychain` is not used. |
| GitHub | hand-written REST client on `net/http`; RS256 JWT with `crypto/rsa`; HMAC with `crypto/hmac` | about six endpoints; small, auditable code instead of a large SDK |
| Metrics | `github.com/prometheus/client_golang` with a private registry | standard exposition, no global registry |
| Web UI | `html/template` and htmx vendored through `embed` | no CDN, no JavaScript build step, works offline |
| UI auth | `github.com/coreos/go-oidc` and `golang.org/x/oauth2`, plus hashed API tokens in Postgres | OIDC discovery and token verification are not hand-rolled |

## GitHub access

`internal/github` is the only package that talks to GitHub. It is a small client on `net/http`; callers think in repositories, not installations or tokens.

```
App.RepoClient(ctx, owner, repo)
   │  validate owner/repo names
   ▼
installation lookup: GET /repos/{owner}/{repo}/installation   (App JWT; cached 1 h per repo)
   ▼
Client ── installation transport ── token cache: POST /app/installations/{id}/access_tokens
   │                                 (App JWT; one token per installation, refreshed 5 min early)
   ▼
Client.Do(ctx, method, path, in, out)   JSON in/out, non-2xx → *APIError
```

- One App, many owners. Installation IDs come from the repository lookup, so repositories under different organizations and personal accounts are served by their own installation's token with no configuration per owner.
- `RepoClient` rejects owner and repository names that are not valid GitHub names (`ErrInvalidRepository`), so names from configuration or webhooks cannot reach other API routes. Owners may contain `_` for Enterprise Managed User accounts (`handle_shortcode`). A missing or suspended installation is `ErrNotInstalled` / `ErrInstallationSuspended` and is never cached.
- Lookups and mints go through a small in-package single-flight group: concurrent callers for the same repository or installation share one call and its result or error, each waiting under its own context. The shared call runs detached from any one caller's cancellation and is bounded to 30 s. A `401` drops the cached token; a `404` or a suspension `403` while minting evicts the token and every lookup that pointed at that installation. Expired lookups are swept whenever a new one is stored. Nothing retries automatically; engine steps are idempotent and retried by the engine.
- Every request, with an App JWT or an installation token, goes through a transport that only sends it when the scheme and host match the configured API URL and refuses anything else with `ErrForeignHost`, including redirects. Tokens are never exported from the package.
- Responses are read through a 10 MiB limit, error messages from GitHub are cut to 512 bytes, and the default HTTP client times out after 30 s. Later beads add typed methods (contents, pull requests, statuses, checks) on top of `Client.Do`.
- `VerifySignature` checks `X-Hub-Signature-256` webhook signatures against the raw body with a constant-time comparison.

Setup, permissions and the least-privilege model are described in [github-app.md](github-app.md).

## Security posture

- GitHub access is through one GitHub App installed on each owner with *Only select repositories*: one-hour installation tokens, held in memory only, refreshed five minutes before expiry, and only ever sent to the configured API host (requests and redirects to any other host are refused). GitHub applies the App's permissions to every selected repository, so the repository selection is what limits reach. Commits are attributed to and verified as the App.
- Pins are written through the GitHub Contents/Git Data API using the blob SHA for optimistic concurrency: no local clone, no git binary, no SSH keys, no writable volume.
- Webhook payloads are verified with HMAC-SHA256 and a constant-time comparison before they are parsed.
- Kubernetes RBAC is `get`, `list` and `watch` on Argo CD `Application` resources only. doorstep never patches or syncs Applications.
- Only image digests are pinned, never mutable tags.
- Registry credentials are read only from the one explicitly configured file: no `$HOME/.docker`, no `DOCKER_CONFIG`, and credential helpers or stores are never executed. Credentials are sent only to the registry (and token realm) they match, are redacted when a `Credential` is formatted or logged, and errors never include file contents.
- Secrets never appear inline in configuration: they come from environment variables or `*_file` paths (mounted Kubernetes Secrets).
- The HTTP server sets read-header, read, write and idle timeouts and a header size limit; panics are recovered and logged.
- CI pins every action by full commit SHA, runs with a read-only token and does not persist checkout credentials. Dependabot keeps pins and Go modules current.

## HTTP surface

One listener (default `:8080`) serves everything public:

| Path | Purpose | Status |
| --- | --- | --- |
| `GET /healthz` | liveness: always `200 {"status":"ok"}` while the process serves | built |
| `GET /readyz` | readiness: runs registered named checks (2 s timeout each, concurrently, results cached for 1 s), currently `postgres`; `200` when all pass, `503` when any fails, with each check reported only as `ok` or `fail`; `503 {"status":"shutting_down"}` once shutdown begins | built |
| `GET /metrics` | Prometheus exposition, on the metrics listener (default `:9090`) rather than this one unless `--metrics-listen-addr` is set empty (see [metrics.md](metrics.md)) | built |
| `/api/v1/...` | JSON API | planned |
| `/webhooks/github` | GitHub App webhooks | planned |
| `/` | web UI | planned |

Readiness checks are registered by name in `server.Options.Checks`; `serve` registers `postgres`, which pings the connection pool. `/readyz` is unauthenticated, so check errors (which can name hosts, users or databases) are only logged, never returned. Concurrent probes share one run of the checks, and the result is reused for one second, so probe traffic cannot multiply load on dependencies. Further handlers are mounted through `server.Options.Routes` (a map of `ServeMux` pattern to handler), which keeps them inside the request logging and panic recovery middleware; `/metrics` requests are logged at debug like probes.

`/metrics` is served by a second `server.Server` on its own listener (default `:9090`, `--metrics-listen-addr`), which also answers `/healthz` and `/readyz` with the same checks, and the main listener does not mount it. This keeps it off the public ingress that receives GitHub webhooks, as the metrics are unauthenticated and name apps and builds. Setting the address to empty is an explicit opt-in to mounting `/metrics` on the main listener. Server-mounted routes may not use the built-in `GET /healthz` and `GET /readyz` patterns; `server.New` panics on them. Metrics live in a private Prometheus registry owned by `metrics.Metrics` (no global registry); components record through the typed `metrics.Recorder`, and the queue depth is read lazily from the database at scrape time, bounded by a 2 second timeout.

On `SIGINT` or `SIGTERM` the server marks itself not ready and keeps serving for the shutdown delay (default 5 s), so `/readyz` answers `503 {"status":"shutting_down"}` while load balancers and Kubernetes endpoints stop routing to it. It then stops accepting connections and waits up to the shutdown timeout for in-flight requests before exiting. A second signal during shutdown terminates the process immediately.

## Testing strategy

- Standard library `testing` only (no testify), table-driven with `t.Run` and `t.Parallel`, `httptest` for HTTP.
- Engine logic is tested against hand-written fakes of its ports; no Postgres, GitHub or Kubernetes needed.
- Tests always run with the race detector and shuffled order (`just test`).
- Postgres integration tests read `DOORSTEP_TEST_DATABASE_URL` and call `t.Skip` when it is unset. Each test gets an isolated schema through `internal/store/storetest`.
- CI runs a `postgres:18-alpine` service (pinned by digest), exports `DOORSTEP_TEST_DATABASE_URL` and sets `DOORSTEP_TEST_REQUIRE_DATABASE=true`, so integration tests run on every pull request and a missing database fails the build instead of skipping.

See [development.md](development.md) for the commands, [configuration.md](configuration.md) for runtime settings, [database.md](database.md) for the schema and [metrics.md](metrics.md) for the exported metrics.

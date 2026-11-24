# Architecture

This document describes how doorstep is put together: what it does, how a deployment flows through the system, how the Go packages are laid out and allowed to depend on each other, and which libraries it builds on. It grows as features land. Significant decisions are recorded as Architecture Decision Records under `docs/decisions`; this document is the current map, the ADRs explain why.

## Purpose and scope

doorstep is a deployment engine that sits upstream of [Argo CD](https://argo-cd.readthedocs.io/). It decides *what* gets deployed and *when*:

- it accepts deploy requests from webhooks, registry polling, its HTTP API and its CLI;
- it queues them and serialises them per application and environment, honouring manual freeze locks;
- it resolves image tags to immutable digests and commits those digests to the GitOps repository;
- it watches Argo CD until the commit is synced and healthy, evaluates gates (health, metrics, soak time) and rolls back by re-pinning the previous digest when a gate fails;
- it notifies people and records every transition in a queryable history.

doorstep never talks to workloads directly and never mutates Argo CD. Git is the only thing it writes to; Argo CD remains the only thing that applies manifests to the cluster. doorstep only observes Argo CD `Application` resources and, when the `job` gate is enabled, validation Jobs that Argo CD runs as PostSync hooks ([ADR 0016](decisions/0016-validation-job-gate.md)).

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
     gates: before (checks, lock, schedule, external, backup webhook for
            releases flagged as migrations) and after (health, soak,
            prometheus queries, validation Job, external)
          │                 │
       pass                fail
          ▼                 ▼
     succeeded         failed, and a rollback deployment is queued
                       at high priority: it re-pins the previous
                       digest through this same path, unless the
                       release crossed a flagged migration: then the
                       rollback is withheld and a person decides

 notify reports queued, started and every terminal state (Slack, webhooks,
 GitHub deployment statuses) and lock changes, every gate result is mirrored
 to a GitHub commit status; history records every transition.
```

## Package map

There is one binary, `cmd/doorstep`. Everything else lives under `internal/`, so the module exposes no public Go API. Kubernetes install manifests live in `deploy/kustomize` (a base, optional registry-credentials and job-gate components and an example overlay) with a test that keeps them consistent with the code; see [install.md](install.md).

| Package | Responsibility | Status |
| --- | --- | --- |
| `cmd/doorstep` | `main`: wires OS signals, arguments, stdio and environment into `cli.Run`; embeds the IANA time zone database (`time/tzdata`) | built |
| `internal/cli` | cobra command tree built by a constructor; `serve` is the composition root; `config validate` checks a configuration file; `lock`, `unlock` and `locks` call the API through a small shared client; `rollback`, `block`, `unblock` and `blocks` call the API too; `argocd status` reads an app's Argo CD Application; `serve` also holds the engine's adapters (`engine.go`), the app mapping (`apps.go`), gate wiring (`gates.go`) and the lock notifier (`locknotify.go`) | built |
| `internal/config` | configuration model, defaults, environment overrides and validation; YAML file loader, app catalog (apps, gates with their deploy windows and Prometheus queries, notification targets, Prometheus servers) and secret references | built |
| `internal/promquery` | small Prometheus HTTP API client on the standard library: `POST` instant and range queries, scalar, vector and matrix results (NaN and infinities included), typed API errors, bearer and basic authentication read per request, no redirects, 1 MiB response limit; a leaf used by `gates` through its `MetricsQuerier` port and built by `cli` ([ADR 0013](decisions/0013-prometheus-query-gate.md)) | built |
| `internal/windows` | recurring time windows: five-field cron parsing (`robfig/cron`), time zone loading and the active/next arithmetic behind `schedule` gates; a leaf used by `config` for validation and by `gates` for evaluation | built |
| `internal/logging` | builds the injected `*slog.Logger` (JSON or text, level) | built |
| `internal/buildinfo` | version, commit and commit time from `-ldflags -X` link-time variables, overridden by VCS revision/time and the dirty flag from `runtime/debug` when present; Go version from `runtime/debug` | built |
| `internal/server` | HTTP lifecycle: `http.Server` with timeouts, graceful shutdown, request logging and panic recovery, `/healthz`, `/readyz` and readiness checks | built |
| `internal/core` | domain types (`Deployment`, `Target`, `State`, `Event`, `Notification`, `ImageRef`, `Digest`, `Role`, `Actor`, `AuditEntry`, `Block`, ...), the state machine and shared errors such as `ErrNotFound`; imports nothing internal | built |
| `internal/store` | Postgres via pgx, embedded goose migrations, transactions, deployment creation, tracked commits, API tokens, sessions, the audit log, compare-and-set transitions with their event log, lineage, deploy history read models, queue claims with batching, leases and `LISTEN/NOTIFY` (claims honour freeze locks), the freeze lock repository and blocks (with their pending git trailers); see [database.md](database.md) | built |
| `internal/queue` | per-app deploy queue adapter: priorities, lease length, per-app batch limit, and the `Runner` worker loop (claims, wake-ups, polling, heartbeats); see [queue.md](queue.md) | built |
| `internal/locks` | manual freeze locks per app and global: validation, lock/unlock/list, `Blocking` for the engine, post-commit notifications through a `Notifier` port, and the expiry and reminder sweep; depends only on `core` through a `Repository` port that `*store.Store` satisfies; see [locks.md](locks.md). Per-target deployment locks are left to the engine | built |
| `internal/github` | GitHub App auth (RS256 JWT, cached installation tokens), small REST client, check runs, commit statuses, check suites, branch heads, deployments and deployment statuses, webhook signature verification, user lookups for GitHub sign-in; `Repo` handle with typed contents, refs, commits and pull request calls | built |
| `internal/github/githubtest` | fake GitHub API for tests: verifies App JWTs, serves installation lookup and token minting, paginated check runs (with `check_name` filtering), statuses, check suites and branch refs, authenticates extra handlers with installation tokens; in-memory fake repository (branches, commits, contents, refs, commit comparisons, pull requests with squash merges) with hooks to inject concurrent writes, blocked merges and conflicts | built |
| `internal/intake` | merge and CI detection: tracks pushes to app branches, evaluates required checks and enqueues green commits exactly once; the signed GitHub webhook handler with delivery deduplication and the polling safety net that drive it; see [Merge and CI detection](#merge-and-ci-detection) and [Webhooks and polling](#webhooks-and-polling). `LiveChecks` reads and judges a commit's checks on demand for the `checks` gate; opt-in migration detection flags a release from PR labels and commit trailers across every commit since the previous deploy ([rollback.md](rollback.md#migration-aware-releases)). API and CLI deploy requests are later beads | built |
| `internal/registry` | OCI tag to digest resolution through go-containerregistry, Docker `config.json` credentials matched per registry and path prefix | built |
| `internal/registry/registrytest` | fake OCI registry for tests: `/v2/` ping and manifest `HEAD`/`GET` with `Docker-Content-Digest` for pushed tags | built |
| `internal/gitops` | pin writer: kustomize `images` and YAML-path strategies, byte-preserving edits, committed through the GitHub Contents API directly or through a pull request doorstep merges; commit trailers; see [gitops.md](gitops.md) | built |
| `internal/argocd` | read-only `Application` watcher through the Kubernetes API: `WaitSynced` (synced to a revision containing the pin commit and healthy, with a per-app timeout), `Check` and `Health`; a get and a watch per wait, no cache; see [argocd.md](argocd.md) | built |
| `internal/argocd/argocdtest` | fake Kubernetes API for Argo CD `Application` resources: get and watch (field selector, resource version, timeout), event pushes, closed and expired watches, injected `401`/`403`/`404`/`5xx`, request counts and a generated kubeconfig | built |
| `internal/kube` | small Kubernetes REST client: in-cluster ServiceAccount or explicit kubeconfig subset, host-pinned transport, bounded `Get` and `Stream`, `StatusError` with `ErrNotFound`, `ErrForbidden` and `ErrGone`; imports nothing internal ([ADR 0008](decisions/0008-kubernetes-rest-client.md)) | built |
| `internal/jobs` | read-only Kubernetes `batch/v1` Job reader for the `job` gate: one `GET` per call through `internal/kube`, decoded into a small `Job` type, `ErrNotFound`; imports only `kube` ([ADR 0016](decisions/0016-validation-job-gate.md)) | built |
| `internal/gates` | gate model and evaluation: the `Gate` interface, a registry that builds per-app before/after plans from the configuration, and an evaluator that runs a phase, records results through its `ResultStore` port, enforces timeouts, skips gates on rollbacks, aggregates the verdict and notifies observers; the built-in `checks`, `lock`, `schedule`, `health` and `soak` gate types, registered by `RegisterBuiltins` over small read ports; the deferred `backup` webhook gate, registered by `RegisterBackup`; the `prometheus` gate type, registered by `RegisterPrometheus` over its `MetricsQuerier` port; the `job` gate type, registered by `RegisterJobGate` over its `JobReader` and the shared `ApplicationReader` port; the `external` gate type (`RegisterExternal`), which reads results other systems posted, and `Reporter`, the service behind the external gate API; see [gates.md](gates.md#built-in-gates) | built |
| `internal/rollback` | rollback requests and blocks: picks the current version and the previous good, unblocked digest, cancels the deploy it replaces and queued deploys of the blocked commit, queues the rollback deployment at rollback priority, records the block and the audit row in one transaction (retried on state conflicts); manual block, unblock and list; the confirmation guard for rollbacks across releases flagged as database migrations and `MigrationsCrossed` for the engine; the engine's `BlockList` port. Depends on `core` and a `Repository` port with `InTx` over `*store.Store`; see [rollback.md](rollback.md) | built |
| `internal/notify` | Slack and generic webhook notifications; GitHub deployment statuses; `GateStatuses`, the gate observer that mirrors every gate result to a GitHub commit status; see [notifications.md](notifications.md) and [gates.md](gates.md#github-commit-statuses) | built |
| `internal/engine` | the deployment state machine: claims run admit (before gates), resolve, pin, sync and verify (after gates) as idempotent, resumable steps; classifies errors, bounds retries per state, routes every failure through a `FailurePolicy`, records transitions, emits `deploy.*` notifications and deployment metrics; declares the ports it consumes and imports only `core`; see [engine.md](engine.md) | built |
| `internal/api` | HTTP JSON API (`/api/v1`): one mux, one file per resource, JSON error envelope, role-checked endpoints (viewer reads, reporter posts external gate results, deployer changes locks, requests rollbacks and changes blocks), queue, lock, rollback, block, gate report and deploy history endpoints; see [api.md](api.md) | built |
| `internal/web` | server-rendered UI with `html/template` and embedded htmx | planned |
| `internal/auth` | viewer, deployer and admin roles; hashed API tokens; OIDC or GitHub sign-in with Postgres-backed sessions; authentication middleware with CSRF enforcement and `RequireRole`; see [authentication.md](authentication.md) | built |
| `internal/metrics` | Prometheus metrics on a private registry, typed recorder, lazy queue depth collector, dedicated listener | built |

Planned packages are created by the bead that implements them; there are no placeholder packages.

## Dependency rules

The design is ports and adapters.

- `core` imports nothing from `internal/`. Every other package may import it.
- `kube` imports nothing from `internal/`. `jobs` imports only `kube`. `argocd` imports only `core` and `kube`; it reaches GitHub for ancestry checks through its own `Ancestry` port, which `cli` implements over `(*github.Repo).Compare`, and it never imports `config` (`cli` maps the catalog to `argocd.Application` values).
- `engine` declares the small interfaces it consumes (its ports) and depends only on `core` and those interfaces.
- Adapters (`store`, `queue`, `locks`, `registry`, `gitops`, `argocd`, `gates`, `rollback`, `notify`) implement engine ports. They never import `engine` or each other, with two exceptions: `gitops`, `intake`, `notify` and `auth` (for GitHub sign-in) use `github`, and Postgres-backed adapters (`queue`, `locks`, `intake`'s `PostgresCommits`, `auth`, `rollback`, and later `gates` persistence) may import `store` for the connection handle, `InTx` and row helpers. `intake` imports only `core`, `github` and `store`; it does not import `config` or `metrics` (its `Observer` port is adapted to `metrics.Recorder` in `cli`).
- `store` imports only `core`.
- Actions are audited through `(*store.Store).RecordAudit`, called on the transaction-bound store in the same `InTx` as the action. Consumers declare a one-method `RecordAudit(ctx, core.AuditEntry) error` port rather than importing `auth`.
- `api` and `web` call `engine` for commands and `store` read models for queries. History is part of `store` rather than a package of its own: a transition must update the deployment, append its event and delete its queue entry atomically through the store's transaction handle. They contribute handlers, wrapped in `auth.RequireRole` or `auth.RequireRoleOrLogin`, which `serve` also wraps in the authenticator; `server` owns the listener, logging, panic recovery and health endpoints. Lock commands are the exception: they do not involve a deployment, so `api` calls `locks.Service` directly through its own small `LockService` port. Rollback requests and blocks are the other exception: a rollback request is a queue and block write, not orchestration, so `api` calls `rollback.Service` directly through its `RollbackService` and `BlockService` ports, and the engine executes the queued rollback deployment like any other. `cli` wraps `rollback.Service` for the API so that the engine announces the queued rollback and the cancelled deployments after the request commits.
- `gates` imports `core`, `config` (the typed `App` and `Gate` its factories read), `windows` (deploy window arithmetic), `promquery` (only the result types of its `MetricsQuerier` port) and `metrics` (only the label types of its `Metrics` port). It never imports `notify`: the `backup` gate gets its HTTP client, secret reader and HMAC signer injected through `gates.BackupDeps`, which `cli` fills with `notify.NewHTTPClient`, a `config.SecretRef` reader and `notify.Sign`. It reaches Postgres only through its `ResultStore`, `TrackedChecks`, `ExternalReports` and `ReportStore` ports, which `*store.Store` satisfies, and never imports `store`, `engine` or another adapter: freezes come through `FreezeReader` (`*locks.Service`), live CI state through `LiveChecks` (`intake.LiveChecks`) and Argo CD through `ApplicationReader`, for which the composition root adapts the `argocd` package to the gates-owned `gates.Application` type, and validation Jobs through `JobReader` (the `job` gate also reads the Application's latest operation through `ApplicationReader`), which `cli` (`internal/cli/jobs.go`) adapts over the `jobs` package to the gates-owned `gates.Job` type.
- `promquery` imports only the standard library. `cli` builds one client per configured server and passes them to `gates.RegisterPrometheus`, so `gates` never sees URLs or credentials.
- `windows` imports only the standard library and `robfig/cron`, so configuration validation and gate evaluation share one parser: a configuration that validates cannot fail to build its gates at startup.
- `cli` `serve` is the only composition root: it constructs adapters, injects them into `engine`, and mounts handlers on `server`. Adapters that only translate types or classify errors for the engine live in `cli`, so no adapter package imports `engine`.
- No package-level mutable state, no `init()`, no `slog.SetDefault`. Dependencies, including the logger and clock, are passed through constructors. The only exceptions are the link-time `buildinfo.version`, `buildinfo.commit` and `buildinfo.commitTime` variables, which the release image build sets because it has no `.git` directory. The store is the one place that does not take the injected clock: row timestamps and lease expiry use Postgres `now()`, the only clock all replicas share ([database.md](database.md#clock)).
- `context.Context` is the first parameter of anything that does I/O or may block.
- Errors are wrapped with `%w` and inspected with `errors.Is` / `errors.As`.

## Engine ports

`internal/engine` drives deployments through their states ([engine.md](engine.md), [ADR 0012](decisions/0012-deployment-engine.md)). It declares the ports it consumes over its own small types, so it imports only `core`:

```go
type DeploymentStore interface {
	Get(ctx context.Context, id core.DeploymentID) (core.Deployment, error)
	Transition(ctx context.Context, t engine.Transition) (core.Deployment, error)
	SetLineage(ctx context.Context, id core.DeploymentID, l core.Lineage, actor string) (core.Deployment, error)
	TimeInState(ctx context.Context, id core.DeploymentID, state core.State) (time.Duration, error)
}

type DigestResolver interface {
	Resolve(ctx context.Context, ref core.ImageRef) (core.Digest, error)
}

type PinWriter interface {
	Pin(ctx context.Context, req engine.PinRequest) (engine.PinResult, error)
	Abandon(ctx context.Context, req engine.PinRequest) error
}

type SyncWatcher interface {
	WaitSynced(ctx context.Context, t core.Target, revision string) (core.SyncStatus, error)
	Check(ctx context.Context, t core.Target, revision string) (core.SyncStatus, error)
	Health(ctx context.Context, t core.Target) (core.Health, error)
}

type GateEvaluator interface {
	Evaluate(ctx context.Context, d core.Deployment, phase core.GatePhase) (core.GateOutcome, error)
	Reevaluate(ctx context.Context, d core.Deployment, phase core.GatePhase) (core.GateOutcome, error)
}

type BlockList interface {
	Blocked(ctx context.Context, t core.Target, commit core.Commit, digest core.Digest) (core.Block, bool, error)
	PendingTrailers(ctx context.Context, t core.Target) (core.BlockTrailers, error)
	MarkMirrored(ctx context.Context, t core.Target, trailers core.BlockTrailers, commit core.Commit) error
}

type Notifier interface {
	Notify(ctx context.Context, n core.Notification)
}

type Metrics interface {
	DeploymentStarted(app string)
	DeploymentFinished(app string, outcome engine.Outcome, duration time.Duration)
}

type FailurePolicy interface {
	Decide(ctx context.Context, f engine.Failure) (engine.FailureAction, error)
}
```

`serve` is the composition root. `internal/cli/engine.go` holds the thin adapters: `engineStore` maps `engine.Transition` to `store.Transition`; `digestResolver` calls `registry.Resolver.ResolveCommit` and wraps its sentinels in `engine.ErrPermanent` or `engine.ErrImageNotReady`; `pinWriter` maps each app to its `gitops.Target` (built by `appWiring` in `internal/cli/apps.go`, which also maps apps to `intake.App` and `argocd.Application`) and classifies `gitops` errors; `engineMetrics` adapts `metrics.Recorder`; `queueHandler` turns `queue.Claimed` into `engine.Claim`. `*argocd.Watcher`, `*gates.Evaluator`, `*rollback.Service` and `*notify.Dispatcher` satisfy their ports as they are, and `engine.FailPolicy` is the default policy.

There is no queue port: `queue.Runner` claims, heartbeats and releases, and a terminal transition deletes the queue entry in the same transaction, so the engine never calls the queue. There is no locker port either: the claim skips frozen deploys and the opt-in `lock` gate reads freezes. `LastSuccessfulDeployment` and an enqueue port are left to automatic rollback (doorstep-ehl.6.5), which also adds the failure policy action that fails a deploy, enqueues its rollback and creates the block in one transaction.

`BlockList` is `*rollback.Service` ([rollback.md](rollback.md), [ADR 0009](decisions/0009-blocks-in-postgres-mirrored-to-git.md)). The engine calls `Blocked` with the resolved digest of a deploy and fails it when blocked, passes `PendingTrailers` with every pin and calls `MarkMirrored` after a pin that wrote a commit, pins a rollback's preset digest without resolving, and calls `Pin` again with `Repin` before a rollback succeeds, which confirms the pin or writes it again if a cancelled deploy's late pin replaced it ([rollback.md](rollback.md#the-late-pin-race)).

`Notifier` has no error result on purpose: `notify.Dispatcher` validates and queues the notification without network I/O and returns, so a notification can never block or fail a deploy (see [Notifications](#notifications)).

`PinWriter` wraps `*gitops.Writer`. `gitops` owns its request and result types (plain strings plus `core.Digest`, `core.Commit`, `core.DeploymentID` and `core.Kind`) and imports only `core` and `github`, never `config`. It has two strategies: editing the `images` list of a `kustomization.yaml`, and setting a value at a YAML path (for Helm values files and plain manifests). See [GitOps pin writer](#gitops-pin-writer).

`SyncWatcher` is `*argocd.Watcher`. `core.SyncStatus`, `core.Health`, `core.SyncFailure` and the sentinels `core.ErrSyncTimeout`, `core.ErrSyncFailed`, `core.ErrSyncTargetNotFound`, `core.ErrSyncForbidden` and `core.ErrSyncUnavailable`, and `core.DefaultSyncTimeout`, live in `core/sync.go`, so the engine switches on them without importing the adapter. `WaitSynced` returns the last observed status with every error; its `Failure` carries a stable reason and a class hint (`transient`, `version`, `configuration`, `unknown`) that the engine passes to its failure policy. The per-app timeout is applied inside the adapter, and a caller deadline or cancellation comes back as the plain context error, so the engine passes the remaining time of a resumed wait as its own deadline and tells it apart from a shutdown. The watcher also offers a non-blocking `Check(ctx, target, revision)` with the same assessment, and `Status`, which the `health` gate reads. `serve` builds it with `cli.newSyncWatcher` whenever apps are configured, from the `--kubeconfig`/`--kube-context` flags or the in-cluster ServiceAccount. See [argocd.md](argocd.md).

`GateEvaluator` is `*gates.Evaluator`. The engine evaluates the `before` phase while the deployment is still `queued` and releases it with a delay while that is `pending`; a `before` failure fails it from `queued`. An `after` outcome of `passed` makes it succeed and `failed` fails it. Individual gates implement `gates.Gate` and are built from the configuration by `gates.Registry`; the engine never sees them. `internal/cli/gates.go` registers the built-in types with `gates.RegisterBuiltins`, passing `*store.Store`, `intake.LiveChecks`, `*locks.Service` and `argoApplications`, an adapter from `(*argocd.Watcher).Status` to `gates.ApplicationReader` ([gates.md](gates.md#wiring)). The `prometheus` type is registered with `gates.RegisterPrometheus`, the `external` type with `gates.RegisterExternal(registry, st)`, and the commit status mirror from `newGateStatusMirror` is appended to the evaluator's observers when a GitHub App is configured and closed on shutdown. The `job` type is registered by `registerJobGateFromDeps`, which builds a Kubernetes client with `newKubeClient` and calls `registerJobGate`, sharing the `argoApplications` adapter, from `internal/cli/jobs.go` when an app configures a `job` gate.

## Registry resolution

`internal/registry` turns a repository and tag into an immutable digest:

```go
resolver := registry.New(registry.Options{Credentials: registry.FileCredentials{Path: path}, UserAgent: ua})
digest, err := resolver.ResolveCommit(ctx, "ghcr.io/owner/app", commitSHA)
```

- Inputs are plain strings and the result is `registry.Digest` (`algorithm:hex`), so no go-containerregistry type leaks out. `cli.digestResolver` adapts `core.ImageRef` to `ResolveCommit` and `registry.Digest` to `core.Digest` for the engine.
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

`serve` loads the credentials file once at startup to fail fast on a missing or malformed file, and gives the engine a `Resolver` with `Credentials: registry.FileCredentials{Path: cfg.Registry.CredentialsFile}`, the same source `doorstep image resolve` uses, so rotation works at runtime.

## Merge and CI detection

`internal/intake` decides when a commit merged to an app's branch becomes deployable. It is driven from outside: the GitHub webhook handler and the poller ([Webhooks and polling](#webhooks-and-polling)) parse payloads and call it with plain strings.

```
push to app branch ──► Service.HandlePush(Push{Repository, Branch, SHA, Source, PushedBy})
                          │ match apps: repository case-insensitive, branch exact
                          ▼
                       Commits.Track      under the app's row lock: ask Branches.BranchHead;
                          │                not the head → ignored (stale delivery);
                          │                head → one tracked commit per app and SHA, every
                          │                other open commit of the app superseded
                          ▼
check_run / status ──► Service.Reevaluate(repository, sha)   tracked, open commits of apps that
poller             ──► Service.Open(limit) + Reevaluate      still follow that repository and branch
                          │
                          ▼
                       CheckSource.Checks  one snapshot per repository and SHA, shared by apps;
                          │                read time taken before the first API call
                          ▼
                       Evaluate(required, snapshot) → pending | success | failure, evidence
                          │
                          ▼
                       migration flag   success and the app has `migrations` only, outside the
                          │             transaction: Baselines.UnreleasedMigrations and
                          │             LastDeployCommit, then
                          │             MigrationSource compare/commit and commits/{sha}/pulls;
                          │             on error nothing is settled or enqueued
                          ▼
                       Commits.Settle  one transaction under the app and row locks: save the
                                       evaluation unless a newer one is stored; if the saved
                                       state is success and the commit is not blocked,
                                       Queue.Enqueue(Deployment) and link it
```

- **Blocked commits.** The settle callback receives the transaction-bound `Blocks` port (`Blocked(ctx, target, commit)`, which `*store.Store` implements) beside the queue, and checks it before enqueueing. A commit with an active block ([rollback.md](rollback.md#blocks)) is not enqueued: the callback returns a `*core.BlockedError`, `SettleTrackedCommit` keeps the saved evaluation and commits without enqueueing, the tracked commit stays open, `Result.Blocked` names the block and intake logs `commit is blocked; not enqueued`. Rollback requests take the same app row lock, so a rollback and a settlement of its commit are strictly ordered. After an unblock, the next check event or poll enqueues the commit if it is still open.
- **Migration flags.** For an app with a `migrations` policy, a commit whose evaluation is `success` is first inspected for database migrations: every commit since the app's newest `deploy` row (`Baselines.LastDeployCommit`, `*store.Store`), read through `MigrationSource` (`Range`, `Commit`, `PullRequests`, implemented by `GitHub`), with reads shared within one evaluation pass. The flags of flagged `deploy` rows that have not reached the cluster since the last one that did (`Baselines.UnreleasedMigrations`) are carried into the result, so a batched-away or failed-before-pin migration still flags the release that ships it. The result becomes `Migration` and `MigrationReason` on the enqueued deployment. It runs outside the settle transaction so the app row lock never waits on GitHub, and it fails closed: on a read error the commit is neither settled nor enqueued, intake logs `reading migration flags failed; not enqueued`, reports `Observer.MigrationDetectionFailed(app)` (the `doorstep_migration_detection_failures_total` counter) and returns the error, and the next event or poll retries. `New` refuses a policy without both ports. Rules are in [rollback.md](rollback.md#detection).
- **Ports.** `Commits` (track, look up by SHA, settle, list open), `CheckSource` (one `Snapshot` of check runs, commit statuses and, when every check counts, check suites), `Branches` (the SHA a branch points at, `""` when it does not exist) and `Queue` (`Enqueue(ctx, core.Deployment)`). `GitHub` implements `CheckSource` and `Branches` over anything with `RepoClient`, such as `*github.App`. `PostgresCommits` implements `Commits` over `*store.Store` and is built with a factory `func(tx *store.Store) Queue`, so the queue a settlement uses is bound to its transaction. `Options.Now` sets the clock that stamps snapshot read times (default `time.Now`).
- **States.** A tracked commit's `checks_state` is `pending`, `success` or `failure` (`core.ChecksState`). None is terminal: a failed commit can turn green after a CI re-run. A commit is closed once it has been enqueued (`enqueued_at` set) or superseded; closed commits are never evaluated again and settling them is a no-op. Deleting the deployment later does not reopen the commit.
- **Newest commit wins, in git order.** GitHub does not deliver webhooks in order, and redeliveries and the poller can replay an old push. `Track` therefore holds the app's row lock while it asks GitHub for the branch head and records the push only if its SHA is still the head; otherwise the outcome is `stale` and nothing changes. Because every push for an app is checked and recorded under that one lock, a later `Track` always sees a head at least as new as an earlier one, and the head is never superseded by an older commit. A recorded head supersedes every other open commit of the app, so a slow CI run on an older commit never deploys after a newer push, and if the newest commit is red nothing deploys until a green one follows. A force-push back to a superseded commit makes it the head again: `Track` reopens it (outcome `reopened`) and it deploys once green. The GitHub call happens inside the transaction; pushes to one app are rare, so serialising them costs nothing noticeable. Queue supersession of deployments that already exist is a separate rule owned by the queue.
- **Exactly once, never on a stale snapshot.** `Settle` locks the app row and then the tracked row. It saves the evaluation only when the snapshot was read after the stored one (`evaluated_at < read time`), so concurrent evaluations of one SHA (several check events, the poller, several replicas) can finish in any order and the newest snapshot wins. If the stored state is then `success` and the commit is open, it enqueues the deployment and links it in the same transaction; an enqueue error rolls back the evaluation, the deployment and the link together, and the commit stays open for the next event or poll. Read times come from each replica's clock, so replicas need ordinary NTP-level clock agreement.
- **Poller.** `Service.Open(limit)` lists every open commit, pending, failed or green, of the configured apps on their configured repository and branch, least recently evaluated first. Commits of apps that were removed or moved to another repository or branch are never listed or re-evaluated, so they cannot crowd out real ones. Since a recorded head supersedes everything else, there is at most one open commit per app. Re-evaluating failed commits lets a re-run that missed its webhook still deploy.
- **Evaluation.** `Evaluate(required, snapshot)` is pure and exported so the `checks` gate can reuse it. Rules are in [configuration.md](configuration.md#required-checks). Evidence is a list of `core.Check` sorted by name and stored as `jsonb` on the tracked commit. When every app evaluating a SHA lists `required_checks`, the snapshot asks GitHub only for check runs with those names (one `check_name` query each) plus commit statuses, so commits with thousands of unrelated check runs still evaluate.
- **Enqueued deployment.** `core.Deployment{Target, Kind: deploy, Source and RequestedBy from the push, Commit{Repository, SHA}, Image{ImageRepository, Tag: full SHA}}`, following the convention that images are tagged with the full commit SHA.
- **Errors.** If the branch head cannot be read, the push is not recorded and the error is returned so the delivery is retried. Once recorded, a commit is evaluated; if reading checks fails, it stays `pending` and the wrapped error is returned (`github.ErrNotInstalled` and friends stay inspectable with `errors.Is`). Failures for one app are joined with `errors.Join` and do not block other apps. There are no retries inside `intake`: webhook redeliveries, later check events and the poller retry.
- **Ignored input.** Pushes to repositories or branches no app follows, the all-zero SHA of a deleted branch and pushes that are no longer the branch head return no results and no error. The caller strips `refs/heads/` before calling.
- **Wiring.** `serve` builds the service in `internal/cli/intake.go` when the GitHub App is configured and apps exist: each `config.App` becomes `intake.App{Target: core.AppTarget(name), Repository, Branch, RequiredChecks, ImageRepository, Migrations}`, `intake.GitHub{Clients: app}` supplies checks, branch heads and migration reads, the store supplies migration baselines, and `PostgresCommits` gets a factory that wraps `queue.New` over the transaction-bound store. The mapping lives in `appWiring.intake()` (`internal/cli/apps.go`). When the engine runs, `PostgresCommits` is wrapped so that `Engine.Queued` announces each enqueued deployment after its settlement commits, and the engine's queue runner picks the deployment up.
- **Not here.** `intake` applies no timeout to commits that stay pending (the `checks` gate owns the 30 minute timeout).

## Webhooks and polling

Two drivers call `intake.Service`, both in `internal/intake` and both depending on it only through small ports (`Pusher` with `HandlePush` and `Reevaluate`, plus `Opener` and `TracksHead` for the poller). The decision record is [ADR 0010](decisions/0010-webhooks-and-polling-safety-net.md); operator setup is in [github-app.md](github-app.md#webhooks).

```
POST /webhooks/github ─► size limit (5 MiB) ─► VerifySignature(raw body) ─► headers ─► parse event
                                                    401                    400          400
    push (refs/heads/*) ──────────► Deliveries.Claim ─► HandlePush{Source: webhook} ─► Complete / Release
    check_run / check_suite / status ─► Reevaluate(repository, sha)
    installation(_repositories) ─► InstallationCache.EvictInstallation / EvictRepository
    ping, anything else ─► 200

Poller.Run (every replica, tick min(interval, 15s))
    PollClaims.ClaimPoll(lease) ── no row ─► skip
        │ claimed
        ▼
    Branches.BranchHead per distinct repository and branch
        ─► TracksHead? ── no ─► HandlePush{Source: poll}
    Opener.Open(100) ─► Reevaluate each repository and SHA not handled this cycle
                        and not evaluated in the last half interval
    PollClaims.FinishPoll(lease, interval)
```

- **Webhook handler.** `intake.NewWebhook` returns an `http.Handler` mounted at `intake.WebhookPattern` (`POST /webhooks/github`) on the main listener, outside the authentication middleware. It is mounted only when a webhook secret is configured, and refuses to construct without one. The body is read through `http.MaxBytesReader` (a larger `Content-Length` is rejected first) and the signature is checked before any parsing or database access. Handled deliveries run synchronously on `context.WithoutCancel(r.Context())` with a 25 second timeout, so a GitHub hang-up does not abort half-done work; graceful shutdown waits for them. Responses are tiny JSON bodies (`{"status":"processed"|"ignored"|"duplicate"}` or `{"error": ...}`); bodies and signatures are never logged. GitHub stops waiting after 10 seconds, so a slower delivery shows as timed out in GitHub even though doorstep finished it.
- **Deduplication.** Only `push` deliveries are claimed: `Deliveries.Claim` (`PostgresDeliveries`, over `webhook_deliveries`) inserts the delivery ID, `Complete` marks it done after success, and a failure releases it and answers `500` so a manual redelivery works. A completed ID is a `200` duplicate; an ID still being handled is a `409` (`in_progress`), unless its claim is older than the handler timeout plus ten seconds, in which case the attempt that claimed it crashed and the delivery is claimed again. `check_run`, `check_suite` and `status` deliveries are not recorded: re-evaluation is idempotent and costs no GitHub call for a commit intake does not track, and these events are the bulk of the traffic. `Sweeper` deletes rows older than seven days every hour while the route is mounted ([database.md](database.md#webhook_deliveries)).
- **Polling.** `intake.NewPoller` reads one branch head per distinct (repository, branch) across apps and asks `Service.TracksHead` whether intake already tracks it, from the database only: every app following the branch has a row for the head that is not superseded, and no app has another open commit. Otherwise it calls `HandlePush` with `Source: poll` and `PushedBy: doorstep-poller`, which also covers a force-push back to a commit a webhook superseded whose delivery was lost. The poller keeps no state of its own, so any replica can run any cycle and the first cycle has no baseline. It then lists up to 100 open commits, least recently evaluated first so a backlog rotates, and re-evaluates those not already handled this cycle and not evaluated in the last half interval. Each app has at most one open commit, so the limit only matters past 100 apps. Errors for one branch or commit are joined and do not stop the cycle. A missing branch is skipped with a warning.
- **One poller at a time.** `ClaimPoll` leases the single-row `intake_poll_state` table using Postgres `now()` ([database.md](database.md#intake_poll_state)); it holds no lock across GitHub calls. The cycle is bounded by `max(interval, 1m)` and the lease by that plus five seconds, so a slow cycle cannot overlap the next one. `FinishPoll` then schedules the next cycle one interval after this one ended. A replica that dies mid-cycle delays polling until its lease expires.
- **Installation cache.** `installation` and `installation_repositories` events call `(*github.App).EvictInstallation` and `EvictRepository` on the replica that received them. Other replicas recover through the one-hour lookup cache and the existing `404`/`403` handling.
- **Observer.** `intake.Observer` (`WebhookDelivery(event, result)`, `PollCycle(result)`) is adapted in `cli` to `doorstep_webhook_deliveries_total{event,result}` and `doorstep_intake_poll_cycles_total{result}` ([metrics.md](metrics.md)).
- **Configuration.** `github.webhook_secret_file` / `DOORSTEP_GITHUB_WEBHOOK_SECRET(_FILE)` (read once at startup) and `intake.poll_interval` / `DOORSTEP_POLL_INTERVAL` (default `1m`, `0` disables, at least `10s`); see [configuration.md](configuration.md#intake). `serve` runs the poller and the sweeper through `runServers`, so they stop with the listeners.

## GitOps pin writer

`internal/gitops` writes a digest into the GitOps repository through the GitHub API, without a clone, and reports the commit Argo CD should sync. The operator reference, with examples, is [gitops.md](gitops.md); the decision record is [ADR 0005](decisions/0005-pin-commits-and-trailers.md).

```go
writer := gitops.New(app, gitops.Options{Logger: logger})
result, err := writer.Pin(ctx, gitops.Request{Target: target, Digest: digest, Deployment: id, Kind: core.KindDeploy, AppCommit: commit})
```

- Every call is idempotent, blocking and bound to its context. `Pin` first reads the pin file on the branch; if it already holds the desired value, nothing is written and the result points at the existing commit among the last 20 touching the file: the one whose trailers name this app, digest and deployment (with its `Doorstep-Previous-Digest`), else the newest naming this app and digest. When neither exists `Result.Commit` is empty and the engine must not record a pin commit for the deployment; it never gets another app's commit. A restart therefore resumes exactly where it stopped, and nothing about the write is stored in Postgres beyond `Result.Commit` and `Result.Previous`.
- The engine serialises pins per target (one active deployment per target). If a stale `Pin` is replayed after its pin landed and a newer one replaced it, both modes fail with `ErrPinOverwritten` instead of writing the old digest back: commit mode finds this deployment's trailers in the history, pull request mode finds its merged pull request.
- Editing finds the target scalar with the yaml v3 `Node` API and splices bytes in the original text; it never re-encodes the document, so comments, blank lines, quoting, indentation and CRLF line endings survive and the diff is one line. The edited file is parsed again and must hold the desired value with the same node count (two more when a kustomize `digest` key is inserted). Block and multi-line scalars, anchors, aliases, merge keys, explicit tags, null values and flow mappings that need an insert fail with `ErrUnsupportedYAML` instead of guessing.
- Commit mode: `GET` the file from the Contents API, edit, `PUT` it with the blob `sha` and `branch`. Only a concurrent edit of the same file conflicts (`409` or `422` whose message is about the blob sha); doorstep re-reads, re-edits and retries up to `Options.MaxAttempts` (5) with jittered exponential backoff from 200 ms capped at 10 s, then returns `ErrConflict` wrapping GitHub's error. Branch protection and ruleset refusals (`GH006`, `GH013`, "rule violations", "through a pull request") return `ErrBranchProtected` at once; other refusals are returned unretried. GitHub signs the commit as the App.
- Pull request mode: the head branch is `doorstep/<app>-<deployment id>`, created from the base, or force-reset to the base when it exists without an open pull request. The edited file is written to it, the pull request is found by head and base or opened (adopting one a concurrent run opened first), and doorstep polls every `Options.PollInterval` (10 s) for up to `Options.MergeTimeout` (30 min). It calls update-branch when GitHub reports `behind` (ignoring "head moved" and "nothing to update" refusals, restarting on a merge conflict, failing on anything else), closes and reopens from the current base when it reports `dirty` (at most three times), and otherwise tries a squash merge pinned to the head SHA it validated, treating `405` and `409` as "not yet". A timeout returns `ErrMergeTimeout` and leaves the pull request open for the next call. A pull request merged by someone else counts only if the base then holds the desired value; otherwise `Pin` returns `ErrPinOverwritten`.
- `Request.Repin` skips the overwrite check for a deliberate second pin by the same deployment: commit mode writes the value again even though this deployment's earlier pin commit is in the history, and pull request mode ignores the deployment's merged pull request and opens a new one from the same head branch. The engine sets it only when a rollback confirms its pin ([rollback.md](rollback.md#the-late-pin-race)); a replayed `Pin` never does.
- Before every merge attempt the pull request must come from the GitOps repository itself (not a fork) and change exactly one file, or doorstep refuses with `ErrPullRequestTampered`. If that file differs from doorstep's own edit of the current base, doorstep writes its edit again before merging, so the merged content is always doorstep's.
- `Current` returns the pinned digest at the branch head (empty, not an error, when the value is not a digest yet). `Abandon` closes the deployment's pull request and deletes its branch.
- The commit subject is `deploy(<app>): pin <image>@sha256:<12 hex>` or `rollback(...)`, and the last paragraph carries `Doorstep-*` trailers (app, deployment, kind, app commit, image, digest, previous digest, rollback target, blocks, unblocks, URL), so the git log alone is enough to find previous digests and blocks. `Request.Blocks` and `Request.Unblocks` carry the app's pending block trailers, which ride on its next pin commit ([ADR 0009](decisions/0009-blocks-in-postgres-mirrored-to-git.md)).
- No retry of `5xx` or network errors inside the adapter; as with `registry` and `github`, transient retry policy belongs to the engine.

| Error | Cause | What callers do |
| --- | --- | --- |
| `ErrInvalidRequest` | the request failed validation inside the adapter | never retry |
| `ErrPinNotFound` | no pin file, no `images` entry with that name, or nothing at the YAML path | fix the GitOps repository or configuration |
| `ErrAmbiguousPin` | duplicate entries, duplicate keys, or the path in several documents | fix the GitOps repository |
| `ErrUnsupportedYAML` | the value cannot be edited without guessing | simplify the YAML around the pin |
| `ErrConflict` | writes kept conflicting, or the pull request kept conflicting with the base; wraps GitHub's message | retry later |
| `ErrBranchProtected` | branch protection or a ruleset refused a direct commit | do not retry; fix the bypass list or use `pull_request` mode |
| `ErrPinOverwritten` | this deployment's pin landed but the branch now holds another value | do not retry; the deployment did not stay pinned |
| `github.ErrRefNotFound` | the GitOps branch is missing or the repository is not visible to the installation | fix the configuration |
| `ErrPullRequestClosed` | someone closed doorstep's pull request without merging | treat as cancelled |
| `ErrPullRequestTampered` | the pull request comes from a fork or changes other files | investigate; doorstep does not merge it |
| `ErrMergeTimeout` | not mergeable within `MergeTimeout` | call `Pin` again to keep waiting, or `Abandon`; the engine retries it until its 45 minute pin deadline |
| `github.ErrFileTooLarge`, `github.ErrNotAFile` | the pin file is over 1 MiB, or a directory, symlink or submodule | fix the GitOps repository |
| `*github.APIError` | any other GitHub refusal, such as `403` or `404` | inspect `StatusCode` |
## Notifications

`internal/notify` is a sink for `core.Notification` values; it decides nothing about when they are emitted. The engine emits deploy events as deployments move through their states and the locks code emits lock events. `core.Notification` carries the deployment (with commit, digests and failure reason), the deployment a rollback reverts, or the lock, which is more than the `core.Event` history row holds.

```
engine ── Notify(n) ──► Dispatcher: validate, route by app, enqueue (no I/O)
                           │
             ┌─────────────┼──────────────────┐
             ▼             ▼                  ▼
        queue + worker  queue + worker   queue + worker      one per target, 64 deep
         Slack "ops"    webhook "hooks"  GitHub deployments
             │             │                  │
             └── retry: 4 attempts, 1s/2s/4s ±20%, 10 s each, Retry-After ≤ 30 s
```

- Routing: `apps[].notify` names the Slack and webhook targets of each app; the GitHub reporter is global and skips apps with `source.deployments: false`. The gate commit status mirror (`notify.GateStatuses`) is not a notification target: it is a `gates.Observer` (satisfied structurally, so `notify` does not import `gates`), skips apps with `source.statuses: false`, and coalesces to the latest status per commit and context ([ADR 0014](decisions/0014-external-gates-and-commit-status-mirror.md)). Each target filters by event kind (`notifications[].events`, six kinds by default).
- Delivery is in memory and best effort ([ADR 0006](decisions/0006-notifications-are-best-effort.md)): per-target queues keep order and isolate slow targets, a full queue drops and counts, and `Dispatcher.Close(ctx)` drains until the shutdown deadline. Outcomes are counted in `doorstep_notifications_total`.
- `notify` does not import `config` or `metrics`. `cli` maps the catalog to `notify.SlackOptions`, `notify.WebhookOptions` and routes, passes secrets as closures over `SecretRef.Read` (read on every send), and adapts `metrics.Recorder` to `notify.Observer`. `cli.newNotifier` builds the dispatcher; `serve` hands it to the engine and, through `cli.lockNotifier`, to the lock service and reminder, and closes it with the shutdown timeout after every worker has stopped. A global freeze has no app, so `lockNotifier` sends it once per app, skipping apps whose targets were already covered.
- The GitHub reporter keeps no state that matters: it finds the GitHub deployment by commit, environment (the app name) and the `doorstep_deployment_id` in its payload, or creates it, and caches IDs in a bounded map until the deployment ends. A `403` or `404` that is not a rate limit is a permission problem and is not retried; a rate-limited `403` or `429` is retried, honouring `Retry-After` or `x-ratelimit-reset`. Status descriptions never carry the failure reason, because GitHub shows them to every reader of the repository.

## State machine and concurrency

Deployment state is persisted in Postgres:

```
queued → resolving → pinning → syncing → verifying → succeeded
                                              │
                                              └────► failed
```

Any of `resolving`, `pinning`, `syncing` and `verifying` can also move to `failed` (an unresolvable tag or a GitHub error fails a deploy before verification), and so can `queued` when a `before` gate fails: the engine evaluates the `before` phase while the deployment is still `queued` and releases it with a delay while that phase is pending, so a deploy waiting for a window does not hold its target in flight ([engine.md](engine.md#before-gates-hold-the-deployment-in-queued)). Any non-terminal deployment can become `cancelled` (operator action), and a `queued` or `resolving` deployment can become `superseded` (a newer deployment for the same target was queued before this one started pinning).

- Terminal states: `succeeded`, `failed`, `cancelled`, `superseded`. A terminal state has no outgoing transitions.
- Active states: `resolving`, `pinning`, `syncing`, `verifying`. At most one deployment per target is active.
- The legal transitions are a table in `core` (`State.CanTransitionTo`), not a switch.
- A rollback is a separate deployment of kind `rollback` with `rollback_of_id` set, queued at a higher priority, and it goes through the same states ([ADR 0002](decisions/0002-rollback-is-a-new-deployment.md)). When a deploy's gates fail, the engine moves it to `failed`, removes its queue entry and enqueues the rollback in one transaction. A deploy still in flight when a rollback is requested is `cancelled`, and so are queued deploys of the commit the rollback blocks; other queued deploys run after the rollback ([rollback.md](rollback.md)).

- Every transition is a compare-and-set on `(id, from_state)` through `store.TransitionDeployment` and is recorded in history in the same transaction; a lost race returns `core.ErrStateConflict` ([database.md](database.md#transitions-and-lineage)).
- Every step is idempotent and resumable: after a restart a worker re-claims the deployment and re-runs the current step. Pinning compares the desired digest with the file in Git before committing; watching keys on the commit SHA and resumes with the remaining sync time. One claim runs as many steps as it can.
- Workers claim work with `SELECT ... FOR UPDATE SKIP LOCKED` and hold a lease column refreshed by heartbeat. An expired lease makes the row claimable again, so a crashed replica never wedges a target. A claim only takes the head entry of a target whose deployment is `queued` or active, and never while another entry of that target holds a live lease. Lease expiry uses the database clock; see [database.md](database.md#claiming-work).
- A target's queue runs in-flight deployments first, then by priority (`normal` 0, `urgent` 100, `rollback` 200), then oldest first. An urgent deploy jumps queued work but never preempts an in-flight deployment.
- Claiming a queued deploy batches it: the deploy with the newest commit among the first `deploy.max_batch` queued deploys at the head's priority runs and the older ones become `superseded`; an urgent deploy also supersedes every older queued normal deploy of an older commit. Commit age is the order in which commits were first enqueued. Rollbacks never batch and are never superseded ([ADR 0005](decisions/0003-queue-batching-and-urgent.md), [queue.md](queue.md)).
- Enqueueing a deploy is idempotent per app and commit SHA while a deploy of that commit is queued or in flight.
- `LISTEN/NOTIFY` is only a wake-up hint: a trigger on `queue_entries` notifies on enqueue, completion, release and priority changes, and `queue.Runner` also polls every 5 seconds, so a missed notification costs latency, not correctness.
- A partial unique index enforces at most one active deployment per (application, environment).
- Manual freeze locks are rows in a `locks` table, per app or global, visible and removable through the API and CLI. A live freeze stops queued deploys from being claimed; in-flight deployments and rollbacks are not blocked ([ADR 0004](decisions/0004-freeze-locks.md), [locks.md](locks.md)). Every replica sweeps expired locks and sends reminders for long-held ones once a minute.

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
| Deploy windows | `github.com/robfig/cron/v3` parser only, five fields, no descriptors; `time/tzdata` embedded | the parser Kubernetes CronJobs and Argo CD sync windows use, so expressions and daylight saving behaviour match what users know; no dependencies; embedded zone data makes zone names resolve the same in the distroless image, release binaries and on laptops ([ADR 0011](decisions/0011-built-in-gates-and-deploy-schedules.md)) |
| Kubernetes | hand-written REST client on `net/http` (`internal/kube`): one `GET` and one `WATCH` with a field selector per wait on `argoproj.io/v1alpha1` `Application`, decoded into private structs | doorstep needs two calls on one resource; avoids client-go, apimachinery and the argo-cd module and their dependency graphs; in-cluster auth is a CA file and a token file ([ADR 0008](decisions/0008-kubernetes-rest-client.md)) |
| Registry | `github.com/google/go-containerregistry` (`name`, `authn`, `v1/remote`, `remote/transport`) | `remote.Head` for tag to digest with proper registry auth: challenge parsing, token exchange, anonymous/basic/identity tokens and redirect handling are not hand-rolled. Its `DefaultKeychain` is not used. |
| GitHub | hand-written REST client on `net/http`; RS256 JWT with `crypto/rsa`; HMAC with `crypto/hmac` | a small set of endpoints (installations, tokens, contents, refs, commits, pull requests); small, auditable code instead of a large SDK |
| Metrics | `github.com/prometheus/client_golang` with a private registry | standard exposition, no global registry |
| Prometheus queries | hand-written client on `net/http` (`internal/promquery`), no PromQL parser | two endpoints and one JSON envelope do not justify pulling `prometheus/common` model types into the engine path, and the PromQL parser lives in the very large `prometheus/prometheus` module; Prometheus reports a bad query itself ([ADR 0013](decisions/0013-prometheus-query-gate.md)) |
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
- Responses are read through a 10 MiB limit, error messages from GitHub are cut to 512 bytes, and the default HTTP client times out after 30 s. Typed methods sit on top of `Client.Do`: `ListCheckRuns`, `ListCheckRunsNamed`, `CombinedStatus`, `ListCheckSuites`, `ListDeployments`, `CreateDeployment` (always `auto_merge: false` and no required contexts), `CreateDeploymentStatus`, `CreateCommitStatus` and `BranchHead` (see [Merge and CI detection](#merge-and-ci-detection). `BranchHead` validates branch names (`ErrInvalidBranch`) and reports a missing branch as `ErrBranchNotFound`.
- `App.Repo(ctx, owner, name)` returns a `Repo` handle with typed calls on top of `Client.Do`: `GetFile`/`PutFile` (Contents API; regular files of at most 1 MiB, `ErrNotAFile`, `ErrFileTooLarge`), `BranchSHA`/`CreateBranch`/`DeleteBranch` (`ErrRefNotFound`, `ErrRefExists`), `ListCommits`, and pull request list, create, get, update, merge and update-branch. Each path segment is escaped on its own and paths with empty, `.` or `..` segments or control characters are rejected (`ErrInvalidPath`). `HasStatus` classifies an `*APIError`. `WebBaseURL` derives the web host for links from the API URL.
- `VerifySignature` checks `X-Hub-Signature-256` webhook signatures against the raw body with a constant-time comparison; `EventHeader` and `DeliveryHeader` name the other webhook headers. `App.EvictInstallation` and `App.EvictRepository` drop cached lookups (and, for an installation, its token) when installation webhooks report a change.

Setup, permissions and the least-privilege model are described in [github-app.md](github-app.md).

## Release and supply chain

Version tags (`vX.Y.Z`) on `main` trigger a workflow that cross-compiles a `linux/amd64` and `linux/arm64` distroless image, pushes it to GHCR by digest, attaches SLSA build provenance and an SPDX SBOM as GitHub attestations, signs the image index keylessly with cosign, and publishes a GitHub Release with git-cliff notes. Images are tagged with the version and the full commit SHA, never `latest`. See [releasing.md](releasing.md).

## Security posture

- GitHub access is through one GitHub App installed on each owner with *Only select repositories*: one-hour installation tokens, held in memory only, refreshed five minutes before expiry, and only ever sent to the configured API host (requests and redirects to any other host are refused). GitHub applies the App's permissions to every selected repository, so the repository selection is what limits reach. Commits are attributed to and verified as the App.
- Pins are written through the GitHub Contents API using the blob SHA for optimistic concurrency: no local clone, no git binary, no SSH keys, no writable volume. Only regular files of at most 1 MiB are read or written, and every value written into a commit message is checked for control characters, so trailers cannot be injected.
- In pull request mode doorstep only merges its own pull request: the head must be a branch of the GitOps repository itself, the pull request must change exactly the pin file, that file must be byte-equal to doorstep's edit of the current base, and the merge is pinned to the head SHA that was checked. Anyone who can push to the branch therefore cannot use the App's write access to merge other changes.
- Webhook payloads are verified with HMAC-SHA256 and a constant-time comparison before they are parsed or anything is stored; the body is capped at 5 MiB, there is no unsigned mode (without a secret the route is not mounted), and the secret is redacted like the App key.
- Outbound notifications use one HTTP client with a 10 s timeout that never follows redirects, so signed payloads and secret webhook URLs stay on the configured host. Slack webhook URLs (and webhook URLs from `url_env`/`url_file`) are credentials: they are read per send, never logged, and removed from transport errors. Generic webhooks are signed with HMAC-SHA256 over the raw body (`X-Doorstep-Signature-256`). Targets on private addresses are allowed, since they are operator-configured and usually on the home network. Response bodies are read through a 4 KiB limit and only 256 bytes are quoted in logs.
- Kubernetes RBAC is `get`, `list` and `watch` on Argo CD `Application` resources only; doorstep uses `get` and `watch`. doorstep never patches, syncs or refreshes Applications. The install base grants exactly this; see [install.md](install.md#rbac). The opt-in `job-gate` component adds `get` on `batch/v1` Jobs for the `job` gate, and nothing that writes, lists or reads Pods, pod logs or Secrets ([install.md](install.md#validation-jobs)); `internal/kube` has no write methods.
- The Kubernetes client sends the ServiceAccount token (re-read from its file for every request, so bound tokens rotate) only to the configured API server, refuses redirects, trusts only the cluster CA bundle, ignores proxy environment variables and bounds every read. A kubeconfig is used only when named explicitly, and credential plugins are never executed ([argocd.md](argocd.md#connecting-to-the-cluster)).
- Only image digests are pinned, never mutable tags.
- Registry credentials are read only from the one explicitly configured file: no `$HOME/.docker`, no `DOCKER_CONFIG`, and credential helpers or stores are never executed. Credentials are sent only to the registry (and token realm) they match, are redacted when a `Credential` is formatted or logged, and errors never include file contents.
- Release images are signed with keyless cosign and carry provenance and SBOM attestations tied to the release workflow; verification steps are in [releasing.md](releasing.md).
- Secrets never appear inline in configuration: they come from environment variables or `*_file` paths (mounted Kubernetes Secrets). `Catalog.CheckSecrets` only proves at startup that each one resolves; code that needs a secret calls `SecretRef.Read` with `config.ReadSecretFile` (capped at 64 KiB) each time it uses it, so rotations apply without a restart and values are not kept in memory.
- The HTTP server sets read-header, read, write and idle timeouts and a header size limit; panics are recovered and logged.
- The public listener authenticates application routes in one middleware (not the built-in probes or `/metrics`): API tokens are 256-bit random values stored only as SHA-256 hashes, sessions are random tokens stored only as hashes in Postgres with an absolute lifetime, and session-authenticated unsafe requests must pass `http.CrossOriginProtection` and a per-session CSRF token. Sign-in uses the authorization code flow with PKCE, `state` and `nonce`, deny-by-default role mapping and a path-only return URL. Cookie security follows `server.public_url` (`__Host-`, `Secure`, `HttpOnly`, `SameSite=Lax`). Roles gate operating doorstep, not what ships ([ADR 0007](decisions/0007-authentication-and-roles.md)).
- Who requested a deploy, rollback, cancellation, lock, block, unblock or token change is recorded in an append-only `audit_log` table whose trigger rejects updates and deletes.
- CI pins every action by full commit SHA, runs with a read-only token and does not persist checkout credentials. Dependabot keeps pins and Go modules current.

## HTTP surface

One listener (default `:8080`) serves everything public:

| Path | Purpose | Status |
| --- | --- | --- |
| `GET /healthz` | liveness: always `200 {"status":"ok"}` while the process serves | built |
| `GET /readyz` | readiness: runs registered named checks (2 s timeout each, concurrently, results cached for 1 s), currently `postgres`; `200` when all pass, `503` when any fails, with each check reported only as `ok` or `fail`; `503 {"status":"shutting_down"}` once shutdown begins | built |
| `GET /metrics` | Prometheus exposition, on the metrics listener (default `:9090`) rather than this one unless `--metrics-listen-addr` is set empty (see [metrics.md](metrics.md)) | built |
| `GET /api/v1/apps/{app}/queue` | an app's queue in run order; viewer role (see [api.md](api.md)) | built |
| `POST /api/v1/apps/{app}/lock`, `DELETE /api/v1/apps/{app}/lock` | freeze or release one app; deployer role; see [locks.md](locks.md#api) | built |
| `POST /api/v1/lock`, `DELETE /api/v1/lock` | global freeze; deployer role | built |
| `GET /api/v1/locks` | live freezes; viewer role | built |
| `GET /api/v1/apps/{app}/deployments` | deploy history of an app, filtered and paginated; viewer role ([api.md](api.md)) | built |
| `GET /api/v1/deployments/{id}` | one deployment with its events, gates, rollbacks and retries ([api.md](api.md)) | built |
| `POST /api/v1/apps/{app}/rollback` | queue a rollback to the previous good digest and block the replaced version; deployer role ([rollback.md](rollback.md#api)) | built |
| `GET`, `POST /api/v1/apps/{app}/blocks`, `DELETE /api/v1/apps/{app}/blocks/{ref}` | list (viewer), add and clear (deployer) blocked versions | built |
| `POST /api/v1/deployments/{id}/gates/{gate}`, `POST /api/v1/apps/{app}/commits/{sha}/gates/{gate}` | post the result of an `external` gate; reporter role ([api.md](api.md#external-gate-results)) | built |
| `GET /auth/login` | starts sign-in at the configured provider; mounted only when one is configured ([authentication.md](authentication.md#the-sign-in-flow)) | built |
| `GET /auth/callback` | completes sign-in, creates the session and redirects to the return path | built |
| `POST /auth/logout` | ends the session; needs the CSRF token | built |
| `/api/v1/...` | further JSON API resources; unknown API paths answer with a JSON `404` | planned |
| `POST /webhooks/github` | GitHub App webhooks, signature-verified and outside sign-in; mounted only when a webhook secret is configured ([github-app.md](github-app.md#webhooks)) | built |
| `/` | web UI | planned |

The JSON API is one handler from `api.New`, mounted at `api.Prefix` (`/api/v1/`) on the main listener. `serve` wraps the application routes (not `/healthz`, `/readyz` or `/metrics`) in the authentication middleware, and `Options.Middleware` applies `RequireRole(viewer)` to every API route, so unauthenticated callers get `401` with `WWW-Authenticate: Bearer`. Lock, rollback and block changes and gate reports additionally call `api.Authenticator`, implemented in `internal/cli/serve.go` over the authenticated principal, which requires the deployer role, or the reporter role for gate reports (`403` otherwise) and returns the caller (`api.Caller{Actor, Role}`): the actor string is recorded as the lock owner, and the actor and role go into audit rows. `serve` builds `rollback.Service` over the store and passes it to the API; its `Observer` port is adapted to `metrics.Recorder.Rollback(app, manual)`.

Readiness checks are registered by name in `server.Options.Checks`; `serve` registers `postgres`, which pings the connection pool. `/readyz` is unauthenticated, so check errors (which can name hosts, users or databases) are only logged, never returned. Concurrent probes share one run of the checks, and the result is reused for one second, so probe traffic cannot multiply load on dependencies. Further handlers are mounted through `server.Options.Routes` (a map of `ServeMux` pattern to handler), which keeps them inside the request logging and panic recovery middleware. `serve` wraps the application routes in the authenticator before handing them over, so the built-in probe endpoints, `/metrics` and the signature-authenticated `POST /webhooks/github` (added after wrapping) stay outside sign-in; `/metrics` requests are logged at debug like probes.

`/metrics` is served by a second `server.Server` on its own listener (default `:9090`, `--metrics-listen-addr`), which also answers `/healthz` and `/readyz` with the same checks, and the main listener does not mount it. This keeps it off the public ingress that receives GitHub webhooks, as the metrics are unauthenticated and name apps and builds. Setting the address to empty is an explicit opt-in to mounting `/metrics` on the main listener. Server-mounted routes may not use the built-in `GET /healthz` and `GET /readyz` patterns; `server.New` panics on them. Metrics live in a private Prometheus registry owned by `metrics.Metrics` (no global registry); components record through the typed `metrics.Recorder`, and the queue depth is read lazily from the database at scrape time, bounded by a 2 second timeout.

On `SIGINT` or `SIGTERM` the server marks itself not ready and keeps serving for the shutdown delay (default 5 s), so `/readyz` answers `503 {"status":"shutting_down"}` while load balancers and Kubernetes endpoints stop routing to it. It then stops accepting connections and waits up to the shutdown timeout for in-flight requests before exiting. A second signal during shutdown terminates the process immediately.

## Testing strategy

- Standard library `testing` only (no testify), table-driven with `t.Run` and `t.Parallel`, `httptest` for HTTP.
- Engine logic is tested against hand-written fakes of every port; no Postgres, GitHub or Kubernetes needed. An integration test runs `serve` end to end against Postgres and the GitHub, registry and Argo CD fakes.
- Tests always run with the race detector and shuffled order (`just test`).
- Postgres integration tests read `DOORSTEP_TEST_DATABASE_URL` and call `t.Skip` when it is unset. Each test gets an isolated schema through `internal/store/storetest`.
- CI runs a `postgres:18-alpine` service (pinned by digest), exports `DOORSTEP_TEST_DATABASE_URL` and sets `DOORSTEP_TEST_REQUIRE_DATABASE=true`, so integration tests run on every pull request and a missing database fails the build instead of skipping.

- The queue runner, the intake poller and the webhook delivery sweeper are tested with fakes inside `testing/synctest` bubbles, so wake-ups, polling, heartbeats and backoff run on a fake clock.
- The Argo CD watcher is tested against `argocdtest`, a real HTTP fake of the Kubernetes API, so the wire format, field selectors, watch reconnects and timeouts are exercised; the assessment itself is a pure function with table tests. `internal/kube` is tested against TLS test servers with generated CAs and client certificates.

See [development.md](development.md) for the commands, [configuration.md](configuration.md) for runtime settings, [authentication.md](authentication.md) for sign-in, tokens and the audit log, [database.md](database.md) for the schema, [queue.md](queue.md) for queue behaviour, [engine.md](engine.md) for the deployment engine, [gates.md](gates.md) for gate evaluation, [argocd.md](argocd.md) for waiting on Argo CD, [api.md](api.md) for the HTTP API, [rollback.md](rollback.md) for rollbacks and blocks and [metrics.md](metrics.md) for the exported metrics.

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
| `internal/cli` | cobra command tree built by a constructor; `serve` is the composition root; `config validate` checks a configuration file; `lock`, `unlock` and `locks` call the API through a small shared client | built |
| `internal/config` | configuration model, defaults, environment overrides and validation; YAML file loader, app catalog (apps, gates, notification targets) and secret references | built |
| `internal/logging` | builds the injected `*slog.Logger` (JSON or text, level) | built |
| `internal/buildinfo` | version, commit and commit time from `-ldflags -X` link-time variables, overridden by VCS revision/time and the dirty flag from `runtime/debug` when present; Go version from `runtime/debug` | built |
| `internal/server` | HTTP lifecycle: `http.Server` with timeouts, graceful shutdown, middleware, `/healthz`, `/readyz` and readiness checks | built |
| `internal/core` | domain types (`Deployment`, `Target`, `State`, `Event`, `ImageRef`, `Digest`, ...), the state machine and shared errors such as `ErrNotFound`; imports nothing internal | built |
| `internal/store` | Postgres via pgx, embedded goose migrations, transactions, deployment creation, tracked commits, compare-and-set transitions with their event log, lineage, deploy history read models, queue claims with batching, leases and `LISTEN/NOTIFY` (claims honour freeze locks) and the freeze lock repository; see [database.md](database.md) | built |
| `internal/queue` | per-app deploy queue adapter: priorities, lease length, per-app batch limit, and the `Runner` worker loop (claims, wake-ups, polling, heartbeats); see [queue.md](queue.md) | built |
| `internal/locks` | manual freeze locks per app and global: validation, lock/unlock/list, `Blocking` for the engine, post-commit notifications through a `Notifier` port, and the expiry and reminder sweep; depends only on `core` through a `Repository` port that `*store.Store` satisfies; see [locks.md](locks.md). Per-target deployment locks are left to the engine | built |
| `internal/github` | GitHub App auth (RS256 JWT, cached installation tokens), small REST client, check runs, commit statuses and check suites, webhook signature verification | built |
| `internal/github/githubtest` | fake GitHub API for tests: verifies App JWTs, serves installation lookup and token minting, paginated check runs, statuses and check suites, authenticates extra handlers with installation tokens | built |
| `internal/intake` | merge and CI detection: tracks pushes to app branches, evaluates required checks and enqueues green commits exactly once; see [Merge and CI detection](#merge-and-ci-detection). The webhook handler and poller that call it, and API and CLI requests, are later beads | built |
| `internal/registry` | OCI tag to digest resolution through go-containerregistry, Docker `config.json` credentials matched per registry and path prefix | built |
| `internal/gitops` | pin writer: kustomize `images` and YAML-path strategies, committed through the GitHub API | planned |
| `internal/argocd` | read-only `Application` watcher through the Kubernetes API | planned |
| `internal/gates` | health, metrics and soak gates | planned |
| `internal/rollback` | selects the previous good digest and re-pins it | planned |
| `internal/notify` | Slack, ntfy and generic webhook notifications | planned |
| `internal/engine` | deployment orchestration state machine; declares the ports it consumes | planned |
| `internal/api` | HTTP JSON API (`/api/v1`): one mux, one file per resource, JSON error envelope, a middleware seam for authentication, queue, lock and deploy history endpoints; see [api.md](api.md) | built |
| `internal/web` | server-rendered UI with `html/template` and embedded htmx | planned |
| `internal/auth` | hashed API tokens for CLI/CI, OIDC login for the UI | planned |
| `internal/metrics` | Prometheus metrics on a private registry, typed recorder, lazy queue depth collector, dedicated listener | built |

Planned packages are created by the bead that implements them; there are no placeholder packages.

## Dependency rules

The design is ports and adapters.

- `core` imports nothing from `internal/`. Every other package may import it.
- `engine` declares the small interfaces it consumes (its ports) and depends only on `core` and those interfaces.
- Adapters (`store`, `queue`, `locks`, `registry`, `gitops`, `argocd`, `gates`, `rollback`, `notify`) implement engine ports. They never import `engine` or each other, with two exceptions: `gitops` and `intake` use `github`, and Postgres-backed adapters (`queue`, `locks`, `intake`'s `PostgresCommits`, and later `gates` persistence and `auth`) may import `store` for the connection handle, `InTx` and row helpers. `intake` imports only `core`, `github` and `store`; it does not import `config`.
- `store` imports only `core`.
- `api` and `web` call `engine` for commands and `store` read models for queries. History is part of `store` rather than a package of its own: a transition must update the deployment, append its event and delete its queue entry atomically through the store's transaction handle. They contribute handlers; `server` owns the listener, middleware and health endpoints. Lock commands are the exception: they do not involve a deployment, so `api` calls `locks.Service` directly through its own small `LockService` port.
- `cli` `serve` is the only composition root: it constructs adapters, injects them into `engine`, and mounts handlers on `server`.
- No package-level mutable state, no `init()`, no `slog.SetDefault`. Dependencies, including the logger and clock, are passed through constructors. The only exceptions are the link-time `buildinfo.version`, `buildinfo.commit` and `buildinfo.commitTime` variables, which the release image build sets because it has no `.git` directory. The store is the one place that does not take the injected clock: row timestamps and lease expiry use Postgres `now()`, the only clock all replicas share ([database.md](database.md#clock)).
- `context.Context` is the first parameter of anything that does I/O or may block.
- Errors are wrapped with `%w` and inspected with `errors.Is` / `errors.As`.

## Engine ports

Illustrative signatures; the engine bead finalises them.

```go
type DeploymentStore interface {
	Create(ctx context.Context, d core.Deployment) (core.Deployment, error)
	Get(ctx context.Context, id core.DeploymentID) (core.Deployment, error)
	TransitionDeployment(ctx context.Context, t store.Transition) (core.Deployment, error)
	SetLineage(ctx context.Context, id core.DeploymentID, l core.Lineage, actor string) (core.Deployment, error)
	LastSuccessfulDeployment(ctx context.Context, t core.Target, excludeDigest core.Digest) (core.Deployment, error)
}

type Queue interface {
	Enqueue(ctx context.Context, d core.Deployment, opts queue.EnqueueOptions) (queue.EnqueueResult, error)
	Expedite(ctx context.Context, id core.DeploymentID, actor string) error
	Claim(ctx context.Context, workerID string) (queue.Claimed, bool, error)
	Heartbeat(ctx context.Context, id core.DeploymentID, workerID string) (time.Time, error)
	Release(ctx context.Context, id core.DeploymentID, workerID string, delay time.Duration) error
	Complete(ctx context.Context, id core.DeploymentID, workerID string) error
}

type Locker interface {
	Blocking(ctx context.Context, t core.Target) ([]core.Lock, error)
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

`*queue.Queue` implements the `Queue` port today (see [queue.md](queue.md)); its `Claimed`, `EnqueueOptions` and `EnqueueResult` types live in `queue`, so the engine bead either moves them to `core` or declares the port over its own types with a thin adapter, keeping `engine` free of adapter imports. `queue.Runner` drives the engine through a `Handler` and is started by `serve` once the engine exists.

`Locker` is what `locks.Service` offers the engine: the live freezes (app and global) that block a target, so the engine can record the `lock` gate as pending with the owner and reason as evidence. The claim itself already skips frozen deploys ([locks.md](locks.md)). Per-target serialisation needs no lock rows: head-of-line claiming and the one-active-deployment index provide it, and kind `deployment` lock rows are reserved for the engine.

The store already implements the deployment side: `TransitionDeployment`, `SetLineage` and `LastSuccessfulDeployment` ([database.md](database.md#transitions-and-lineage)). The engine bead decides whether its port takes `store.Transition` as is or a mirror type in `core`, keeping `engine` free of a `store` import.

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

## Merge and CI detection

`internal/intake` decides when a commit merged to an app's branch becomes deployable. It is driven from outside: the GitHub webhook handler and the poller (a later bead) parse payloads and call it with plain strings.

```
push to app branch ──► Service.HandlePush(Push{Repository, Branch, SHA, Source, PushedBy})
                          │ match apps: repository case-insensitive, branch exact
                          ▼
                       Commits.Track      one tracked commit per app and SHA; older open commits
                          │                of the app are superseded in the same transaction
                          ▼
check_run / status ──► Service.Reevaluate(repository, sha)   tracked and open commits only;
poller             ──► Service.Pending(limit)                 untracked SHAs cost no API call
                          │
                          ▼
                       CheckSource.Checks  one snapshot per repository and SHA, shared by apps
                          │
                          ▼
                       Evaluate(required, snapshot) → pending | success | failure, evidence
                          │
                          ▼  success
                       Commits.Promote ── Queue.Enqueue(Deployment) in one transaction
```

- **Ports.** `Commits` (track, look up, save checks, promote, list pending), `CheckSource` (one `Snapshot` of check runs, commit statuses and, when asked, check suites) and `Queue` (`Enqueue(ctx, core.Deployment)`). `GitHubChecks` implements `CheckSource` over anything with `RepoClient`, such as `*github.App`. `PostgresCommits` implements `Commits` over `*store.Store` and is built with a factory `func(tx *store.Store) Queue`, so the queue the promotion uses is bound to the promotion's transaction.
- **States.** A tracked commit's `checks_state` is `pending`, `success` or `failure` (`core.ChecksState`). None is terminal: a failed commit can turn green after a CI re-run. A commit is final once it has a deployment (enqueued) or has been superseded; closed commits are never evaluated again and saving checks on them is a no-op.
- **Exactly once.** `store.PromoteTrackedCommit` locks the commit's app row and the tracked row, returns without enqueueing unless the commit is `success` and still open, calls the enqueue callback with the transaction-scoped store, and links the new deployment, all in one transaction. Webhook deliveries, the poller and several replicas can evaluate the same commit concurrently; one deployment is created, and an enqueue error rolls back both the deployment and the link.
- **Supersession.** Recording a new commit for an app supersedes every older commit of that app that is not yet enqueued. A slow CI run on an older commit therefore never deploys after a newer push. If the newest commit is red, the previous one does not deploy either; re-tracking a superseded SHA (for example after a force-push back to it) returns the superseded row, so that case needs a manual deploy. Queue supersession of deployments that already exist is a separate rule owned by the queue.
- **Evaluation.** `Evaluate(required, snapshot)` is pure and exported so the `checks` gate can reuse it. Rules are in [configuration.md](configuration.md#required-checks). Evidence is a list of `core.Check` sorted by name and stored as `jsonb` on the tracked commit.
- **Enqueued deployment.** `core.Deployment{Target, Kind: deploy, Source and RequestedBy from the push, Commit{Repository, SHA}, Image{ImageRepository, Tag: full SHA}}`, following the convention that images are tagged with the full commit SHA.
- **Errors.** A commit is recorded before GitHub is asked about it; if GitHub fails, the commit stays tracked as `pending` and the wrapped error is returned (`github.ErrNotInstalled` and friends stay inspectable with `errors.Is`). Failures for one app are joined with `errors.Join` and do not block other apps. There are no retries inside `intake`: webhook redeliveries, later check events and the poller retry.
- **Ignored input.** Pushes to repositories or branches no app follows and the all-zero SHA of a deleted branch return no results and no error. The caller strips `refs/heads/` before calling.
- **Wiring.** Nothing constructs `intake.Service` in `serve` yet. The engine bead maps each `config.App` to an `intake.App{Target, Repository, Branch, RequiredChecks, ImageRepository}` (including the target environment) and supplies the `Queue` factory over the queue adapter; the webhook bead calls `HandlePush` and `Reevaluate`, and the poller calls `Pending` and `Reevaluate`.
- **Not here.** `intake` applies no timeout to commits that stay pending (the `checks` gate owns the 30 minute timeout) and records no metrics yet.

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

- Every transition is a compare-and-set on `(id, from_state)` through `store.TransitionDeployment` and is recorded in history in the same transaction; a lost race returns `core.ErrStateConflict` ([database.md](database.md#transitions-and-lineage)).
- Every step is idempotent and resumable: after a restart a worker re-claims the deployment and re-runs the current step. Pinning compares the desired digest with the file in Git before committing; watching keys on the commit SHA.
- Workers claim work with `SELECT ... FOR UPDATE SKIP LOCKED` and hold a lease column refreshed by heartbeat. An expired lease makes the row claimable again, so a crashed replica never wedges a target. A claim only takes the head entry of a target whose deployment is `queued` or active, and never while another entry of that target holds a live lease. Lease expiry uses the database clock; see [database.md](database.md#claiming-work).
- A target's queue runs in-flight deployments first, then by priority (`normal` 0, `urgent` 100, `rollback` 200), then oldest first. An urgent deploy jumps queued work but never preempts an in-flight deployment.
- Claiming a queued deploy batches it: the deploy with the newest commit among the first `deploy.max_batch` queued deploys at the head's priority runs and the older ones become `superseded`; an urgent deploy also supersedes every older queued normal deploy of an older commit. Commit age is the order in which commits were first enqueued. Rollbacks never batch and are never superseded ([ADR 0003](decisions/0003-queue-batching-and-urgent.md), [queue.md](queue.md)).
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
- Responses are read through a 10 MiB limit, error messages from GitHub are cut to 512 bytes, and the default HTTP client times out after 30 s. Typed methods sit on top of `Client.Do`: `ListCheckRuns`, `CombinedStatus` and `ListCheckSuites` (see [Merge and CI detection](#merge-and-ci-detection)); later beads add contents, pull requests and status writes.
- `VerifySignature` checks `X-Hub-Signature-256` webhook signatures against the raw body with a constant-time comparison.

Setup, permissions and the least-privilege model are described in [github-app.md](github-app.md).

## Release and supply chain

Version tags (`vX.Y.Z`) on `main` trigger a workflow that cross-compiles a `linux/amd64` and `linux/arm64` distroless image, pushes it to GHCR by digest, attaches SLSA build provenance and an SPDX SBOM as GitHub attestations, signs the image index keylessly with cosign, and publishes a GitHub Release with git-cliff notes. Images are tagged with the version and the full commit SHA, never `latest`. See [releasing.md](releasing.md).

## Security posture

- GitHub access is through one GitHub App installed on each owner with *Only select repositories*: one-hour installation tokens, held in memory only, refreshed five minutes before expiry, and only ever sent to the configured API host (requests and redirects to any other host are refused). GitHub applies the App's permissions to every selected repository, so the repository selection is what limits reach. Commits are attributed to and verified as the App.
- Pins are written through the GitHub Contents/Git Data API using the blob SHA for optimistic concurrency: no local clone, no git binary, no SSH keys, no writable volume.
- Webhook payloads are verified with HMAC-SHA256 and a constant-time comparison before they are parsed.
- Kubernetes RBAC is `get`, `list` and `watch` on Argo CD `Application` resources only. doorstep never patches or syncs Applications.
- Only image digests are pinned, never mutable tags.
- Registry credentials are read only from the one explicitly configured file: no `$HOME/.docker`, no `DOCKER_CONFIG`, and credential helpers or stores are never executed. Credentials are sent only to the registry (and token realm) they match, are redacted when a `Credential` is formatted or logged, and errors never include file contents.
- Release images are signed with keyless cosign and carry provenance and SBOM attestations tied to the release workflow; verification steps are in [releasing.md](releasing.md).
- Secrets never appear inline in configuration: they come from environment variables or `*_file` paths (mounted Kubernetes Secrets). `Catalog.CheckSecrets` only proves at startup that each one resolves; code that needs a secret calls `SecretRef.Read` with `config.ReadSecretFile` (capped at 64 KiB) each time it uses it, so rotations apply without a restart and values are not kept in memory.
- The HTTP server sets read-header, read, write and idle timeouts and a header size limit; panics are recovered and logged.
- CI pins every action by full commit SHA, runs with a read-only token and does not persist checkout credentials. Dependabot keeps pins and Go modules current.

## HTTP surface

One listener (default `:8080`) serves everything public:

| Path | Purpose | Status |
| --- | --- | --- |
| `GET /healthz` | liveness: always `200 {"status":"ok"}` while the process serves | built |
| `GET /readyz` | readiness: runs registered named checks (2 s timeout each, concurrently, results cached for 1 s), currently `postgres`; `200` when all pass, `503` when any fails, with each check reported only as `ok` or `fail`; `503 {"status":"shutting_down"}` once shutdown begins | built |
| `GET /metrics` | Prometheus exposition, on the metrics listener (default `:9090`) rather than this one unless `--metrics-listen-addr` is set empty (see [metrics.md](metrics.md)) | built |
| `GET /api/v1/apps/{app}/queue` | an app's queue in run order; served only on the metrics listener until the auth work lands (see [api.md](api.md)) | built |
| `POST /api/v1/apps/{app}/lock`, `DELETE /api/v1/apps/{app}/lock` | freeze or release one app; see [locks.md](locks.md#api) | built |
| `POST /api/v1/lock`, `DELETE /api/v1/lock` | global freeze | built |
| `GET /api/v1/locks` | live freezes | built |
| `GET /api/v1/apps/{app}/deployments` | deploy history of an app, filtered and paginated; same listener as the queue endpoint ([api.md](api.md)) | built |
| `GET /api/v1/deployments/{id}` | one deployment with its events, gates, rollbacks and retries ([api.md](api.md)) | built |
| `/api/v1/...` | further JSON API resources; unknown API paths answer with a JSON `404` | planned |
| `/webhooks/github` | GitHub App webhooks | planned |
| `/` | web UI | planned |

The JSON API is one handler from `api.New`, mounted at `api.Prefix` (`/api/v1/`) on the metrics listener only, and not at all when that listener is disabled, until authentication exists; its `Options.Middleware` wraps every API route, which is where authentication plugs in.

Lock routes authenticate through `api.Authenticator` and answer `401` with `WWW-Authenticate: Bearer` when it fails. API tokens are not issued yet, so `serve` currently wires `api.RejectAll` through `apiAuthenticator` in `internal/cli/serve.go`, which the token work replaces.

Readiness checks are registered by name in `server.Options.Checks`; `serve` registers `postgres`, which pings the connection pool. `/readyz` is unauthenticated, so check errors (which can name hosts, users or databases) are only logged, never returned. Concurrent probes share one run of the checks, and the result is reused for one second, so probe traffic cannot multiply load on dependencies. Further handlers are mounted through `server.Options.Routes` (a map of `ServeMux` pattern to handler), which keeps them inside the request logging and panic recovery middleware; `/metrics` requests are logged at debug like probes.

`/metrics` is served by a second `server.Server` on its own listener (default `:9090`, `--metrics-listen-addr`), which also answers `/healthz` and `/readyz` with the same checks, and the main listener does not mount it. This keeps it off the public ingress that receives GitHub webhooks, as the metrics are unauthenticated and name apps and builds. Setting the address to empty is an explicit opt-in to mounting `/metrics` on the main listener. Server-mounted routes may not use the built-in `GET /healthz` and `GET /readyz` patterns; `server.New` panics on them. Metrics live in a private Prometheus registry owned by `metrics.Metrics` (no global registry); components record through the typed `metrics.Recorder`, and the queue depth is read lazily from the database at scrape time, bounded by a 2 second timeout.

On `SIGINT` or `SIGTERM` the server marks itself not ready and keeps serving for the shutdown delay (default 5 s), so `/readyz` answers `503 {"status":"shutting_down"}` while load balancers and Kubernetes endpoints stop routing to it. It then stops accepting connections and waits up to the shutdown timeout for in-flight requests before exiting. A second signal during shutdown terminates the process immediately.

## Testing strategy

- Standard library `testing` only (no testify), table-driven with `t.Run` and `t.Parallel`, `httptest` for HTTP.
- Engine logic is tested against hand-written fakes of its ports; no Postgres, GitHub or Kubernetes needed.
- Tests always run with the race detector and shuffled order (`just test`).
- Postgres integration tests read `DOORSTEP_TEST_DATABASE_URL` and call `t.Skip` when it is unset. Each test gets an isolated schema through `internal/store/storetest`.
- CI runs a `postgres:18-alpine` service (pinned by digest), exports `DOORSTEP_TEST_DATABASE_URL` and sets `DOORSTEP_TEST_REQUIRE_DATABASE=true`, so integration tests run on every pull request and a missing database fails the build instead of skipping.

- The queue runner is tested with fakes inside `testing/synctest` bubbles, so wake-ups, polling, heartbeats and backoff run on a fake clock.

See [development.md](development.md) for the commands, [configuration.md](configuration.md) for runtime settings, [database.md](database.md) for the schema, [queue.md](queue.md) for queue behaviour, [api.md](api.md) for the HTTP API and [metrics.md](metrics.md) for the exported metrics.

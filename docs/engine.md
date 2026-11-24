# Deployment engine

`internal/engine` drives one deployment at a time through its states. It decides nothing about what to deploy (intake, the API and rollback requests queue deployments) and it touches nothing directly: every effect goes through a port that `cli serve` fills with an adapter. The decision record is [ADR 0012](decisions/0012-deployment-engine.md).

```
queue.Runner ── claim ──► Engine.Handle(Claim)
                              │ announce superseded deployments of this claim
                              ▼
     queued ──── admit ────► resolving ── resolve ──► pinning ── pin ──► syncing ── sync ──► verifying ── verify ──► succeeded
       │ before gates          │ registry, blocks      │ gitops          │ argocd             │ after gates
       │ pending: release 30s  │ not pushed: 15s       │                 │                    │ pending: release 10s
       ▼                       ▼                       ▼                 ▼                    ▼
     failed                  failed                  failed            failed               failed
```

## Running

`serve` starts one `queue.Runner` per replica with the engine as its handler whenever at least one app is configured; without apps it logs `engine idle: no apps configured`. The runner claims the head of a target's queue, heartbeats the lease and releases or completes the entry ([queue.md](queue.md#runner)). Several replicas run several runners; the claim guarantees one worker per target, and the one-active-deployment index guarantees one active deployment per target.

With apps configured, `serve` needs the GitHub App (to pin) and a Kubernetes connection (to read Argo CD Applications: in-cluster, or `--kubeconfig`/`--kube-context`) and refuses to start without them, so a server that accepts apps can always deploy them.

## Steps

`Engine.Handle` runs the step for the deployment's current state, then the next, inside one claim, until the deployment is terminal, a step asks to be released with a delay, or an error or cancellation stops it. Every step re-derives what to do from the persisted deployment, so a re-claim after a crash or a lost lease resumes at the current state.

| State | Step | Success | Not yet | Fails when |
| --- | --- | --- | --- | --- |
| `queued` | evaluate the `before` gates | `queued → resolving` (the deploy *starts*: `started_at`, `deploy.started`, `doorstep_deployments_started_total`) | gates `pending`: release, claim again in 30 s | the app is no longer configured; a gate failed |
| `resolving` | rollbacks: use the preset digest. Deploys: resolve `image:commit-sha` to a digest, then refuse a blocked digest | `resolving → pinning` with `digest` | image not pushed (`404`) or denied (`401`/`403`, logged at `warn`): release, retry in 15 s | invalid reference; digest blocked (reason names the block); image not available within 15 minutes; other errors past 15 minutes |
| `pinning` | read pending block trailers, `Pin`, mark trailers mirrored when the pin wrote a commit | `pinning → syncing` with `pin_commit_sha`, `previous_digest`, `gitops_repository` | | refused by the GitOps repository (see below); other errors past 45 minutes |
| `syncing` | wait until Argo CD synced a revision containing the pin commit and the Application is healthy | `syncing → verifying` with `argocd_revision` | | sync failed, timed out, Application not found or forbidden |
| `verifying` | evaluate the `after` gates; for rollbacks confirm the pin is still current | `verifying → succeeded` | gates `pending`: release, claim again in 10 s | a gate failed |

### Before gates hold the deployment in `queued`

A `schedule` or `lock` gate can keep a deploy waiting for hours or days. While its before phase is pending the deployment stays `queued` and the runner releases its entry with a 30 second delay instead of holding the lease. Claims order in-flight deployments first, so a deployment held in `resolving` would block rollbacks and urgent deploys of its target; held as `queued` they still outrank it, and each re-claim batches again, so the newest commit wins when the window opens. A failed before gate moves the deployment straight from `queued` to `failed` (the one transition from `queued` other than `resolving`, `cancelled` and `superseded`), without a spurious start.

### Error classification and deadlines

Adapters in `internal/cli` wrap their errors in one of two engine sentinels; anything else is transient.

| Error | Source | What the engine does |
| --- | --- | --- |
| `engine.ErrPermanent` | `registry.ErrInvalidReference`; `gitops.ErrInvalidRequest`, `ErrPinNotFound`, `ErrAmbiguousPin`, `ErrUnsupportedYAML`, `ErrBranchProtected`, `ErrPinOverwritten`, `ErrPullRequestClosed`, `ErrPullRequestTampered`, `ErrMergeTimeout`; `github.ErrFileTooLarge`, `ErrNotAFile`; an app with no GitOps target | fails the deployment at once |
| `engine.ErrImageNotReady` | `registry.ErrNotFound`, `registry.ErrDenied` | releases and retries every 15 s until the resolve deadline |
| `core.ErrSyncFailed`, `ErrSyncTimeout`, `ErrSyncTargetNotFound`, `ErrSyncForbidden` | the Argo CD watcher | fails the deployment with the sync failure's reason and class |
| `core.ErrStateConflict` | a transition lost its compare-and-set | stops quietly: someone else (a cancel, a rollback request, another worker) changed the deployment |
| context cancelled | shutdown or a lost lease | stops without failing anything; the runner releases the entry |
| anything else | network errors, `5xx`, rate limits | returned to the runner, which releases the entry with backoff (5 s doubling to 5 minutes) |

Retries are bounded by how long the deployment has been in its state, measured from the transition event that entered it (`store.StateEnteredAt`, the database clock), so a flaky dependency never wedges a target:

| State | Deadline |
| --- | --- |
| `resolving` | 15 minutes |
| `pinning` | 45 minutes, longer than the GitOps pull request merge timeout (30 minutes) |
| `syncing` | the app's `argocd.sync_timeout` (default 10 minutes) |
| `queued`, `verifying` | the gates' own timeouts ([gates.md](gates.md)) |

Past its deadline the deployment fails with the last error as its reason and class `transient`.

### Resuming a sync wait

`WaitSynced` applies the full per-app timeout on every call, so the engine passes the remaining time as a context deadline: `max(entered syncing + sync_timeout, now + 10 s)`. The 10 second floor guarantees one final assessment after a long outage. When that deadline fires the deployment fails as a sync timeout with the last observed status; when the parent context ends (shutdown, lost lease) the wait just stops and the next claim resumes it.

If `Pin` reports no attributable commit (the value was already in git, set by hand or older than the last 20 commits), there is no revision to wait for. The deployment moves to `syncing` without a pin commit and the sync step polls the Application's health every 10 seconds until it is `Healthy`, within the sync timeout, then moves to `verifying` with the note `pin already current; no pin commit to wait for`.

### Rollbacks

A rollback ([rollback.md](rollback.md)) goes through the same states. It skips resolving and the block check and pins its preset digest; a rollback without a digest fails at once. The `checks`, `lock` and `soak` gates, and `schedule` without `apply_to_rollbacks`, are recorded as `skipped` for rollbacks ([gates.md](gates.md#rollbacks)); `health` still runs.

Before `verifying → succeeded` the engine reads the digest pinned at the GitOps branch head. If it is not the rollback's digest (a cancelled deploy's worker landed its pin late, see [the late-pin race](rollback.md#the-late-pin-race)), it pins again, records the new pin commit, waits for Argo CD to sync it with a fresh sync timeout and evaluates again, all while staying in `verifying`. `ErrPinOverwritten` from that pin fails the rollback: it means someone changed the pin by hand and a person should act.

### Failure policy

Every failure, from any step, goes through one method that builds an `engine.Failure` (deployment, stage, class, reason, sync failure, gate outcome) and asks the `FailurePolicy`. The only action today is `fail`, which is what the default `FailPolicy` returns; an error or an unknown action is logged and treated as `fail`. Retrying transient failures (doorstep-ehl.6.2) and automatic rollback on version failures (doorstep-ehl.6.5) plug in here. Until automatic rollback lands, the pin of a deploy that failed after pinning stays in git until someone requests a rollback.

The failure reason is cleaned to one line of at most 500 characters. The `failed` transition event carries `{"stage", "failure_class", "sync_reason"}` as data. A deploy that failed in `pinning` abandons its pull request, if any.

## Pins of stopped deployments

`Abandon` closes a deployment's pull request and deletes its branch; it is idempotent and a no-op in commit mode. It is called when:

- a cancelled deployment had already started (from the post-commit announcement below);
- a worker's pin step is interrupted by a state conflict or a lost lease and the reloaded deployment is `cancelled` or `superseded` (the worker may open its pull request after the canceller's `Abandon` ran);
- a deployment fails while `pinning`.

Deployments superseded straight from `queued` never had a branch, so they are not abandoned.

## Notifications and metrics

The worker emits each `deploy.*` event right after its own successful transition, so exactly one replica announces it. A crash between the transition and the notification loses the notification, which [ADR 0006](decisions/0006-notifications-are-best-effort.md) accepts.

| Event | Emitted by | Metric |
| --- | --- | --- |
| `deploy.queued` | `Engine.Queued`, called after the enqueue committed: by a decorator around intake's settlement and by a decorator around rollback requests in `cli` | |
| `deploy.started` | the worker, on `queued → resolving` | `doorstep_deployments_started_total` |
| `deploy.succeeded`, `deploy.rolled_back` | the worker, on `verifying → succeeded` (a rollback carries the deployment it reverts) | duration with outcome `succeeded` or `rolled_back` |
| `deploy.failed` | the worker, on `→ failed` | duration with outcome `failed` or `rollback_failed` |
| `deploy.superseded` | the worker that claimed the newer deployment, for each deployment the claim superseded | duration with outcome `superseded` |
| `deploy.cancelled` | `Engine.Cancelled`, after a rollback request cancelled deployments | duration with outcome `cancelled` |

`deploy.queued` is announced after the enqueue transaction commits, so a worker that claims the deployment at once can announce `deploy.started` first. The duration is from `started_at` to `finished_at`, zero when the deployment never started. Gate durations come from the gate evaluator.

## Ports

The engine imports only `core` and the standard library and declares the ports it uses, with its own small types (`Claim`, `Result`, `Transition`, `PinRequest`, `PinResult`, `Outcome`). The adapters live in `internal/cli`:

| Port | Adapter |
| --- | --- |
| `DeploymentStore` (`Get`, `Transition`, `SetLineage`, `StateEnteredAt`) | `engineStore` over `*store.Store` |
| `DigestResolver` | `digestResolver` over `registry.Resolver.ResolveCommit`, with credentials re-read on every call |
| `PinWriter` (`Pin`, `Current`, `Abandon`) | `pinWriter` over one `*gitops.Writer`, mapping each app to its `gitops.Target` |
| `SyncWatcher` | `*argocd.Watcher` |
| `GateEvaluator` | `*gates.Evaluator` built by `newGateEvaluator` |
| `BlockList` | `*rollback.Service` |
| `Notifier` | `*notify.Dispatcher` |
| `Metrics` | `engineMetrics` over `metrics.Recorder` |
| `FailurePolicy` | `engine.FailPolicy` |

`queueHandler` turns `queue.Claimed` into `engine.Claim` and `engine.Result` into `queue.Result`. There is no queue port: the runner claims, heartbeats and releases, and a terminal transition deletes the queue entry in the same transaction.

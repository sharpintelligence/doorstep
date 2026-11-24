# Deployment engine

`internal/engine` drives one deployment at a time through its states. It decides nothing about what to deploy (intake, the API and rollback requests queue deployments) and it touches nothing directly: every effect goes through a port that `cli serve` fills with an adapter. The decision record is [ADR 0012](decisions/0012-deployment-engine.md).

```
queue.Runner ── claim ──► Engine.Handle(Claim)
                              │ announce superseded deployments of this claim
                              ▼
                                                                       ┌─ retry ─┐ same pin, new window
                                                                       ▼         │ after a backoff
     queued ──── admit ────► resolving ── resolve ──► pinning ── pin ──► syncing ── sync ──► verifying ── verify ──► succeeded
       │ before gates          │ registry, blocks      │ gitops          │ argocd             │ after gates
       │ pending: release 30s  │ not pushed: 15s       │                 │                    │ pending: release 10s
       ▼                       ▼                       ▼                 ▼                    ▼
     failed                  failed                  failed            failed               failed
```

## Running

`serve` starts one `queue.Runner` per replica with the engine as its handler whenever at least one app is configured; without apps it logs `engine idle: no apps configured`. The runner claims the head of a target's queue, heartbeats the lease and releases or completes the entry ([queue.md](queue.md#runner)). Several replicas run several runners; the claim guarantees one worker per target, and the one-active-deployment index guarantees one active deployment per target.

Each replica handles at most `engine.concurrency` claims at once (`DOORSTEP_ENGINE_CONCURRENCY`, default 4, see [configuration.md](configuration.md#engine)). A claim is held for as long as its step runs: the pin step holds it while a pull request waits to merge, and the sync step while Argo CD syncs, up to the app's `argocd.sync_timeout` per attempt. With the default, a fifth target whose deployment is ready waits while four apps are syncing on that replica; raise the setting, or run more replicas, when more apps deploy at the same time. Before gates, pending after gates and the pause between [retry attempts](#retry-before-rollback) release the claim, so waiting on a gate or a backoff does not take a slot.

With apps configured, `serve` needs the GitHub App (to pin) and a Kubernetes connection (to read Argo CD Applications: in-cluster, or `--kubeconfig`/`--kube-context`) and refuses to start without them, so a server that accepts apps can always deploy them.

## Steps

`Engine.Handle` runs the step for the deployment's current state, then the next, inside one claim, until the deployment is terminal, a step asks to be released with a delay, or an error or cancellation stops it. Every step re-derives what to do from the persisted deployment, so a re-claim after a crash or a lost lease resumes at the current state.

| State | Step | Success | Not yet | Fails when |
| --- | --- | --- | --- | --- |
| `queued` | evaluate the `before` gates | `queued → resolving` (the deploy *starts*: `started_at`, `deploy.started`, `doorstep_deployments_started_total`) | gates `pending`: release, claim again in 30 s | the app is no longer configured; a gate failed |
| `resolving` | rollbacks: use the preset digest. Deploys: resolve `image:commit-sha` to a digest, then refuse a blocked digest | `resolving → pinning` with `digest` | image not pushed (`404`) or denied (`401`/`403`, logged at `warn`): release, retry in 15 s | invalid reference; digest blocked (reason names the block); image not available within 15 minutes; other errors past 15 minutes |
| `pinning` | read pending block trailers, `Pin` (bounded by the time left before the pin deadline), mark trailers mirrored when the pin wrote a commit | `pinning → syncing` with `pin_commit_sha`, `previous_digest`, `gitops_repository` | | refused by the GitOps repository (see below); other errors, including a pull request that is not merged yet, past 45 minutes |
| `syncing` | wait until Argo CD synced a revision containing the pin commit and the Application is healthy | `syncing → verifying` with `argocd_revision` | the failure policy retries a transient or unknown failure: `syncing → syncing`, release for the backoff, then a fresh window on the same pin ([Retry before rollback](#retry-before-rollback)) | sync failed, timed out, Application not found or forbidden, and the failure policy does not retry |
| `verifying` | evaluate the `after` gates; for rollbacks confirm the pin, re-pinning if it was replaced | `verifying → succeeded` | gates `pending`: release, claim again in 10 s | a gate failed; a rollback's re-pin failed or was not confirmed within 1 hour |

### Before gates hold the deployment in `queued`

A `schedule` or `lock` gate can keep a deploy waiting for hours or days. While its before phase is pending the deployment stays `queued` and the runner releases its entry with a 30 second delay instead of holding the lease. Claims order in-flight deployments first, so a deployment held in `resolving` would block rollbacks and urgent deploys of its target; held as `queued` they still outrank it, and each re-claim batches again, so the newest commit wins when the window opens. A failed before gate moves the deployment straight from `queued` to `failed` (the one transition from `queued` other than `resolving`, `cancelled` and `superseded`), without a spurious start.

### Error classification and deadlines

Adapters in `internal/cli` wrap their errors in one of two engine sentinels; anything else is transient.

| Error | Source | What the engine does |
| --- | --- | --- |
| `engine.ErrPermanent` | `registry.ErrInvalidReference`; `gitops.ErrInvalidRequest`, `ErrPinNotFound`, `ErrAmbiguousPin`, `ErrUnsupportedYAML`, `ErrBranchProtected`, `ErrPinOverwritten`, `ErrPullRequestClosed`; `github.ErrFileTooLarge`, `ErrNotAFile`; an app with no GitOps target | fails the deployment at once |
| `engine.ErrNeedsAttention` | `gitops.ErrPullRequestTampered` | fails the deployment at once and leaves its pull request open for a person to inspect (it is not abandoned) |
| `engine.ErrImageNotReady` | `registry.ErrNotFound`, `registry.ErrDenied` | releases and retries every 15 s until the resolve deadline |
| `core.ErrSyncFailed`, `ErrSyncTimeout`, `ErrSyncTargetNotFound`, `ErrSyncForbidden` | the Argo CD watcher | asks the failure policy, which may [retry](#retry-before-rollback) the same pin; otherwise fails the deployment with the sync failure's reason and the policy's class |
| `core.ErrStateConflict` | a transition lost its compare-and-set | stops quietly: someone else (a cancel, a rollback request, another worker) changed the deployment |
| context cancelled | shutdown or a lost lease | stops without failing anything; the runner releases the entry |
| anything else | network errors, `5xx`, rate limits, `gitops.ErrMergeTimeout` | returned to the runner, which releases the entry with backoff (5 s doubling to 5 minutes) |

Retries of these errors (`retryUntilDeadline`, not to be confused with [retry attempts](#retry-before-rollback)) are bounded by how long the deployment has been in its state, measured from the transition event that entered it, so a flaky dependency never wedges a target. `store.TimeInState` computes `now() - occurred_at` in Postgres, so every deadline runs on the database clock and a replica whose clock is off does not shift it:

| State | Deadline |
| --- | --- |
| `resolving` | 15 minutes |
| `pinning` | 45 minutes, longer than the GitOps pull request merge timeout (30 minutes). `Pin` is called with a context that ends at the deadline (at least 10 seconds), so a pull request still waiting when the deadline passes fails the deployment and is closed |
| `syncing` | the app's `argocd.sync_timeout` (default 10 minutes) per attempt, measured from the end of the retry pause |
| `queued` | the gates' own timeouts ([gates.md](gates.md)) |
| `verifying` | pending gates: their own timeouts. A rollback's pin confirmation (errors from `Pin`, a re-pin pull request waiting to merge, waits for a re-pin to sync, and further re-pins): 1 hour, with `Pin` and the sync wait bounded by the time left. Errors from the gate evaluator itself are retried with backoff and count against no deadline, because the evaluator's results and gate timeouts live in the same database |

Past its deadline the deployment fails with the last error as its reason and class `transient`.

### Resuming a sync wait

`WaitSynced` applies the full per-app timeout on every call, so the engine passes the remaining time as a context deadline: `max(entered syncing + retry pause + sync_timeout, now + 10 s)`, where "entered syncing" is the newest transition into `syncing` (the pin, or the latest retry) and the pause is that retry's `delay_ms`. The 10 second floor guarantees one final assessment after a long outage. When that deadline fires the engine reads the Application once more with `Check`: if it is synced and healthy the deployment moves on, otherwise it fails as a sync timeout with the reason from that read (out of sync, degraded, progressing, ...). If the read itself fails, the last status the wait observed is used, or `unavailable` with the read error when the wait never got an answer. When the parent context ends (shutdown, lost lease) the wait just stops and the next claim resumes it.

If `Pin` reports no attributable commit (the value was already in git, set by hand or older than the last 20 commits), there is no revision to wait for. The deployment moves to `syncing` without a pin commit and the sync step polls the Application's health every 10 seconds until it is `Healthy`, within the sync timeout, then moves to `verifying` with the note `pin already current; no pin commit to wait for`.

### Rollbacks

A rollback ([rollback.md](rollback.md)) goes through the same states. It skips resolving and the block check and pins its preset digest; a rollback without a digest fails at once. The `checks`, `lock` and `soak` gates, and `schedule` without `apply_to_rollbacks`, are recorded as `skipped` for rollbacks ([gates.md](gates.md#rollbacks)); `health` still runs.

Before `verifying → succeeded` the engine calls `Pin` again with `Repin` set, which skips the overwrite check that refuses a replayed pin ([gitops.md](gitops.md#restarts)). If the digest is still pinned, nothing is written, and the rollback succeeds when the returned commit is the recorded pin commit. Otherwise (a cancelled deploy's worker landed its pin late, see [the late-pin race](rollback.md#the-late-pin-race), and `Pin` wrote the digest again, or an earlier claim did and was interrupted) the engine waits for Argo CD to sync the new commit (the sync timeout, cut to what is left of the 1 hour verify deadline) and runs the `after` gates again with `Reevaluate`: their verdicts are sticky, so `Evaluate` would return the passes recorded for the replaced pin. Only then does it record the new pin commit, and it evaluates the gates again in the same claim, all while staying in `verifying`. Recording the commit last means an interrupted claim redoes the wait and the re-evaluation. A replaced pin found after the verify deadline fails the rollback with class `transient`.

### Failure policy

Every failure, from any step, goes through one method that builds an `engine.Failure` (deployment, stage, class, reason, sync failure, gate outcome, whether the pin is kept for a person, and the 1-based attempt) and asks the `FailurePolicy` for an `engine.Decision`:

| Field | Meaning |
| --- | --- |
| `Action` | `fail` or `retry` |
| `Reason` | a typed `DecisionReason`: `transient`, `version_caused`, `retries_exhausted`, `not_retryable`, `retries_disabled`, `newer_release_queued` |
| `Class` | the policy's verdict (`transient`, `version`, `configuration`, `unknown`); it refines the engine's class, which is only a hint |
| `Summary` | one line for history and notifications, such as `transient: image pull failing on node pi-3` |
| `Delay`, `MaxAttempts` | the backoff before the next attempt and the attempt budget |
| `Diagnosis` | an opaque JSON object the engine stores under `diagnosis` |

`serve` uses `retry.Policy` ([below](#retry-before-rollback)); `engine.FailPolicy`, the default when no policy is wired, always fails with `not_retryable`. A policy error, an unknown action, or `retry` for anything but an Argo CD wait failure in `syncing` is logged and treated as `fail`. The failure policy only judges; it never rolls back. The engine acts on its own verdict: for a failure marked `rollback_eligible` of an app with `auto_rollback: true` it fails the deploy and queues the rollback in one transaction ([Automatic rollback](#automatic-rollback)). Otherwise the pin of a deploy that failed after pinning stays in git until someone requests a rollback.

The failure reason is cleaned to one line of at most 500 characters. A deploy that failed in `pinning` abandons its pull request, if any, except after `engine.ErrNeedsAttention`, which leaves it open for inspection. The `failed` transition event carries:

```json
{
  "stage": "syncing",
  "failure_class": "transient",
  "sync_reason": "degraded",
  "attempts": 3,
  "decision": "retries_exhausted",
  "rollback_eligible": true,
  "diagnosis": {"verdict": "transient", "summary": "transient: image pull failing on node pi-3", "...": "..."},
  "auto_rollback": {"outcome": "queued", "rollback_id": 43, "to_deployment_id": 41, "digest": "sha256:1111aaaa2222..."}
}
```

Its message is `failed while <stage>`, with `after N attempts` appended when there was more than one. `rollback_eligible` is true for a deploy (not a rollback) that failed in `syncing` or `verifying` with a pin recorded, when the decision is `version_caused` or `retries_exhausted`, or the failure is an after-gate failure of class `version`. Configuration failures and `newer_release_queued` are never eligible. Manual rollback is available regardless. `auto_rollback` is present only when the engine attempted an automatic rollback (see below).

### Automatic rollback

When a failure is `rollback_eligible` and the optional `AutoRollbacker` port reports that the app opted in (`auto_rollback: true`, [configuration.md](configuration.md#apps)), the engine does not transition the deploy itself. It calls `FailAndRollBack` with the deployment, a trigger (`gate_failed` for a failure in `verifying`, `sync_failed` for one in `syncing`), the block reason `automatic rollback of deployment N: <failure reason>` and a callback that builds the failed transition from the plan. `(*rollback.Service).Automatic` locks the app, decides the plan, queues the rollback, calls the callback, applies the transition with a compare-and-set, cancels queued deploys of the blocked commit, creates the block and audits, all in one transaction ([rollback.md](rollback.md#automatic-rollbacks)). So there is never a rollback without a failed deploy or the other way round. The callback keeps the event schema and the reason text in the engine, and the decision that needs the row lock in `rollback`.

| Outcome | What happened | `failure_reason` | Appended to the event message and `deploy.failed` |
| --- | --- | --- | --- |
| `queued` | rollback queued, version blocked | unchanged | `rolling back automatically to sha256:1111aaaa2222 (deployment 41) as deployment 43` |
| `withheld_migrations` | nothing queued or blocked | original (shortened to fit) + `; automatic rollback withheld: release contains database migrations; roll back with --confirm-migration-rollback` | `automatic rollback withheld: release contains database migrations (deployment 44); roll back with --confirm-migration-rollback` |
| `no_target` | no earlier successful, unblocked deployment | unchanged | `automatic rollback not possible: no earlier successful deployment` |
| `same_version` | the failing digest is the last one that succeeded | unchanged | `automatic rollback skipped: this digest is the last one that succeeded` |
| `rollback_in_progress` | a rollback is already queued or running | unchanged | `automatic rollback skipped: a rollback is already in progress` |

The `failed` event's `auto_rollback` object records `outcome` and, when known, `rollback_id`, `to_deployment_id`, `digest` and `migration_deployment_ids`; the event message is `failed while <stage>; <sentence above>`. After the transaction commits the engine announces `deploy.failed`, then, for `queued`, `deploy.queued` for the rollback (through `Engine.Queued`) and `deploy.cancelled` for the cancelled duplicates. A `core.ErrStateConflict` from the port (someone cancelled or rolled back the deploy by hand) stops quietly like any lost transition; any other error rolls the whole transaction back and is returned to the runner, and the next claim re-runs the step, whose sticky gate verdicts or sync check lead to the same decision.

## Retry before rollback

Self-hosted clusters on small or old hardware fail deploys for reasons that have nothing to do with the new version: a slow or flaky node, an image pull that times out, an OOM kill on a small machine, a pod scheduled onto a node that then drops. Kubernetes keeps retrying pulls, scheduling and restarts by itself, so doorstep's job is to wait long enough and to tell such failures from a bad version. The decision record is [ADR 0017](decisions/0017-retry-before-rollback.md).

### An attempt

A retry is a fresh observation window on the same pin: the same deployment, digest and pin commit, and no write to git or the cluster.

1. The sync step fails (timeout, degraded, ...). The engine reads the retry state (`store.RetryState`: the number of retry events and the newest retry's `delay_ms` and `max_attempts`) and asks the policy with `Attempt = retries + 1`.
2. On `retry`, the engine records a `syncing → syncing` transition with the message `attempt N failed; retrying (attempt N+1 of M) in <delay>`, logs it at `warn`, counts `doorstep_deployment_retries_total`, sends `deploy.retrying` on the first failed attempt only, and releases the claim for the delay (or carries on in the same claim when the delay is zero).
3. The next claim measures the attempt from the end of the pause: `elapsed = TimeInState(syncing) − delay`. A negative value (an early claim, say after a restart) releases again for the remainder. The new attempt then waits the full `argocd.sync_timeout` again, with the same final `Check`.
4. On `fail`, the deployment fails as described above, and the `deploy.failed` notification carries the final decision as its message: `<summary>; not retried` for a version-caused failure, `still failing after N attempts: <summary>` once the budget is spent, `not retried because a newer deployment is waiting: ...` or `not retried because retries are disabled: ...`.
5. A deployment that succeeds after a retry says `succeeded on attempt N of M` in its `deploy.succeeded` or `deploy.rolled_back` message.

The retry transition carries the retry count the engine read in step 1, and the store locks the deployment row and rejects the transition with a state conflict when the count has moved since. When a lease expires during a long wait and a second worker claims the deployment, only one of them records the retry, so attempts are not double-counted and `deploy.retrying` is not sent twice. Every failure, in any stage, reads the retry state, so an after-gate failure in `verifying` that followed retried sync attempts records `attempts` and says `failed while verifying after N attempts`.

### What a retry cannot fix

A retry is passive: doorstep waits again, but neither Kubernetes nor Argo CD does anything new because of it. It helps when the cluster is converging on its own: an image pull that Kubernetes retries, a pod that waits for capacity and is scheduled once a node frees up, a node that comes back, a slow start that finishes after the first window. It does not help when nothing on the cluster side will change:

- **A crash-looping or not-ready pod stays on its node.** Kubernetes restarts the container in place; it does not move the pod to another node. When the node is the problem, a retry only waits longer for the same pod, unless something else (an eviction, a drained node, a person deleting the pod) gets it rescheduled.
- **A failed sync operation is not re-run.** Argo CD does not sync a revision again after its operation failed, unless the Application sets `syncPolicy.retry` or someone syncs by hand, so a retried `sync_failed` waits for a sync that never comes.

For those failures a retry mostly lengthens the time before the deploy fails, bounded by the attempt budget. doorstep holds no Argo CD token and never writes to the cluster ([ADR 0001](decisions/0001-go-and-core-design.md)), so it cannot trigger a sync or delete a pod itself; an opt-in active retry would need that grant. Set `syncPolicy.retry` on the Application if failed syncs should be retried, and lower `retry.max_attempts` for apps where waiting cannot help.

The retry event's data is:

```json
{
  "stage": "syncing",
  "failure_class": "transient",
  "sync_reason": "degraded",
  "retry": {"attempt": 1, "next_attempt": 2, "max_attempts": 3, "delay_ms": 60000, "reason": "transient", "summary": "transient: image pull failing on node pi-3"},
  "diagnosis": {"verdict": "transient", "summary": "...", "attempt": 1, "digest": "sha256:...", "findings": [{"signal": "image_pull", "node": "pi-3", "pods": 2, "reason": "ImagePullBackOff", "detail": "..."}], "nodes": ["pi-3"], "pods_inspected": true}
}
```

Only failures of the Argo CD wait in `syncing` are retried, for deploys and rollbacks alike (a rollback has nothing behind it, so patience matters most there). Failures in `queued`, `resolving` and `pinning` keep their time-bounded handling above, and after-gate failures in `verifying` are final: gates carry their own patience, and metric, external and Job gate failures are version-caused by definition.

### Backoff and the worst case

Each app has `retry: {enabled, max_attempts, backoff, max_backoff}` ([configuration.md](configuration.md#apps)), defaulting to on, 3 attempts in total, 1 minute doubling per attempt, capped at 10 minutes, with no jitter. The pause before attempt N+1 is `min(backoff × 2^(N−1), max_backoff)`. With the defaults and a 10 minute sync timeout, a deploy that never becomes healthy fails after 3 × 10m + 1m + 2m = 33 minutes instead of 10. Pauses release the claim, but each window holds one of the replica's `engine.concurrency` slots, so several apps retrying on slow hardware at once can delay other deploys; raise `engine.concurrency` or lower `max_attempts` where that matters.

Before retrying, the policy checks the target's queue. When another deployment of the target is waiting (a fix-forward push, an urgent deploy), it fails with `newer_release_queued` instead of holding it back for the rest of the budget. Rollback requests cancel the active deployment anyway.

### Classification

`retry.Classify` is pure and always runs, even with retries disabled, so every failure records a diagnosis. In order:

| Rule | Verdict |
| --- | --- |
| not an Argo CD wait failure in `syncing` | the engine's class, `not_retryable` |
| `not_found`, `forbidden`, `untracked_revision`, or `out_of_sync` with manual sync | `configuration`, not retried |
| `sync_failed` with a sync hook in phase `Failed` or `Error` (a failing migration or other hook Job) | `version`, not retried |
| `sync_failed` where an object was rejected as invalid (`... is invalid: ...`) | `configuration`, not retried |
| `sync_failed` otherwise (an apply error such as an API server timeout or an unavailable admission webhook) | like any other reason: pod signals below, else `unknown` |
| any new-version pod waiting with `InvalidImageName` or `RunContainerError` | `version` |
| pods that cannot be scheduled, `ErrImagePull`/`ImagePullBackOff` (the digest is already known to exist in the registry), a lost node (`NodeLost`, or terminating in phase `Unknown`), `Evicted`, containers stuck in `ContainerCreating`/`PodInitializing`, or running without restarts but not ready (a probe timeout) | `transient` |
| `CrashLoopBackOff` or `OOMKilled` | decided by evidence, below |
| no signal at all: `degraded` | `unknown` (application health alone cannot tell a slow node from a bad build) |
| no signal at all: any other reason | the reason's class |

Signals come from pod statuses when [pod diagnostics](#pod-diagnostics) are on, and otherwise, or when no pod shows one, from the same keywords in the Argo CD failure message and the messages of unhealthy Pods, Deployments, StatefulSets, DaemonSets, ReplicaSets and Argo Rollouts (other resources' messages are ignored, so a Service or Ingress message cannot fake a signal). `transient` and `unknown` are retried while attempts remain; then the decision is `retries_exhausted`. `version` fails at once with `version_caused`.

### Evidence: the hardware or the release

For a crash loop or an OOM kill, in order:

1. **Hardware.** A pod of the previous digest is also not ready on the same node. Pods of the previous digest that are terminating (shut down by the rolling update) do not count. Verdict `transient`: "hardware, not the release".
2. **The version.** The same termination reason and exit code on two or more distinct nodes, in this attempt or across this deployment's earlier attempts. Verdict `version`.
3. **Hardware.** Another digest of this target crashed (`crash_loop` or `oom_killed`) on the same node in the last 7 days (`store.Diagnoses`), in a diagnosis whose verdict was `transient` or `unknown`. Diagnoses of a digest that any diagnosis found version-caused are ignored, as are other signals such as image pulls, so a bug that ships again in the next version is not blamed on the nodes it crashed on before. Verdict `transient`.
4. **The version.** A pod of the previous digest is ready on the same node. Verdict `version`.
5. Otherwise `unknown`, which is retried.

New and previous pods are told apart by the `@<digest>` suffix of their images against the deployment's `digest` and `previous_digest`; when no pod matches either, all pods count as new. Live pods give the evidence while a rolling update leaves old pods running; stored diagnoses give it when they do not. On a single-node homelab, rule 3 can blame the hardware for a version that broke twice in a week; the attempt budget bounds the cost.

Findings are capped at 10, nodes at 10, messages at 200 bytes and the summary at 300 characters.

### Pod diagnostics

Argo CD's Application does not carry pods, and Argo CD 3 no longer persists resource health in it, so node-level evidence needs pod reads. They are opt-in: `kubernetes.pod_diagnostics: true` (`DOORSTEP_KUBE_POD_DIAGNOSTICS`), granted per namespace with a RoleBinding to the ClusterRole of the `components/pod-diagnostics` kustomize component, or cluster-wide with `components/pod-diagnostics-cluster-wide` ([install.md](install.md#pod-diagnostics)). On a failure, `argocd.Watcher.Workloads` reads the Deployments, StatefulSets, DaemonSets and Argo Rollouts from the Application's `status.resources`, and `internal/pods` lists pods with `GET /api/v1/namespaces/{ns}/pods?limit=500` per namespace (at most five), following `continue` tokens for at most 10 pages and logging a `warn` when a namespace has more, and keeps at most 200 pods, matching them by controller owner reference (a ReplicaSet named `<workload>-<pod-template-hash>` or `<rollout>-<rollouts-pod-template-hash>`, or the StatefulSet or DaemonSet itself). Only node, images, phase, reason, conditions and container states are decoded, so environment values are never held or logged. A `403` or any other read error is logged at `warn` and the classification falls back to the Application; each lookup is bounded by 15 seconds.

## Pins of stopped deployments

`Abandon` closes a deployment's pull request and deletes its branch; it is idempotent and a no-op in commit mode. It is called when:

- a cancelled deployment had already started (from the post-commit announcement below), whether a rollback request or a manual cancel cancelled it;
- a worker's pin step is interrupted by a state conflict or a lost lease and the reloaded deployment is `cancelled` or `superseded` (the worker may open its pull request after the canceller's `Abandon` ran);
- a deployment fails while `pinning`, unless the failure needs a person (`ErrPullRequestTampered`).

Deployments superseded straight from `queued` never had a branch, so they are not abandoned.

## Notifications and metrics

The worker emits each `deploy.*` event right after its own successful transition, so at most one replica announces it. Delivery is at most once: a crash between the transition commit and the announcement loses both the notification and the metric, which [ADR 0006](decisions/0006-notifications-are-best-effort.md) accepts. `deploy.superseded` is announced only by the claim that superseded the deployment, so it is lost when that claim crashes first. See [notifications.md](notifications.md#delivery).

| Event | Emitted by | Metric |
| --- | --- | --- |
| `deploy.queued` | `Engine.Queued`, called after the enqueue committed: by a decorator around intake's settlement, by a decorator around rollback requests, by the worker right after an automatic rollback committed, and by `announcingDeploys` around manual deploys (API, web UI, CLI) in `cli`; a manual request that returned an existing deployment is not announced again | |
| `deploy.started` | the worker, on `queued → resolving` | `doorstep_deployments_started_total` |
| `deploy.retrying` | the worker, on the first `syncing → syncing` retry only | `doorstep_deployment_retries_total` on every retry |
| `deploy.succeeded`, `deploy.rolled_back` | the worker, on `verifying → succeeded` (a rollback carries the deployment it reverts; after a retry the message is `succeeded on attempt N of M`) | duration with outcome `succeeded` or `rolled_back` |
| `deploy.failed` | the worker, on `→ failed`, with the final decision as its message, followed by the [automatic rollback](#automatic-rollback) outcome when there was one | duration with outcome `failed` or `rollback_failed` |
| `deploy.superseded` | the worker that claimed the newer deployment, for each deployment the claim superseded | duration with outcome `superseded` |
| `deploy.cancelled` | `Engine.Cancelled`, after a rollback request (manual or automatic) cancelled deployments and after a manual cancel (`announcingDeploys`) | duration with outcome `cancelled` |

`deploy.queued` is announced after the enqueue transaction commits, so a worker that claims the deployment at once can announce `deploy.started` first. The duration is from `started_at` to `finished_at`, zero when the deployment never started. Gate durations come from the gate evaluator.

## Ports

The engine imports only `core` and the standard library and declares the ports it uses, with its own small types (`Claim`, `Result`, `Transition`, `PinRequest`, `PinResult`, `Outcome`, `RetryState`, `Failure`, `Decision`, `RollbackPlan`, `AutoRollbackRequest`, `AutoRollbackResult`). The adapters live in `internal/cli`:

| Port | Adapter |
| --- | --- |
| `DeploymentStore` (`Get`, `Transition`, `SetLineage`, `TimeInState`, `RetryState`) | `engineStore` over `*store.Store` |
| `DigestResolver` | `digestResolver` over `registry.Resolver.ResolveCommit`, with credentials re-read on every call |
| `PinWriter` (`Pin`, `Abandon`) | `pinWriter` over one `*gitops.Writer`, mapping each app to its `gitops.Target` and `PinRequest.Repin` to `gitops.Request.Repin` |
| `SyncWatcher` (`WaitSynced`, `Check`, `Health`) | `*argocd.Watcher` |
| `GateEvaluator` (`Evaluate`, `Reevaluate`) | `*gates.Evaluator` built by `newGateEvaluator` |
| `BlockList` | `*rollback.Service` |
| `AutoRollbacker` (`Enabled`, `FailAndRollBack`), optional | `autoRollbacks` over `catalogApps.AutoRollback` and `(*rollback.Service).Automatic`, mapping the engine's `RollbackPlan`, `RollbackTrigger` and `Transition` to the `rollback` and `store` types; the actor is `system:doorstep`. Without it the engine never rolls back on its own |
| `Notifier` | `*notify.Dispatcher` |
| `Metrics` (`DeploymentStarted`, `DeploymentFinished`, `DeploymentRetried`) | `engineMetrics` over `metrics.Recorder` |
| `FailurePolicy` | `*retry.Policy` built by `newRetryPolicy` in `internal/cli/retry.go`: app settings from the catalog, `retryHistory` and `retryQueue` over `*store.Store`, and `podDiagnostics` (`*argocd.Watcher` plus `*pods.Reader`) when pod diagnostics are on |

`queueHandler` turns `queue.Claimed` into `engine.Claim` and `engine.Result` into `queue.Result`. There is no queue port: the runner claims, heartbeats and releases, and a terminal transition deletes the queue entry in the same transaction.

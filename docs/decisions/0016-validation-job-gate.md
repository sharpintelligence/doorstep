# 0016: The validation Job gate reads a PostSync hook

- Status: Accepted
- Date: 2026-11-24

## Context

Teams want doorstep to run their own validation after a deploy (smoke tests, API checks, browser tests) as a per-app Kubernetes Job, use its exit status as an after gate and link its logs from the deploy. A failed validation should roll the deploy back like any other failed gate.

[ADR 0001](0001-go-and-core-design.md), decision 2, makes git the only thing doorstep writes to and Argo CD the only thing that applies manifests to the cluster; doorstep only reads Argo CD `Application` resources, and that property is meant to be checkable in the RBAC manifests. Running a Job seems to need a write. [ADR 0008](0008-kubernetes-rest-client.md) keeps `internal/kube` a small GET-only client and asks that any new Kubernetes resource doorstep reads be recorded in an ADR.

Other constraints: the engine's syncing step finishes only once the Application is synced to a revision containing the pin, with no operation `Running`; the health gate fails an Application that stays `Degraded` for a minute; gates keep their state in evidence; gate evidence is shown in the UI and mirrored to GitHub; a rollback must not be held back by anything but Argo CD ([ADR 0001](0001-go-and-core-design.md), decision 5).

## Decision

- **The validation Job is a user-declared Argo CD PostSync hook.** The app's GitOps manifests contain a Job with a fixed `metadata.name` and `argocd.argoproj.io/hook: PostSync` (or Helm's `post-install`/`post-upgrade`, which Argo CD maps to PostSync). Argo CD creates it after every sync once the synced resources are healthy, and with the default `BeforeHookCreation` delete policy replaces the previous run. doorstep writes no manifests and creates nothing.
- **doorstep reads the `batch/v1` Job with one `GET` per evaluation** (`/apis/batch/v1/namespaces/{ns}/jobs/{name}`) through a new adapter, `internal/jobs`, which imports only `internal/kube`. `internal/kube` is unchanged and still has no write methods. `gates` owns the `JobReader` port and the `gates.Job` type; `cli` adapts one to the other, and `gates.RegisterJobGate` registers the type separately from `RegisterBuiltins`.
- **A new gate type `job`**, after phase only, metrics kind `job`, default timeout `10m`, configured with `job: {namespace, name, logs_url}`.
- **Verdict rules.** A Job that is not a PostSync hook fails at once. A Job that is not fresh (below) never passes. `Complete=True` on a fresh Job passes; `Failed=True` fails with the condition's reason and message. Not found, being deleted, stale, suspended or still active is `pending` with a message, until the evaluator's timeout fails it. Other read errors, including `401` and `403`, are evaluation errors.
- **Staleness is checked against the Application's latest operation.** The hook annotation alone is not enough: the syncing step treats a `Failed` operation as done once the Application is synced and healthy, so a sync that fails in its Sync phase after applying the changes, or a selective sync (which runs no hooks), leaves the previous deploy's successful Job in place. The gate therefore also reads the Application through the existing `ApplicationReader` port and judges the Job only when its `creationTimestamp` is at or after `status.operationState.startedAt`, whichever revision that operation synced. A full sync always recreates a `BeforeHookCreation` hook after its operation starts, so a fresh Job is the latest sync's. An older Job is `pending` while the operation can still rerun it and `failed` once the operation ended `Failed` or `Error`; no operation at all is `pending`. Both timestamps come from the cluster (API server and Argo CD controller), and an unchanged pin keeps working because Argo CD starts no new operation for it. A plain Job is still rejected outright, since it never reruns.
- **Logs are linked by a URL template** (`job.logs_url`, with `{app}`, `{namespace}`, `{job}`, `{uid}`, `{deployment}`, `{argocd_namespace}` and `{argocd_application}` replaced by path-escaped values, query-escaped after the first `?` or `#`, and no link while a placeholder has no value), stored as evidence `logs_url`. doorstep reads no Pods and no pod logs.
- **Skipped on rollbacks** (`SkipOnRollback`), with no per-gate opt-in.
- **RBAC is opt-in.** A kustomize component, `components/job-gate`, adds the ClusterRole `doorstep-jobs-reader` (`get` on `jobs` in `batch`) and its ClusterRoleBinding; the base is unchanged. A per-namespace RoleBinding is documented as the narrower alternative. ADR 0001's list of what doorstep reads is extended by "`get` on `batch/v1` Jobs, opt-in".

## Options considered

### A PostSync hook declared in git, read from the Job (chosen)

The Job spec is reviewed in git, Argo CD is the only applier and doorstep stays read-only, so the trust boundary of ADR 0001 holds. PostSync already means "after the resources are synced and healthy". Hook Jobs do not count towards Application health, so a failed validation cannot leave the Application `Degraded` and fail the following rollback's health gate. It needs no change to the pin writer or the engine. The costs are that `argocd.sync_timeout` must cover the Job's runtime, and that users must keep a fixed name and the default delete policy.

### doorstep commits a uniquely named Job manifest per deployment

Keeps doorstep writing only to git, but needs multi-file atomic commits (the Git data API) in both commit and pull request modes, template rendering, edits to kustomization resources and pruning of old Jobs, and it is a second commit per deploy because the gate only runs after the pin is synced. A plain Job is immutable, shows the Application `Progressing` or `Degraded`, and a failed one stays in git and fails the rollback's health gate after the one minute grace. Far more surface for no gain over a hook.

### An RBAC exception letting doorstep create Jobs

A narrow `create` on Jobs in one namespace would let doorstep launch the Job itself. It breaks the "checkable in RBAC manifests" property, gives doorstep cluster write credentials (creating a Job in a namespace is effectively running any pod with that namespace's ServiceAccounts and Secrets), would make `internal/kube` grow write methods, and Argo CD would see, or prune, an unmanaged Job.

### The verdict from the Application's `operationState` hook results

Needs no new RBAC, but yields only a hook phase and message, is overwritten by the next operation, needs revision and ancestry matching in GitOps repositories shared by several apps, and would change `internal/argocd` and its gates adapter. The Job object gives the counts, backoff limit and terminal condition with its reason, self-contained in one small reader.

### A `creationTimestamp >= deployment.StartedAt` freshness guard

Looks like a direct staleness check, but falsely fails, and so rolls back, a deploy whose pin is unchanged, where Argo CD rightly does not resync, and it compares the Postgres clock with the Kubernetes clock. Comparing with the Application's latest operation start (chosen) has neither problem.

### The hook annotation as the only staleness defence

The first version of this decision relied on Argo CD recreating the hook on every sync and on the syncing step waiting for the operation. Review showed it can pass on old results: a `Failed` operation counts as done once the Application is synced and healthy, and a failure in the Sync phase or a selective sync runs no PostSync hook, so the previous deploy's `Complete` Job is still there.

### Matching the Job against `operationState.syncResult.resources`

Requiring a hook entry for the Job in the latest operation's sync result would also catch the bypass, but the result is replaced by the next operation, needs the adapter to expose a resource list rather than one timestamp, and adds nothing the timestamp comparison misses.

### Reading Pods or pod logs for the link

A Kubernetes API pod log URL is not usable in a browser, reading `pods` or `pods/log` widens RBAC, and copying logs into evidence risks mirroring secrets to GitHub. A template covers the Argo CD UI, Grafana with Loki and Kubernetes dashboards with no extra permissions. Braces rather than `text/template` keep user input from executing template logic.

## Consequences

- `argocd.sync_timeout` must cover the rollout plus the Job's runtime, because the syncing step waits for the running hook. The gate's own timeout covers only a missing, replaced or still running Job after the sync. doorstep cannot know a Job's runtime, so this is documented rather than validated.
- Users must give the Job a fixed name, keep `BeforeHookCreation`, and avoid `HookSucceeded` and short `ttlSecondsAfterFinished`; violations show as a pending gate with an explanatory message and then a timeout, never a silent pass. Retries belong to `backoffLimit`, since the first terminal failure is sticky.
- A failed PostSync hook now fails the deploy when the app has a `job` gate; without one it still does not, as before.
- There is no logs link without `logs_url`, so the docs recommend setting it. The link is evidence returned by the API; rendering it in the UI, notifications and the GitHub mirror belongs to those surfaces, none of which renders gate evidence yet.
- Installs that do not use the gate grant nothing new. A cluster-wide `get` on Jobs can expose inline environment values in Job specs, so the namespaced RoleBinding is documented.
- No new Go modules, no migration, no store change; `internal/kube` is unchanged.
- The `job` gate reads the Argo CD Application once per evaluation in addition to the Job, through the port the `health` and `soak` gates already use, so it needs no new RBAC for it. The `gates.Application` port type gains the latest operation's start time.
- If `serve` does not register the type, a configuration using `type: job` fails at startup with `unknown gate type`, never a silently passing gate.

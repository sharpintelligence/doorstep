# Argo CD

After doorstep commits a pin to the GitOps repository, it waits for Argo CD to roll it out. It never asks Argo CD to do anything: it does not sync, refresh, patch or annotate Applications, and it holds no Argo CD API token. It only reads the Argo CD `Application` resource through the Kubernetes API, with `get` and `watch` on `applications.argoproj.io`. The code lives in `internal/argocd`, on top of a small Kubernetes client in `internal/kube` ([ADR 0008](decisions/0008-kubernetes-rest-client.md)).

## What is read

Each app names one Application (`apps[].argocd.application`, default the app name) in one namespace (`apps[].argocd.namespace`, default `argocd`). doorstep reads these fields and ignores the rest:

| Field | Used for |
| --- | --- |
| `spec.source.repoURL`, `spec.sources[].repoURL` | finding the GitOps source of a multi-source Application |
| `spec.syncPolicy.automated` | whether Argo CD syncs by itself (a failure hint, see below) |
| `status.sync.status`, `status.sync.revision`, `status.sync.revisions` | sync state and the revision Argo CD last compared against |
| `status.health.status`, `status.health.message` | Application health |
| `status.operationState` | the phase, message, requested revision, finish time and per-resource results of the last sync operation |
| `status.conditions` | `ComparisonError`, `SyncError` and `InvalidSpecError` conditions |
| `status.resources` | resources that are not healthy or out of sync, to explain a failure; and, after a failed attempt, the Deployments, StatefulSets, DaemonSets and Argo Rollouts the Application manages (`Watcher.Workloads`), whose pods [pod diagnostics](engine.md#pod-diagnostics) list |
| `status.reconciledAt` | when Argo CD last looked at the Application |

## When a deploy counts as synced

`WaitSynced` returns success only when all of these hold at once:

1. the Application's synced revision contains the pin commit (see below);
2. `status.sync.status` is `Synced`;
3. no sync operation is `Running` or `Terminating`;
4. `status.health.status` is `Healthy`;
5. when the current sync operation has a `finishedAt` and the Application a `reconciledAt`, the health was assessed no earlier than the operation finished (otherwise the reason is `progressing` until Argo CD reconciles again).

Apart from the last check, this is the same condition as `argocd app wait --sync --health --operation`. It means the deployment's `syncing` state covers the whole rollout, and `verifying` starts from a healthy Application. The `health` after-gate then checks the same condition again through `Check`, which normally passes at once.

A terminal operation whose phase is `Failed` does not stop the Application from counting as ready when its resources nevertheless ended `Synced` and `Healthy`; doorstep reports the operation message but does not fail the deploy for it.

### Contains the pin

Argo CD tracks the branch head, and several apps often share one GitOps repository. By the time Argo CD refreshes, the head may be a later commit than this app's pin, made by another app's deploy. doorstep therefore accepts any synced revision that contains the pin:

- a revision equal to the pin is accepted at once;
- otherwise, for a revision of 40 hex characters, doorstep asks GitHub whether the pin is an ancestor of it (`GET /repos/{owner}/{repo}/compare/{pin}...{revision}`): `ahead` or `identical` means it is contained, `behind`, `diverged` and `404` mean it is not;
- answers are remembered for the rest of the wait, so each new revision costs one GitHub call;
- a GitHub error is logged at `warn` and counts as "not yet"; the next event or resync asks again;
- a `404` from the compare means GitHub does not know one of the commits in that repository, for example because the Application tracks a fork or another repository. It is logged at `warn` and remembered as "not contained", so check the log when a wait times out with reason `not_observed`.

The GitHub App therefore needs read access to the GitOps repository's contents, which it already has to write pins. `doorstep argocd status` works without the GitHub App and then only accepts an equal revision.

### Multi-source Applications

For an Application with `spec.sources`, the candidate revisions are the entries of `status.sync.revisions` whose source `repoURL` is the app's GitOps repository. These forms match `owner/name`, case-insensitively and with or without `.git`:

- `https://github.com/owner/name`
- `git@github.com:owner/name`
- `ssh://git@github.com/owner/name`

The host is not compared, so GitHub Enterprise URLs match too. If no source matches by URL, every revision of 40 hex characters is a candidate. Helm chart versions such as `4.5.6` are never sent to GitHub. When Argo CD reports revisions but none of the candidates is a commit, as for a single-source Application of a Helm chart, the pin can never be reached and the reason is `untracked_revision`. The pin counts as reached if any candidate equals it or contains it. The revision recorded for the deployment is the list joined with `, `, for example `0123…cdef, 4.5.6`.

## Timeouts

`apps[].argocd.sync_timeout` (default `10m`, must be positive) bounds each wait. The adapter applies it itself on every call:

- When the app's own timeout expires, `WaitSynced` returns `core.ErrSyncTimeout` with the last observed status. The message names the reason, for example `argocd: sync timed out after 10m0s: application health is degraded: Deployment web: Deployment exceeded its progress deadline`.
- If the Application could not be read at all during the wait, the error also wraps `core.ErrSyncUnavailable` and the reason is `unavailable`.
- If the caller's context has an earlier deadline, or is cancelled (for example on shutdown), `WaitSynced` returns that context error unchanged, never `ErrSyncTimeout`, so a shutdown is not reported as a failed deploy. The engine resumes a wait after a restart with the remaining time as a context deadline (measured from when the deployment entered `syncing` or its latest [retry](engine.md#retry-before-rollback) ended its pause, at least 10 seconds) and tells its own deadline apart from a shutdown, treating the former as a sync timeout ([engine.md](engine.md#resuming-a-sync-wait)).

The `health` gate has its own `timeout` (default `10m`), measured from its first evaluation. Because `WaitSynced` already waited for health, the gate normally passes on its first evaluation; its timeout only matters when health is lost again after syncing.

Argo CD keeps a sync operation `Running` until its PostSync hooks finish, and the wait treats a running operation as not ready. With a [`job` gate](gates.md#job), whose validation Job is a PostSync hook, `sync_timeout` must therefore cover the rollout plus the Job's runtime. A failed hook ends the operation `Failed`; when the app's resources are nevertheless synced and healthy the wait succeeds, as above, and the `job` gate fails the deploy with the Job's own reason (otherwise the wait can end early with `sync operation failed`). The gate's `timeout` (default `10m`) then only covers a Job that is missing, being replaced or still running after the sync. Without a `job` gate, a failed PostSync hook does not fail the deploy.

## Failing before the timeout

Only deterministic conditions end a wait early:

| Condition | Error |
| --- | --- |
| The Application does not exist (`404`), or is deleted during the wait | `core.ErrSyncTargetNotFound` |
| The app has no Application configured | `core.ErrSyncTargetNotFound` |
| The Kubernetes API answers `401` or `403` to the read | `core.ErrSyncForbidden` (also `kube.ErrForbidden`) |
| The current sync operation's phase is `Failed` and its revision is the current synced revision, which contains the pin | `core.ErrSyncFailed` |

Everything else keeps waiting until the timeout: an operation in phase `Error` (Argo CD retries those), `Degraded` health (it often flaps during a rollout), `Missing`, `Suspended`, `OutOfSync`, `untracked_revision`, and Kubernetes API errors.

An operation's revision is the one in its `syncResult`. A sync that fails before Argo CD writes a `syncResult`, for example on a manifest generation error, is matched by the revision it requested in `operation.sync.revision` (or `revisions`), so it still fails fast. An `operationState` left over from an older revision, or one that names no revision, is ignored.

## Failure reasons

Every non-ready status carries a `core.SyncFailure` with a stable reason, a class and a message of at most 512 bytes. The class is only a hint: the [retry policy](engine.md#classification) refines it with pod signals and history, for example downgrading `degraded` without other evidence to `unknown`, and decides whether to wait again on the same pin or fail.

| Reason | When | Class |
| --- | --- | --- |
| `sync_failed` | the current sync operation failed | `version` when a sync hook is in phase `Failed` or `Error`; `configuration` when the operation or a failed object says `... is invalid: ...`; otherwise `unknown` (an apply error can be a flaky API server or webhook) |
| `untracked_revision` | Argo CD reports revisions, but none of the candidates is a Git commit, so the pin can never be reached | `configuration` |
| `not_observed` | Argo CD has not reached a revision containing the pin | `transient` |
| `sync_error` | the current operation is in phase `Error`, or a `ComparisonError`, `SyncError` or `InvalidSpecError` condition is set | `transient` |
| `sync_running` | a sync operation is still running | `unknown` |
| `out_of_sync` | the Application is `OutOfSync` | `configuration` when automated sync is off, otherwise `unknown` |
| `degraded` | health is `Degraded` | `version` |
| `missing` | health is `Missing` | `unknown` |
| `suspended` | health is `Suspended` | `unknown` |
| `progressing` | health is `Progressing` or unknown, or was read before the sync operation finished | `unknown` |
| `not_found` | the Application does not exist | `configuration` |
| `forbidden` | the API refused the read | `configuration` |
| `unavailable` | the API could not be read until the timeout | `transient` |

The reasons are chosen in the order of the table. `Resources` lists up to 20 resources: those whose sync result failed in the current operation (including failed hooks), then those that are not `Healthy` or are `OutOfSync`, each with a message of at most 512 bytes. Argo CD 3 no longer stores resource health in the Application by default, so resource health may be `unknown` there; the Application's own health is always present.

## How waiting works

There is no informer, cache or background goroutine. Each `WaitSynced` call loops:

1. `GET` the Application and assess it; return on success or on a fail-fast condition.
2. Open a watch on the Application's namespace with `fieldSelector=metadata.name=<name>`, starting at the `resourceVersion` just read, with `timeoutSeconds` equal to the resync interval (30 seconds) and bookmarks allowed. Every `ADDED` or `MODIFIED` event is assessed the same way.
3. When the watch ends, returns an `ERROR` event (such as `410 Gone`), fails to decode or loses its connection, go back to step 1.

A failed `GET` is retried with jittered backoff from 1 second up to 30 seconds. A watch that ran its course, or a first `410 Gone`, is followed by an immediate `GET`; any other watch failure waits with the same backoff, capped at the resync interval, so a watch that keeps failing degrades to a `GET` every 30 seconds. A watch the API refuses with `403` (for example when RBAC grants `get` but not `watch`) waits the full resync interval each time. A silently dead connection is bounded by the server-side `timeoutSeconds` and a client deadline 5 seconds later. One wait holds at most one connection, and at most one wait runs per app at a time.

`Check` and `Health` do one `GET` and never block. `Check(ctx, target, revision)` runs the same assessment as `WaitSynced` once; `Health(ctx, target)` reports the Application's health and unhealthy resources regardless of revision.

## Refresh latency

Argo CD notices a new commit when it polls the repository, about every 3 minutes by default, or at once when its GitHub webhook is configured. doorstep cannot trigger a refresh, because it never writes to the cluster. Configure the [Argo CD webhook](https://argo-cd.readthedocs.io/en/stable/operator-manual/webhook/) on the GitOps repository for fast deploys, and keep `sync_timeout` well above the polling interval.

## Manual sync

An Application without automated sync stays `OutOfSync` after the pin lands until someone syncs it. doorstep waits for that until `sync_timeout`, and the failure class is `configuration`. Raise `sync_timeout` if people sync such apps by hand.

## Connecting to the cluster

The `kubernetes` section of the configuration file (or flags and environment variables) selects the connection. `serve` connects whenever apps are configured and refuses to start when it cannot build the client (for example an unreadable kubeconfig, or no in-cluster ServiceAccount outside a cluster); it reads nothing from the cluster until a deployment syncs or a gate reads an Application:

| Key | Flag | Environment variable | Default |
| --- | --- | --- | --- |
| `kubernetes.kubeconfig` | `--kubeconfig` (`serve`, `argocd status`) | `DOORSTEP_KUBECONFIG` | empty: in-cluster |
| `kubernetes.context` | `--kube-context` (`serve`, `argocd status`) | `DOORSTEP_KUBE_CONTEXT` | the kubeconfig's `current-context` |

### In the cluster

With no kubeconfig, doorstep uses the pod's ServiceAccount: the API server from `KUBERNETES_SERVICE_HOST` and `KUBERNETES_SERVICE_PORT`, the CA bundle `/var/run/secrets/kubernetes.io/serviceaccount/ca.crt` as the only trust root, and the token in `/var/run/secrets/kubernetes.io/serviceaccount/token`. The token is read again for every request, so bound token rotation needs no restart. The [install base](install.md) mounts it and grants the RBAC.

### With a kubeconfig

doorstep never falls back to `$KUBECONFIG` or `~/.kube/config`; a kubeconfig is only used when it is named explicitly, so a development machine's current context is never used by accident. A context needs a kubeconfig. The supported subset covers kind, k3s and k3d, Talos, minikube and MicroK8s:

- `server` (`https`, or plain `http` only to a loopback address), `certificate-authority` or `certificate-authority-data`, `tls-server-name`;
- `token` or `tokenFile` (read again for every request);
- `client-certificate` or `client-certificate-data` with `client-key` or `client-key-data`.

Relative paths are resolved against the kubeconfig's directory. These settings are refused with `kube.ErrUnsupportedAuth`: `exec` credential plugins, `auth-provider`, `username`/`password`, impersonation (`as`, `as-groups`, `as-uid`), `insecure-skip-tls-verify` and `proxy-url`. For clusters that need a credential plugin, such as EKS, GKE or AKS, run `kubectl proxy` and point a kubeconfig at it:

```yaml
apiVersion: v1
kind: Config
current-context: proxy
clusters:
  - name: proxy
    cluster:
      server: http://127.0.0.1:8001
contexts:
  - name: proxy
    context:
      cluster: proxy
users: []
```

### Transport

Credentials are only sent when the scheme and host of a request match the configured API server, and redirects are never followed (`kube.ErrForeignHost`, `kube.ErrRedirect`). TLS 1.2 is the minimum. No HTTP proxy from the environment is used. A `GET` times out after 30 seconds and reads at most 16 MiB; a watch reads at most 64 MiB before it is reopened. API error messages are cut to 512 bytes, and tokens are never logged.

## `doorstep argocd status`

```sh
doorstep argocd status <app> --config config.yaml [--kubeconfig PATH] [--kube-context NAME] [--revision SHA]
```

Reads the app's Application with the same connection and permissions as `serve` and prints it:

```text
application  argocd/blog
sync         synced
revision     0123456789abcdef0123456789abcdef01234567
operation    Succeeded: successfully synced (all tasks run)
health       degraded: one pod failing
resources
  apps/Deployment blog/web: degraded, Synced: Deployment exceeded its progress deadline
```

With `--revision`, it also prints `pin`, `reached`, `ready` and the failure `reason` with its class. When the GitHub App is configured it is used for the ancestry check; otherwise only an equal revision counts. The command exits non-zero when the Application cannot be read (missing, forbidden, unreachable), not when it is unhealthy. Use it to check RBAC and connectivity after installing.

## Troubleshooting

| Symptom | Cause |
| --- | --- |
| `not running in a kubernetes pod ... set DOORSTEP_KUBECONFIG or pass --kubeconfig` | outside a cluster without a kubeconfig |
| `kube api: 403 Forbidden` | the ServiceAccount lacks `get` on `applications.argoproj.io` in the Application's namespace; see [install.md](install.md#rbac) |
| `sync target not found` | `argocd.application` or `argocd.namespace` does not name an existing Application |
| `unsupported kubeconfig setting` | the kubeconfig uses exec, auth-provider, basic auth, impersonation, `insecure-skip-tls-verify` or `proxy-url`; use `kubectl proxy` |
| timeouts with reason `not_observed` | Argo CD has not refreshed (configure its webhook), tracks another branch or path, or the GitHub App cannot read the GitOps repository; a `github compare found no such commit` warning means the Application tracks another repository |
| reason `untracked_revision` | the Application's source is a Helm chart or other non-Git source; point `gitops.repo` at the repository the Application syncs from |
| timeouts with reason `out_of_sync` and class `configuration` | automated sync is off; sync the Application or enable automated sync |
| `syncing → syncing` events and a `deploy.retrying` notification | the deploy did not become healthy in time and doorstep is waiting again on the same pin; the event's `diagnosis` names the signal and node ([engine.md](engine.md#retry-before-rollback)) |

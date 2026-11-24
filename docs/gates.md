# Gates

A gate is a check that decides whether a deployment may continue. Gates run in two phases:

- **before** gates must pass before a deploy starts (the pin is written). The default is `checks`: CI succeeded on the commit.
- **after** gates must pass after Argo CD has synced the pin for the deployment to count as successful. The default is `health`: the Argo CD Application is synced and healthy. That the Application has synced the pin commit is established by the engine's syncing step before after gates run.

Which gates an app runs, their names and their timeouts come from the configuration file ([configuration.md](configuration.md#gates)). Gate results are recorded data, not control flow ([ADR 0001](decisions/0001-go-and-core-design.md), decision 4): every evaluation leaves a row explaining the verdict.

The code lives in `internal/gates`: the data model, the registry, the evaluator, the six [built-in gate types](#built-in-gates) (`checks`, `lock`, `schedule`, `health`, `soak`, `job`), the [`prometheus`](#prometheus) gate type, which reads Prometheus through the `internal/promquery` client, and the [`external`](#external) gate type with the service behind its API. Every gate result is mirrored to a [GitHub commit status](#github-commit-statuses) by `internal/notify`.

## Data model

A gate result is the latest verdict of one named gate for one deployment, stored in `gate_results` ([database.md](database.md#gate_results)) and returned as `core.GateResult`:

| Field | Meaning |
| --- | --- |
| `DeploymentID` | the deployment; its commit is the deployment's commit, so a result is effectively *(commit, gate name, phase, details)*, and a rollback of the same commit gets its own results |
| `Name` | gate name from the configuration, unique within the app across both phases |
| `Type` | gate type from the configuration (`checks`, `lock`, `schedule`, `health`, `soak`, `prometheus`, `job`, `external`, ...) |
| `Phase` | `before` or `after` (`core.GatePhase`) |
| `Status` | `pending`, `passed`, `failed` or `skipped` |
| `Message` | short human-readable explanation |
| `Evidence` | JSON object with the details behind the verdict (`{}` when there are none) |
| `StartedAt` | when the gate started waiting: its first evaluation, or the evaluation that moved it from a verdict back to `pending` |
| `FinishedAt` | when it reached its current verdict; empty while `pending` |
| `UpdatedAt` | the latest evaluation |

All three timestamps come from the database clock. Every change of status also appends a `gate` event to the deployment's history, so earlier verdicts are not lost when the row is updated.

## Evaluating a phase

The engine asks for a verdict on one phase of one deployment:

```go
outcome, err := evaluator.Evaluate(ctx, deployment, core.GatePhaseAfter)
// outcome.Status: passed, pending or failed; outcome.Results in configured order
```

A pass runs every gate of that phase, one after the other in configured order. A failing or pending gate does not stop the pass, so every gate gets a result and the UI always shows the whole picture. Only cancellation of `ctx` stops a pass early.

The phase verdict (`core.AggregateGateStatus`) considers only the gates currently configured for that phase:

| Gate results | Phase verdict |
| --- | --- |
| any `failed` | `failed` |
| otherwise any `pending` | `pending` |
| otherwise (all `passed` or `skipped`, or no gates) | `passed` |

Results left behind by a gate that has since been removed from the configuration are ignored, so they can neither hold nor pass a deploy.

### Sticky results and re-evaluation

`Evaluate` is safe to call repeatedly. The engine polls a phase until it is no longer `pending`, and re-runs the step after a restart:

- a gate with a verdict (`passed`, `failed`, `skipped`) keeps it and is not run again, so a passed CI gate cannot flap;
- a `pending` gate is run again and its row updated.

`Reevaluate` runs every gate of the phase regardless of its recorded result. It is the explicit retry path for the engine and operators: the engine calls it for the `after` phase when a rollback had to pin its digest again, so `health` judges the new pin commit instead of keeping the pass it recorded for the replaced one ([engine.md](engine.md#rollbacks)). A gate that goes from a verdict back to `pending` restarts its clock: `StartedAt` is reset and `FinishedAt` cleared.

Each gate receives the deployment, the phase and its previously recorded result (`Request.Previous`), so a gate that needs state across polls and restarts (a soak start time, the start of a degradation) keeps it in its evidence rather than in its own table.

### Deferred gates

A gate type registered with `Definition.Deferred` (today only `backup`) waits its turn: in a pass the evaluator first settles every gate of the phase that is not deferred, whatever the configured order, and only then the deferred ones, in their configured order. A deferred gate is neither called nor recorded until every non-deferred gate of the phase, and every deferred gate listed before it, is `passed` or `skipped` in that pass. Until then the phase is already `pending` or `failed` because of those gates, so the verdict is unchanged. Results are still reported in configuration order. Without deferral the evaluator would call the gate on every pass while CI is still running or a deploy window holds the deploy for days, taking a stale backup and burning its timeout. Because nothing is recorded while it waits, its timeout clock starts only when it is first called. Where it is listed does not matter. A result it already recorded is kept and reported while an earlier gate is re-run.

### Timeouts

A gate's `timeout` from the configuration (default `30m` for `checks` and `backup`, `10m` for `health` and `job`, the soak `duration` plus `10m` for `soak`, `initial_delay` + `success_threshold` × `interval` + `10m` for `prometheus`) is enforced by the evaluator for every gate type. When a gate is still `pending` and its `UpdatedAt - StartedAt` has reached the timeout, it is recorded as `failed` with the message `timed out after <timeout>`, keeping the gate's last evidence. A gate without a timeout (`lock`, `schedule`) never times out, because a freeze legitimately lasts days; an `external` gate times out after `24h` unless it sets its own. Because both timestamps are database readings, the timeout is measured on the clock all replicas share and survives restarts.

### Errors

A gate that cannot reach a verdict (it returns an error, panics, returns an unknown status, invalid or oversized evidence, or gives no answer within 30 seconds) is treated as a transient problem. It is recorded as `pending` with the message `evaluation error: <reason>` and keeps its previous evidence, the error is logged at `warn` and counted in metrics, and the phase stays `pending` until the gate recovers or its timeout fails it. A Prometheus or GitHub blip shorter than the gate's timeout therefore never fails a deploy by itself, but it is visible, and it cannot hold a deploy forever: an outage that outlasts the timeout fails the gate, and an after gate failing rolls the deploy back.

A gate call that gives no answer within 30 seconds is abandoned: the evaluator stops waiting and logs `gate call abandoned while still running` at `warn`. A gate that ignores `ctx` keeps running in the background, so while an abandoned call of a gate for a deployment has not returned, later passes do not call that gate for that deployment again; they record the evaluation error `previous call has not returned yet` instead. A stuck gate therefore costs at most one goroutine per deployment, and its timeout still fails it.

`Evaluate` itself returns an error only when the result store fails, the app has no gate plan (`ErrNoPlan`), the phase is invalid (`ErrInvalidPhase`) or `ctx` is cancelled. Nothing is recorded for a gate whose call was cut short by cancellation.

### Rollbacks

A rollback re-pins a digest that already passed CI and soak, but it still waits for Argo CD ([ADR 0001](decisions/0001-go-and-core-design.md), decision 5). Gate types are registered with `SkipOnRollback`; for a deployment of kind `rollback` those gates are recorded as `skipped` with the message `skipped for rollback` and never called. `skipped` counts as passed. The skip is a recorded result, so a rollback's history shows which gates it bypassed. A gate whose bypass depends on its configuration (`schedule`, `external`) is registered without `SkipOnRollback` and returns `skipped` with the same message itself.

## Built-in gates

| Type | Phase | Metrics kind | Rollbacks | Default timeout | Reads |
| --- | --- | --- | --- | --- | --- |
| `checks` | before | `ci` | skipped | `30m` | tracked commits, then GitHub |
| `lock` | before | `lock` | skipped | none | freeze locks |
| `schedule` | before | `schedule` | skipped unless `apply_to_rollbacks: true` | `24h` | the clock |
| `health` | after | `argocd` | runs | `10m` | the Argo CD Application |
| `soak` | after | `soak` | skipped | `duration` + `10m` | the Argo CD Application |
| `prometheus` | after | `metrics` | skipped | `initial_delay` + `success_threshold` × `interval` + `10m` | a Prometheus server |
| `job` | after | `job` | skipped | `10m` | one Kubernetes `batch/v1` Job, a user-declared Argo CD PostSync hook |
| `external` | either | `external` | skipped unless `apply_to_rollbacks: true` | `24h` | reports posted through the API |
| `backup` | before (deferred) | `backup` | skipped unless `apply_to_rollbacks: true` | `30m` | an operator-owned webhook |

The defaults are `before: [checks]` and `after: [health]`; `lock`, `schedule`, `soak`, `prometheus`, `job`, `external` and `backup` are opt-in ([configuration.md](configuration.md#gates)). Every gate honours `ctx`, keeps any state it needs across polls in its evidence (timestamps as RFC 3339 UTC), starts fresh when previous evidence cannot be decoded, and never puts credentials in messages or evidence. Strings read from GitHub or Argo CD are cleaned like messages before they are stored as evidence.

### Wiring

The composition root (`serve`) registers the five built-in types with one call, the `prometheus`, `job` and `external` types with further calls, and builds the plans from the catalog. In `internal/cli/gates.go`, `newGateEvaluator` runs every function returned by `gateRegistrars()` against a fresh registry (today `registerBuiltinGates`, the Prometheus registrar, `registerJobGateFromDeps`, `registerExternalGateFromDeps` and `registerBackupGateFromDeps`), builds the plans and creates the evaluator with `*store.Store` as its result store, the metrics recorder and the observers returned by `gateObservers()`; a new gate type or observer adds one entry there and a constructor in its own file, so `serve.go` does not change. The engine evaluates the phases ([engine.md](engine.md#steps)):

```go
registry := gates.NewRegistry()
err := gates.RegisterBuiltins(registry, gates.Builtins{
	Tracked:      st,                                      // *store.Store
	Live:         intake.LiveChecks{Source: githubChecks}, // optional
	Freezes:      lockService,                             // *locks.Service
	Applications: argoApplications,                        // adapter over the argocd package
})
err = registerPrometheusGate(registry, catalog, secretSource{lookupEnv: lookupEnv, readFile: config.ReadSecretFile})
err = registerJobGate(registry, kubeClient)                // internal/cli/jobs.go, only when a gate has type job
err = gates.RegisterExternal(registry, st)                // *store.Store reads gate_reports
plans, err := registry.Plans(catalog)
```

`registerPrometheusGate` (in `internal/cli`) builds one `promquery.Client` per configured `prometheus` server and calls `gates.RegisterPrometheus(registry, gates.Prometheus{Servers: ...})`. It registers the type even when no server is configured, so a gate without a server is rejected by configuration validation rather than by wiring. Without the call, a configuration that uses the gate fails at startup with `unknown gate type "prometheus"`.

`newGateStatusMirror(catalog, cfg, githubApp, logger, recorder)` (in `internal/cli`) returns the [commit status mirror](#github-commit-statuses), which `newEngine` passes to the evaluator as an observer (nil without a GitHub App) and drains on shutdown.

The ports are small interfaces in `internal/gates`:

| Port | Method | Satisfied by |
| --- | --- | --- |
| `TrackedChecks` | `TrackedCommits(ctx, repository, sha) ([]core.TrackedCommit, error)` | `*store.Store` |
| `LiveChecks` | `Checks(ctx, repository, sha, required) (core.ChecksEvaluation, error)` | `intake.LiveChecks`: GitHub checks read through intake's `CheckSource` and judged by `intake.Evaluate` |
| `FreezeReader` | `Blocking(ctx, core.Target) ([]core.Lock, error)` | `*locks.Service` |
| `ApplicationReader` | `Application(ctx, namespace, name) (gates.Application, error)` | `cli.argoApplications`: finds the app configured for that Application and reads it with `(*argocd.Watcher).Status`, mapping not found to `gates.ErrApplicationNotFound` |
| `MetricsQuerier` | `Query(ctx, expr, at) (promquery.Result, error)` and `QueryRange(ctx, expr, from, to, step) (promquery.Result, error)` | `*promquery.Client`, one per `prometheus` server |
| `ExternalReports` | `GetGateReport(ctx, deploymentID, gate) (core.GateReport, error)` | `*store.Store` |
| `JobReader` | `Job(ctx, namespace, name) (gates.Job, error)` | `cli.jobReader`, an adapter over `internal/jobs` (one `GET` of a `batch/v1` Job through `internal/kube`) |

`gates.Application` carries the Application's sync status, revision, health status, health message and operation phase as the plain strings Argo CD reports; `gates` defines constants for the values it acts on (`SyncSynced`, `HealthHealthy`, `HealthDegraded`, `OperationRunning`, ...). The adapter returns an error wrapping `gates.ErrApplicationNotFound` when the Application does not exist. `Builtins.Now` (default `time.Now`) is the clock for soak, grace and schedule decisions, and `Builtins.DegradedGrace` (default `gates.DefaultDegradedGrace`, one minute) exists for tests; neither is a configuration key. `Live` is the only optional dependency; a missing required one is a startup error.

The `backup` type needs an HTTP client, a secret reader and a signer, which `gates` may not take from `notify` itself. It is registered separately so `RegisterBuiltins` keeps its signature. `internal/cli` has the helper the composition root calls next to `RegisterBuiltins`:

```go
err := registerBackupGate(registry, lookupEnv, userAgent(), time.Now)
// = gates.RegisterBackup(registry, gates.BackupDeps{
//	Client:     notify.NewHTTPClient() with a 20s timeout, redirects not followed,
//	ReadSecret: config.SecretRef reader over the environment and secret files,
//	Sign:       notify.Sign,
// })
```

The `job` type is registered separately with `gates.RegisterJobGate(registry, gates.JobGateDeps{Jobs: reader})`, so `gates.Builtins` gains no required field. `gates.Job` carries the Job's UID, creation time, whether it is being deleted, its annotations, `suspend`, `backoffLimit`, the active, succeeded and failed pod counts, start and completion times, and whether its `Complete` or `Failed` condition is `True` (with the `Failed` condition's reason and message); the adapter returns an error wrapping `gates.ErrJobNotFound` when the Job does not exist. If `serve` does not register the type, a configuration using `type: job` fails at startup with `unknown gate type "job"`, never a silently passing gate.

### checks

CI must have succeeded on the deployment's commit.

| Situation | Verdict |
| --- | --- |
| the commit is tracked for this target and its recorded checks are `success` | `passed`, without calling GitHub |
| otherwise, with `Live` set: GitHub reports `success` / `pending` / `failure` | `passed` / `pending` / `failed` |
| otherwise, without `Live`: the recorded checks are `pending` / `failure` | `pending` / `failed` |
| otherwise, without `Live`: the commit is not tracked | `pending` (`commit is not tracked by doorstep; waiting for its checks`) |

Intake only enqueues commits whose checks are green, so webhook and poll deployments pass from recorded state at once and cannot flap. A deployment of a commit intake never saw (an API or CLI deploy of an arbitrary SHA) has no tracked row and is read live. Which checks count follows intake's rules ([configuration.md](configuration.md#apps)): with `source.required_checks`, exactly those; without, everything reported on the commit.

Evidence: `{"source": "tracked" | "live", "state", "read_at", "checks": [{"name", "kind", "state", "detail"}], "truncated"}`. The check list is shortened so the evidence stays under the 64 KiB limit, and `truncated` counts the checks left out. The message names up to five failing or pending checks.

### lock

Holds a deployment while a manual freeze ([locks.md](locks.md)) blocks its target: `pending` with the message `frozen by <owner>: <reason>` (`globally frozen by ...` for a global freeze, followed by `(and N more freezes)` when there are several), `passed` with `no freeze` otherwise. Evidence: `{"locks": [{"scope", "owner", "reason", "created_at", "expires_at"}], "truncated"}`, listing at most 50 freezes.

The queue claim already refuses to start a deployment while a freeze blocks its target ([ADR 0004](decisions/0004-freeze-locks.md)), so this gate is not the enforcement point. It makes the freeze visible as a gate result and a GitHub status, and closes the short window between claiming a deployment and starting it. It is opt-in so existing configurations do not gain a gate row on every deployment.

### schedule

Deploy windows defined as cron expressions with allow and deny periods, configured inline on the gate ([configuration.md](configuration.md#deploy-windows)):

```yaml
gates:
  before:
    - type: checks
    - type: schedule
      timezone: Europe/London
      windows:
        - effect: allow
          name: business hours
          cron: "0 9 * * 1-5"
          duration: 8h
        - effect: deny
          name: weekend
          cron: "0 17 * * 5"
          duration: 63h
```

A window is active at time *now* when some firing *t* of its cron expression satisfies *t* ≤ *now* < *t* + `duration`; the start is inclusive and the end exclusive. `duration` is elapsed time, so a window that spans a daylight saving change is still exactly that long. The verdict:

| Windows active now | Verdict | Message |
| --- | --- | --- |
| any `deny` window | `pending` | `deploys are frozen until <end> (<name or cron>)` |
| no `deny`; the gate has `allow` windows and none is active | `pending` | `outside deploy windows; next opens <time>`, where `<time>` is the first moment an `allow` window is open and no `deny` window is active; the suffix is left out when no such moment is found within 100 deny windows |
| an `allow` window and no `deny` | `passed` | `inside deploy window until <end> (<name or cron>)` |
| none, and the gate has no `allow` windows | `passed` | `no deny window is active` |

Deny wins. The start and end shown for a window extend across earlier and later firings that overlap or touch it, so an hourly window with a 90 minute duration reports when the run of firings began and when it ends (each direction follows at most 1000 firings). A `pending` verdict leaves the deployment waiting at the head of its target's queue; it starts on the first poll after the window opens. A pass is sticky ([above](#sticky-results-and-re-evaluation)), so a deploy that started inside a window is not stopped when the window closes: windows stop the start of a deploy, not a deploy in flight. The gate has no timeout, because a freeze may last days.

For a rollback the gate returns `skipped` (`skipped for rollback`) unless the gate sets `apply_to_rollbacks: true`, in which case rollbacks wait for the window like any deploy. The default lets recovery through a freeze, like manual freeze locks ([ADR 0004](decisions/0004-freeze-locks.md)).

Evidence: `{"evaluated_at", "active": [{"effect", "name", "cron", "timezone", "start", "end"}], "next_allow"}`; `next_allow` is the `<time>` of the message.

Cron firings are wall-clock times in the window's time zone and follow `robfig/cron` across daylight saving changes: a firing inside the spring-forward gap is skipped that day, and a firing inside the fall-back overlap fires twice, once in each offset. In `America/New_York`, `30 2 * * *` does not fire on 2026-03-08, so an allow window at that time holds deploys an extra day and a deny window at that time does not apply that day; `30 1 * * *` fires at 05:30 and 06:30 UTC on 2026-11-01, opening two windows (one run of firings when `duration` covers the hour between them). Schedule windows outside 01:00-03:00 local time, or use a zone without daylight saving such as `UTC`, to avoid both.

The semantics follow Argo CD sync windows (kind, schedule, duration, time zone), and expressions are parsed by the library Kubernetes CronJobs and Argo CD use (`robfig/cron`), restricted to five fields ([ADR 0011](decisions/0011-built-in-gates-and-deploy-schedules.md)). The parsing and window arithmetic live in `internal/windows`, which `config` also uses, so a configuration that validates cannot fail to build its gates at startup.

### health

The Argo CD Application (`argocd.namespace`/`argocd.application`) must be synced and healthy.

| Application | Verdict |
| --- | --- |
| sync `Synced`, health `Healthy`, and no operation `Running` | `passed` |
| health `Degraded` one minute or more after it was first seen `Degraded`, without a synced and healthy observation in between | `failed` (`Argo CD Application <ns>/<name> stayed degraded for 1m0s: <health message>`) |
| health `Degraded` for less than that minute | `pending` |
| anything else (`Progressing`, `OutOfSync`, `Missing`, `Suspended`, `Unknown`, an operation running, the Application not found) | `pending` with an explanatory message, until the timeout fails it |

Right after a sync Argo CD can still show the previous version's `Degraded` state, which is exactly what happens when a fix is deployed to a degraded app, and a false failure triggers an automatic rollback, so degradation must persist for the grace before it fails the gate. The start of the degradation is kept as `degraded_since` in the evidence of the `pending` result. It is carried through every observation that is not synced and healthy (`Progressing`, `OutOfSync`, not found, ...) and dropped only when the Application is synced and healthy, so an Application flapping between `Degraded` and `Progressing`, as a crash-looping workload does, fails on the first `Degraded` observation after the grace rather than at the timeout. Only a `Degraded` observation fails the gate; a carried `degraded_since` alone does not. Errors reading the Application other than not found are evaluation errors ([Errors](#errors)).

The gate does not compare the Application's revision with the pin commit: in a GitOps repository shared by several apps another app's pin moves the Application's revision. Evidence: `{"application", "found", "sync", "health", "health_message", "revision", "operation_phase", "degraded_since", "pin_commit"}`.

### soak

Requires the Application to stay healthy for `duration` after the deploy. The gate reads the Application itself, so it also catches a regression that appears after the health gate passed.

- The soak clock starts at the first evaluation that sees the Application synced and healthy (`healthy_since`) and is not reset when the Application is briefly `Progressing`, `OutOfSync` or not found, so autoscaling does not restart the soak.
- Any `Degraded` observation stops the clock: `healthy_since` is dropped and the soak starts again from the next synced and healthy observation, so a soak never passes across a degraded stretch.
- The gate passes once `duration` has elapsed since `healthy_since` and the Application is synced and healthy at that moment; until then it is `pending` with the stable message `soaking until <time>`.
- `Degraded` for one minute fails it, with the same grace and the same `degraded_since` carrying as `health`.
- The clock is carried only from a `pending` previous result, so `Reevaluate` of a passed or failed soak starts a new soak.
- The timeout defaults to `duration` + `10m`, so an Application that never becomes healthy cannot hold the target forever.

Evidence: `{"application", "found", "sync", "health", "health_message", "revision", "operation_phase", "degraded_since", "healthy_since", "soak", "until"}`.

### prometheus

Evaluates a PromQL expression after the deploy and passes once it has been good for long enough, optionally comparing each reading with the level before the deploy ([ADR 0013](decisions/0013-prometheus-query-gate.md)). The server and its credentials come from the top-level `prometheus` list ([configuration.md](configuration.md#prometheus)).

```yaml
gates:
  after:
    - type: health
    - type: prometheus
      name: error-rate
      query: sum(rate(http_requests_total{app="api",code=~"5.."}[5m])) / sum(rate(http_requests_total{app="api"}[5m]))
      condition: "< 0.05"
      initial_delay: 2m
      baseline:
        ratio: 0.2
        absolute: 0.001
```

**Sampling.** The gate takes at most one sample per `interval` (default `1m`): an instant query at the current time. The first sample is taken `initial_delay` (default `0s`) after the gate's first evaluation. Polls inside the interval return the previous message and evidence without contacting Prometheus, so several replicas polling the same gate do not multiply the load. Attempt and start times are compared with a tolerance of 5 seconds, so a replica whose clock is slightly behind the one that took the last sample still waits instead of taking an extra sample that would count towards `success_threshold`; a recorded time more than 5 seconds in the future is treated as a skewed clock and ignored rather than trusted. The measurement window of each sample is the range selector in the query (`[5m]` above); the gate does not average samples itself.

| Samples | Verdict |
| --- | --- |
| `success_threshold` (default `5`) successes in a row | `passed` (`5 consecutive samples passed; latest 0.004`) |
| `failure_threshold` (default `3`) failures in a row | `failed` (`3 consecutive samples failed; latest 0.08 (want < 0.05)`) |
| otherwise | `pending` (`3/5 samples passed; latest 0.004 (want < 0.05; baseline limit <= 0.013)`) |

A success resets the failure streak and a failure resets the success streak. With the defaults the gate passes after about four minutes of good samples and fails after about two minutes of bad ones; the effective window is (`success_threshold` - 1) × `interval`. The timeout defaults to `initial_delay` + `success_threshold` × `interval` + `10m`, and configuration validation rejects a timeout no longer than `initial_delay` + (`success_threshold` - 1) × `interval`, which could never pass. Because a failed after gate rolls the deploy back, `failure_threshold` defaults to more than one sample.

**A sample succeeds** when every configured comparison holds:

- `condition`, such as `"< 0.05"`: an operator (`<`, `<=`, `>`, `>=`, `==`, `!=`) and a finite number;
- `baseline`: the value stays within a margin of the level before the deploy.

At least one of the two is required; with both, both must hold, which reads as "no worse than before, and never above a hard ceiling".

**Baseline.** The baseline is one number: the mean of the same query evaluated as a range query over [*anchor* - `window`, *anchor*] with step `interval`, where *anchor* is the deployment's start time (just before the pin was written), or its creation time when it has none. NaN and infinite points are ignored. It is computed on the first sample, stored in the evidence and reused for the rest of the deployment, so samples are never compared with post-deploy data. With `direction: increase` (the default; higher is worse) a sample passes when

*value* ≤ *baseline* + \|*baseline*\| × `ratio` + `absolute`

and with `direction: decrease` (lower is worse, for throughput or success rates) when

*value* ≥ *baseline* - \|*baseline*\| × `ratio` - `absolute`.

`absolute` covers a zero baseline, where a ratio alone would fail on the first error. `window` defaults to `30m`.

**No data.** An empty result or a NaN value (for example `0/0` when there is no traffic) is no data. With `no_data: pending` (the default) such a sample counts neither way and does not reset the streaks, so the gate waits and its timeout eventually fails it. With `no_data: fail` it counts as a failing sample; when every sample of the failing streak had no data the message is `no data for 3 consecutive samples`. There is no option to pass on no data, because a misspelt metric name would then pass forever; write `or vector(0)` into the query when an absent series really means zero:

```yaml
query: (sum(rate(http_requests_total{code=~"5.."}[5m])) or vector(0)) / sum(rate(http_requests_total[5m]))
```

When the baseline window has no data (a new app or a new metric), `baseline.no_data: ignore` (the default) skips the baseline comparison for this deployment, records `"status": "missing"` and appends `baseline unavailable, compared against the condition only` to the message (`baseline unavailable, so any value passes` without a `condition`); `baseline.no_data: fail` fails the gate at once with `no baseline data`. Missing history never appears later, so waiting would only end in a timeout.

**Result shape.** The query must return a scalar or an instant vector with exactly one series, and the baseline range query at most one series. Aggregate with `sum`, `max` or `min`, which also makes the intent explicit. More series, or a range vector or string from the instant query, leaves the gate `pending` with `query failed: query returned 2 series; aggregate it to a single value`.

**Errors.** A query that Prometheus rejects as invalid, a `400` or `422` response with error type `bad_data` (a PromQL syntax error or a bad parameter, which configuration validation cannot catch), fails the gate at once with `query failed: <reason>` (`baseline query failed: <reason>` for the baseline): it would fail the same way on every sample and every deploy, so waiting for the timeout would only delay the rollback. Any other error, a network error, a timeout, another `4xx` or `5xx` response (including `422 execution` for a query that ran out of time or memory), or a malformed body, leaves the gate `pending` with the same message and records `last_attempt_at` and `last_error`. Those errors count neither as success nor as failure, so a Prometheus blip shorter than the gate's timeout never fails a deploy, and the interval still applies, so a Prometheus that is down is not queried on every poll by every replica. An outage that lasts until the timeout does fail the gate, with `timed out after <timeout>`, and so rolls the deploy back; the evidence keeps `last_error`, which shows the cause. They are gate results rather than evaluation errors, so they do not appear as `error` observations in the gate duration metric. Only cancellation is returned to the evaluator as an error.

**Client.** `internal/promquery` is a small standard library client: `POST` form requests to `<url>/api/v1/query` and `<url>/api/v1/query_range` (long queries fit, and a path prefix in `url` is kept), a 10 second timeout per request, a 1 MiB response limit, `User-Agent: doorstep/<version>`, no redirects (so the `Authorization` header never reaches another host), and the bearer token or password read from its secret on every request, so a rotated token takes effect without a restart. Prometheus, Thanos, Mimir and VictoriaMetrics serve this API.

State lives in the evidence of the `pending` result and is carried only from a `pending` previous result, so `Reevaluate` starts over, and so does a change of server or query. Evidence: `{"server", "query", "query_sha256", "condition", "interval", "success_threshold", "failure_threshold", "started_at", "last_attempt_at", "consecutive_successes", "consecutive_failures", "consecutive_no_data", "last_error", "baseline": {"status": "ok" | "missing", "direction", "value", "limit", "from", "to", "points"}, "samples": [{"at", "value", "outcome": "success" | "failure" | "no_data"}]}`. Values are strings (`"0.004"`, `"+Inf"`), because JSON numbers cannot hold NaN or infinities; `samples` keeps the last 10. `query` holds the configured query cut to 1024 bytes for display, and `query_sha256` the SHA-256 of the whole query, which is what detects a changed query; the server URL and credentials never appear in evidence or messages.

### job

Runs your own validation (smoke tests, API checks, browser tests) as a Kubernetes Job after the deploy and uses its exit status as the verdict. doorstep never creates the Job: you declare it in the app's GitOps manifests as an Argo CD [PostSync hook](https://argo-cd.readthedocs.io/en/stable/user-guide/resource_hooks/) with a fixed name, Argo CD creates and reruns it on every sync, and doorstep reads its status with one `GET` per evaluation ([ADR 0016](decisions/0016-validation-job-gate.md)). The Job spec is reviewed in git like every other manifest, and Argo CD remains the only thing that applies manifests to the cluster.

```yaml
gates:
  after:
    - type: health
    - type: job
      name: smoke
      timeout: 10m
      job:
        namespace: web-prod
        name: web-smoke-test
        logs_url: https://argocd.example.com/applications/{argocd_namespace}/{argocd_application}?resource=&node=batch%2FJob%2F{namespace}%2F{job}%2F0&tab=logs
```

The Job lives next to the app's other manifests, in the Application's destination namespace:

```yaml kubernetes
apiVersion: batch/v1
kind: Job
metadata:
  name: web-smoke-test
  annotations:
    argocd.argoproj.io/hook: PostSync
    argocd.argoproj.io/hook-delete-policy: BeforeHookCreation
spec:
  backoffLimit: 2
  activeDeadlineSeconds: 600
  template:
    spec:
      restartPolicy: Never
      containers:
        - name: smoke
          image: ghcr.io/example-org/web-smoke@sha256:0000000000000000000000000000000000000000000000000000000000000000
          args: ["--base-url", "http://web.web-prod.svc"]
```

Argo CD runs PostSync hooks once every resource of the sync is synced and healthy, and keeps the sync operation `Running` until they finish. The engine's syncing step waits for the operation to end, so when after gates start, the Job present was created by the pin's sync or a later one. The verdict:

| Job | Verdict | Message |
| --- | --- | --- |
| being deleted (`deletionTimestamp` set: Argo CD is replacing it) | `pending` | `Job <ns>/<name> is being replaced; waiting for Argo CD to recreate it` |
| not an Argo CD PostSync hook | `failed` | `Job <ns>/<name> is not an Argo CD PostSync hook, so it does not rerun on each deploy` |
| condition `Complete=True` | `passed` | `Job <ns>/<name> succeeded in <runtime>` (the runtime is left out when a timestamp is missing) |
| condition `Failed=True` | `failed` | `Job <ns>/<name> failed: <reason>: <message>`, for example `BackoffLimitExceeded` or `DeadlineExceeded` |
| suspended | `pending` | `Job <ns>/<name> is suspended` |
| otherwise (created, running, retrying) | `pending` | `Job <ns>/<name> is running: 1 active, 2 failed of backoff limit 6` |
| not found | `pending` | `Job <ns>/<name> not found; waiting for Argo CD to create it` |

Only the Job's terminal conditions decide. Retries belong to the Job's `backoffLimit`, so failed pods within the limit keep the gate `pending`. A Job that is missing, suspended or still running when the gate's `timeout` (default `10m`) runs out fails the gate. Errors reading the Job other than not found, including `401` and `403` when the RBAC component is missing, are evaluation errors ([Errors](#errors)): visible, `pending`, and bounded by the timeout.

A Job counts as a PostSync hook when its `argocd.argoproj.io/hook` annotation lists `PostSync` (comma-separated, so `PostSync,SyncFail` qualifies), or, when that annotation is absent or empty, when its `helm.sh/hook` annotation lists `post-install` or `post-upgrade`, which Argo CD maps to PostSync. The check is what ties the verdict to this deployment: a plain Job is not rerun by a sync, so its old success would otherwise pass every later deploy. The Job's `created_at` and `uid` are recorded but not compared with the deployment, because Argo CD rightly does not resync an unchanged pin.

**Requirements on the Job.**

- A fixed `metadata.name` (no `generateName`) equal to `job.name`, in the namespace `job.namespace`. `argocd.namespace` is where the Application lives, not the workload, which is why the namespace is explicit.
- The default `BeforeHookCreation` delete policy, so every sync deletes the previous run and creates a fresh one. Do not use `HookSucceeded` or `HookFailed`, and do not set a short `ttlSecondsAfterFinished`: a Job removed before doorstep reads it leaves the gate `pending` until the timeout fails it.
- Retries through `backoffLimit`, and a bound through `activeDeadlineSeconds`. The first terminal failure is sticky, as for every gate.
- `argocd.sync_timeout` must cover the rollout plus the Job's runtime, because Argo CD keeps the operation `Running` while PostSync hooks run and the syncing step waits for it ([argocd.md](argocd.md#timeouts)). The gate's own `timeout` then only covers a Job that is missing, being replaced or still running after the sync ended.

Hook Jobs do not count towards the Application's health, so a failed validation does not leave the Application `Degraded`, and the rollback that follows is judged on the workload alone.

**Logs.** doorstep reads no Pods and no pod logs. `job.logs_url` is an optional link template whose rendered value is stored as `logs_url` in the evidence, which the API returns and the UI and status mirror can link to. Its placeholders are replaced with path-escaped values:

| Placeholder | Value |
| --- | --- |
| `{app}` | the doorstep app name |
| `{namespace}` | `job.namespace` |
| `{job}` | `job.name` |
| `{uid}` | the Job's UID (empty when the Job was not found) |
| `{deployment}` | the deployment ID |
| `{argocd_namespace}` | `argocd.namespace` |
| `{argocd_application}` | `argocd.application` |

Besides the Argo CD UI link above, Grafana with Loki works too: `https://grafana.example.com/explore?orgId=1&left=%7B%22queries%22%3A%5B%7B%22expr%22%3A%22%7Bnamespace%3D%5C%22{namespace}%5C%22%2Cjob_name%3D%5C%22{job}%5C%22%7D%22%7D%5D%7D`. Literal braces must be percent-encoded (`%7B`, `%7D`); any other `{...}` is rejected by validation, as are schemes other than `http` and `https`, a missing host and credentials in the URL. Without a template there is no link.

**Rollbacks.** The gate is registered with `SkipOnRollback`, so a rollback records it as `skipped for rollback` and does not read the Job: a rollback re-pins a digest that already passed its checks, and a flaky browser test must not fail recovery ([ADR 0001](decisions/0001-go-and-core-design.md), decision 5). Argo CD still runs the hook during the rollback's sync.

**RBAC.** The gate needs `get` on `batch/v1` Jobs, which the base install does not grant. Add the `components/job-gate` component, or bind the same ClusterRole per namespace ([install.md](install.md#validation-jobs)).

Evidence: `{"job", "found", "uid", "created_at", "hook", "state", "active", "succeeded", "failed", "backoff_limit", "started_at", "completed_at", "reason", "message", "logs_url"}`, where `state` is one of `not_found`, `terminating`, `not_a_hook`, `succeeded`, `failed`, `suspended` and `active`, and the pod counts are `0` when the Job was not found. The gate keeps no state between polls.

### external

Waits for another system, such as an end-to-end test job, a load test or a change approval tool, to post a verdict through the [external gate API](api.md#external-gate-results). It is the only gate type allowed in either phase:

```yaml
gates:
  before:
    - type: checks
    - type: external
      name: change-approval
      timeout: 168h
      reporters: [user:github:1234567]
  after:
    - type: health
    - type: external
      name: e2e
      timeout: 30m
      reporters: [token:ci-e2e]
```

A posted result is stored as a report in `gate_reports` ([database.md](database.md#gate_reports)), one per deployment and gate, by the API alone. The gate reads that report on each evaluation and returns it as its verdict, so the evaluator stays the only writer of gate results, and timeouts, sticky results, events, metrics and the commit status mirror all work as for any other gate. The verdict lands on the engine's next poll of the phase.

| Report | Verdict | Message |
| --- | --- | --- |
| none yet | `pending` | `waiting for an external result` |
| `pending` | `pending` | the report's message, or `reported pending by <actor>` |
| `passed` | `passed` | the report's message, or `reported passed by <actor>` |
| `failed` | `failed` | the report's message, or `reported failed by <actor>` |
| any, on a rollback without `apply_to_rollbacks: true` | `skipped` | `skipped for rollback` |

Evidence: `{"reported": false}` before a report, then `{"reported": true, "status", "reported_by", "reported_at", "url", "details"}`, where `details` is the object the reporter posted. A read error is an [evaluation error](#errors).

`timeout` defaults to `24h`, so a tool that never reports cannot hold the deployment, and every deployment queued behind it, forever. An approval before the deploy that may take days needs a longer one; an after-deploy gate runs while the new version is already live, so give it a short one. When it runs out the gate fails with `timed out after <timeout>`, which fails the deployment.

`reporters` limits who may post: when it is set, only the listed actors (`token:<name>` or `user:<provider>:<subject>`, the `reported_by` form) are accepted and everyone else gets `403`, admins included. Without it any caller with the `reporter` role or higher may report every external gate of every app, so set it on gates that guard production ([authentication.md](authentication.md#roles)).

A final report and a timeout cannot both win. Saving a report locks the gate's result row and refuses the report when the result is already final; recording a final result locks the same row and is refused with `core.ErrGateReported` when a final report with a different verdict exists. The evaluator treats that refusal of a timeout as "still pending", and its next poll reads the report and records the reported verdict. So a `200` for `passed` or `failed` means the gate ends with that verdict. A report posted after the gate has a final result is refused with `409 gate_decided`, so a late `passed` cannot disagree with the recorded verdict. `Reevaluate` reads the same report again, so it keeps a reported verdict.

Rollbacks do not wait on an external tool by default: the gate returns `skipped` (`skipped for rollback`) and the API refuses reports for the rollback. Set `apply_to_rollbacks: true` to make rollbacks wait for a result too.

A typical flow for an after-deploy test job:

1. Create a token for the job: `doorstep token create --name ci-e2e --role reporter`, and list it in the gate's `reporters` as `token:ci-e2e`.
2. The job starts when the `doorstep/<app>/after/e2e` commit status turns `pending` (the after phase is being evaluated), or polls the deployment API.
3. It may post `pending` with a progress message while it runs, then `passed` or `failed` with a link to its logs, to `POST /api/v1/apps/<app>/commits/<sha>/gates/e2e`.
4. Retries of the final post are safe: the same verdict answers `200` again and writes nothing.

Reports for a gate in `gates.after` are accepted only while the deployment is `verifying`, so a result about the old version cannot be posted before Argo CD synced the new one; earlier posts answer `409 gate_not_ready` with `Retry-After: 5`. The checks use the configuration, not the engine, and live in `gates.Reporter`, which the API calls.

### backup

Asks an operator-owned webhook to back the database up before a release is pinned, and holds the deploy until the backup is done. Whatever takes the backup (a Kubernetes Job launcher, a script on a NAS, a managed database API) belongs to the operator; doorstep only sends an HTTP request, so it still never writes to the cluster ([ADR 0015](decisions/0015-migration-aware-releases.md)).

```yaml
gates:
  before:
    - type: checks
    - type: backup
      when: migration          # default; or always
      apply_to_rollbacks: true # default false
      timeout: 45m             # default 30m
      webhook:
        url_env: BLOG_BACKUP_URL   # or url, or url_file
        secret_env: BLOG_BACKUP_SECRET  # required, or secret_file
```

> **Not active yet.** `serve` builds no gate plans and evaluates no gates until the engine (bead doorstep-ehl.4.4) lands, so a configured `backup` gate is validated at startup but no backup is requested yet. Do not rely on it until then.

When it runs:

- For a rollback without `apply_to_rollbacks: true` it is `skipped` with `skipped for rollback`. With it, `when` applies to the rollback row's own flag, which a [confirmed rollback across migrations](rollback.md#confirmation) sets.
- With `when: migration` (the default) a deployment that is not [flagged as containing database migrations](rollback.md#migration-aware-releases) is `skipped` with `release is not flagged as a database migration`. With `when: always` it runs for every deploy.
- It is [deferred](#deferred-gates): it is first called once every other `before` gate has passed or been skipped, wherever it is listed, so the backup is taken right before the pin, not while CI is still running.

Protocol: the gate `POST`s the same JSON document on its first call and on every poll, at most every 15 seconds, each with a 20 second request timeout:

```json
{
  "event": "backup.requested",
  "gate": "backup",
  "requested_at": "2026-11-24T12:00:00Z",
  "deployment": {
    "id": 44, "app": "blog", "environment": "default", "kind": "deploy",
    "commit": {"repository": "example-user/blog", "sha": "0123..."},
    "image": {"repository": "ghcr.io/example-user/blog", "tag": "0123...", "digest": "sha256:..."},
    "migration": true, "migration_reason": "label db-migration on #12 (0123456)"
  }
}
```

| Header | Value |
| --- | --- |
| `Content-Type` | `application/json` |
| `User-Agent` | `doorstep/<version>` |
| `X-Doorstep-Event` | `backup` |
| `Idempotency-Key` | `doorstep-backup-<deployment id>-<gate name>-<requested_at>`, the same on every poll of one backup request; a re-run after a verdict (`Reevaluate`) starts a new request with a new `requested_at` and so a new key |
| `X-Doorstep-Signature-256` | `sha256=<hex HMAC-SHA256 of the body>`, exactly as for [notification webhooks](notifications.md); the secret is required, because the endpoint starts a database backup |

The receiver may answer with an optional JSON body `{"status": "running" | "succeeded" | "failed", "message": "...", "reference": "..."}` (at most 4 KiB is read). The verdict:

| Answer | Verdict |
| --- | --- |
| body `status: failed` (any HTTP status) | `failed` |
| `2xx` with `status: succeeded` | `passed` |
| `202`, or `2xx` with `status: running` | `pending`, polled again |
| any other `2xx` | `passed` |
| `3xx` | `failed`: redirects are not followed |
| `408`, `425`, `429`, `5xx`, a network error or no answer within 20 seconds | `pending`, retried |
| any other `4xx` | `failed` |

A gate still `pending` when its timeout (default `30m`) is reached fails: no backup, no deploy. A receiver must either answer within 20 seconds with the final result (`200` when done) or treat the `Idempotency-Key` as the identity of one backup: start it on the first request, answer `202` (or `running`) while it runs and the final status afterwards, without starting a second backup. It must verify the signature over the raw body before acting. Because a re-run uses a new key, a receiver that caches a `failed` answer per key still lets an operator retry the gate. Evidence: `{"requested_at", "last_request_at", "requests", "status_code", "state", "reference"}`; the URL is never stored or logged. `message` and `reference` appear in the gate message, so they must not carry secrets.

## Limits

- Messages are cut to 1024 bytes (on a UTF-8 boundary); NUL bytes are removed and invalid UTF-8 is replaced.
- Evidence must be valid JSON of at most 64 KiB and no string in it (key or value) may contain the NUL character `\u0000`, which Postgres `jsonb` cannot store. An escaped backslash followed by `u0000` (`"\\u0000"`) is ordinary text and allowed. Anything else is an evaluation error.
- The limits live in `core` (`core.CleanGateMessage`, `core.ValidateGateEvidence`) and are enforced by both the evaluator and `Store.RecordGateResult`, so any other writer of gate results gets the same cleaning and a validation error instead of a raw Postgres error.
- Evidence and messages are shown in the UI, and messages are mirrored to GitHub for apps that set `source.status_messages: true`. Gates must never put secrets, tokens or credentials in them.

## Adding a gate type

A gate implements one method and decides only the verdict; the evaluator stamps the deployment, name, type, phase and timestamps:

```go
type Gate interface {
	Evaluate(ctx context.Context, req gates.Request) (gates.Evaluation, error)
}
```

It must honour `ctx`. A gate type is registered once at startup with the metrics kind it reports as, whether rollbacks skip it, whether it is [deferred](#deferred-gates), and a factory that builds one instance per configured gate from the typed `config.App` and `config.Gate`:

```go
registry := gates.NewRegistry()
err := registry.Register(config.GateHealth, gates.Definition{
	Kind: metrics.GateKindArgoCD,
	New: func(app config.App, g config.Gate) (gates.Gate, error) { ... },
})
plans, err := registry.Plans(catalog)
```

`Plans` turns every app's `gates.before` and `gates.after` into a `Plan` of `Binding`s (name, type, kind, phase, timeout, skip-on-rollback, deferred and the built gate). A gate type with no registered definition, or a factory error, is a startup error naming the app, phase and gate, never a silently passing gate. All errors are reported together. `NewRegistry` returns an empty registry; `RegisterBuiltins` adds the [built-in types](#wiring).

## Observers

Something that must react to gate results implements `Observer`:

```go
type Observer interface {
	GateRecorded(ctx context.Context, d core.Deployment, result core.GateResult) error
}
```

Observers are called after a result is committed, only when its status changed (including the first `pending` and `skipped`), so a pending gate polled every few seconds does not call the GitHub API on every poll. Observers are best effort: an error is logged at `warn` with the deployment and gate and never changes the verdict. Retrying a failed mirror is the observer's own responsibility.

### GitHub commit statuses

`notify.GateStatuses` is the observer that publishes every gate as a commit status on the deployment's app commit, so GitHub shows deploy state next to CI. It runs when a GitHub App is configured, for deploys and rollbacks alike, on every app whose `source.statuses` is not `false` ([configuration.md](configuration.md#apps)).

| Gate result | Commit status |
| --- | --- |
| context | `doorstep/<app>/<phase>/<gate>`, for example `doorstep/blog/after/health` |
| `pending` | `pending` |
| `passed`, `skipped` | `success` (GitHub has no skipped state, and doorstep counts `skipped` as passed) |
| `failed` | `failure` |
| description | fixed: `Gate pending`, `Gate passed`, `Gate skipped` or `Gate failed`, prefixed `Rollback #<id>: ` for a rollback; with `source.status_messages: true`, the gate message instead (the fixed text when it is empty), cut to 140 characters |
| target URL | `<public url>/deployments/<id>` when [`--public-url`](configuration.md#doorstep-serve) is set, left out otherwise |

Commit statuses are visible to everyone who can read the repository, which is everyone for a public one. A message from an external reporter or an Argo CD health message can name internal systems, so by default descriptions are fixed, as for [GitHub deployment statuses](notifications.md#github-deployments), and the details are one click away through the target URL. Set `source.status_messages: true` to post the gate message instead, for private repositories or where messages are known to be safe.

A status is posted only when the gate's status changes ([Observers](#observers)). A `pending` external report replaced by another `pending` one with a new message does not change the gate's status, so with `source.status_messages: true` the description keeps the first pending message until the verdict; the latest progress message is in the deployment API and the UI.

`GateRecorded` never blocks the engine. It records the latest desired status per repository, commit and context in memory, queues that key if it is not queued yet and returns. One worker delivers keys in the order they were first queued, always sending the newest status of the key, so a GitHub outage or a slow call can delay statuses but never lets an older one overwrite a newer one: a `pending` that has not been sent when the gate passes is replaced by the `success`. At most 1024 keys wait; beyond that a status is dropped with a `warn`. A delivery is tried up to four times with backoff, honouring GitHub's `Retry-After` and rate limit headers. A `403`, `404`, missing or suspended installation is not retried and logs `cannot mirror gates to github` at `warn` at most once per repository per hour with a permission hint ([github-app.md](github-app.md#permissions)); a `422` is not retried either. Deliveries are counted in `doorstep_notifications_total` with `type="github"` ([metrics.md](metrics.md)). `Close` drains the queue on shutdown within the shutdown deadline.

Statuses are best effort, like notifications ([ADR 0006](decisions/0006-notifications-are-best-effort.md)): there is no durable outbox, so a crash can lose the last update of a gate until its next change. Gate results in doorstep stay authoritative.

The `doorstep/` context prefix is reserved ([ADR 0014](decisions/0014-external-gates-and-commit-status-mirror.md)). Intake ignores commit statuses with that prefix when it decides whether a commit is green, the `status` webhook ignores them, and `source.required_checks` may not name them. Otherwise, with `required_checks` empty, doorstep's own pending `doorstep/<app>/before/checks` status would hold the `checks` gate forever, and a failed after-deploy status would block retries and other apps that track the same commit.

## Metrics

Evaluations are recorded in `doorstep_gate_evaluation_duration_seconds` ([metrics.md](metrics.md)):

- `gate` is the kind from the gate type's registration (`ci`, `lock`, `schedule`, `argocd`, `soak`, `metrics` for `prometheus`, `job`, `external`, ...), never the configured name;
- `phase` is the deployment kind (`deploy` or `rollback`), not `before`/`after`;
- one `passed` or `failed` observation when a gate reaches that verdict, with the time from `StartedAt` to `FinishedAt` plus the wall time of the call that produced the verdict (`StartedAt` is stamped after the first call returns, so a gate that decides on its first evaluation records the duration of that call, not 0; a timeout counts as `failed`);
- one `error` observation per evaluation error, with the wall time of that call;
- nothing for `pending` polls or `skipped` gates.

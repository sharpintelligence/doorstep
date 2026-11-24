# Gates

A gate is a check that decides whether a deployment may continue. Gates run in two phases:

- **before** gates must pass before a deploy starts (the pin is written). The default is `checks`: CI succeeded on the commit.
- **after** gates must pass after Argo CD has synced the pin for the deployment to count as successful. The default is `health`: the Argo CD Application is synced and healthy. That the Application has synced the pin commit is established by the engine's syncing step before after gates run.

Which gates an app runs, their names and their timeouts come from the configuration file ([configuration.md](configuration.md#gates)). Gate results are recorded data, not control flow ([ADR 0001](decisions/0001-go-and-core-design.md), decision 4): every evaluation leaves a row explaining the verdict.

The code lives in `internal/gates`: the data model, the registry, the evaluator and the five [built-in gate types](#built-in-gates) (`checks`, `lock`, `schedule`, `health`, `soak`).

## Data model

A gate result is the latest verdict of one named gate for one deployment, stored in `gate_results` ([database.md](database.md#gate_results)) and returned as `core.GateResult`:

| Field | Meaning |
| --- | --- |
| `DeploymentID` | the deployment; its commit is the deployment's commit, so a result is effectively *(commit, gate name, phase, details)*, and a rollback of the same commit gets its own results |
| `Name` | gate name from the configuration, unique within the app across both phases |
| `Type` | gate type from the configuration (`checks`, `lock`, `schedule`, `health`, `soak`, ...) |
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

`Reevaluate` runs every gate of the phase regardless of its recorded result. It is the explicit retry path for the engine and operators. A gate that goes from a verdict back to `pending` restarts its clock: `StartedAt` is reset and `FinishedAt` cleared.

Each gate receives the deployment, the phase and its previously recorded result (`Request.Previous`), so a gate that needs state across polls and restarts (a soak start time, a launched Job name) keeps it in its evidence rather than in its own table.

### Timeouts

A gate's `timeout` from the configuration (default `30m` for `checks`, `10m` for `health`, the soak `duration` plus `10m` for `soak`) is enforced by the evaluator for every gate type. When a gate is still `pending` and its `UpdatedAt - StartedAt` has reached the timeout, it is recorded as `failed` with the message `timed out after <timeout>`, keeping the gate's last evidence. A gate without a timeout (`lock`, `schedule`) never times out, because a freeze legitimately lasts days. Because both timestamps are database readings, the timeout is measured on the clock all replicas share and survives restarts.

### Errors

A gate that cannot reach a verdict (it returns an error, panics, returns an unknown status, invalid or oversized evidence, or gives no answer within 30 seconds) is treated as a transient problem. It is recorded as `pending` with the message `evaluation error: <reason>` and keeps its previous evidence, the error is logged at `warn` and counted in metrics, and the phase stays `pending` until the gate recovers or its timeout fails it. A Prometheus or GitHub blip therefore never fails a deploy by itself, but it is visible, and it cannot hold a deploy forever.

A gate call that gives no answer within 30 seconds is abandoned: the evaluator stops waiting and logs `gate call abandoned while still running` at `warn`. A gate that ignores `ctx` keeps running in the background, so while an abandoned call of a gate for a deployment has not returned, later passes do not call that gate for that deployment again; they record the evaluation error `previous call has not returned yet` instead. A stuck gate therefore costs at most one goroutine per deployment, and its timeout still fails it.

`Evaluate` itself returns an error only when the result store fails, the app has no gate plan (`ErrNoPlan`), the phase is invalid (`ErrInvalidPhase`) or `ctx` is cancelled. Nothing is recorded for a gate whose call was cut short by cancellation.

### Rollbacks

A rollback re-pins a digest that already passed CI and soak, but it still waits for Argo CD ([ADR 0001](decisions/0001-go-and-core-design.md), decision 5). Gate types are registered with `SkipOnRollback`; for a deployment of kind `rollback` those gates are recorded as `skipped` with the message `skipped for rollback` and never called. `skipped` counts as passed. The skip is a recorded result, so a rollback's history shows which gates it bypassed. A gate whose bypass depends on its configuration (`schedule`) is registered without `SkipOnRollback` and returns `skipped` with the same message itself.

## Built-in gates

| Type | Phase | Metrics kind | Rollbacks | Default timeout | Reads |
| --- | --- | --- | --- | --- | --- |
| `checks` | before | `ci` | skipped | `30m` | tracked commits, then GitHub |
| `lock` | before | `lock` | skipped | none | freeze locks |
| `schedule` | before | `schedule` | skipped unless `apply_to_rollbacks: true` | none | the clock |
| `health` | after | `argocd` | runs | `10m` | the Argo CD Application |
| `soak` | after | `soak` | skipped | `duration` + `10m` | the Argo CD Application |

The defaults are `before: [checks]` and `after: [health]`; `lock`, `schedule` and `soak` are opt-in ([configuration.md](configuration.md#gates)). Every gate honours `ctx`, keeps any state it needs across polls in its evidence (timestamps as RFC 3339 UTC), starts fresh when previous evidence cannot be decoded, and never puts credentials in messages or evidence. Strings read from GitHub or Argo CD are cleaned like messages before they are stored as evidence.

### Wiring

The composition root (`serve`) registers all five types with one call and builds the plans from the catalog:

```go
registry := gates.NewRegistry()
err := gates.RegisterBuiltins(registry, gates.Builtins{
	Tracked:      st,                                      // *store.Store
	Live:         intake.LiveChecks{Source: githubChecks}, // optional
	Freezes:      lockService,                             // *locks.Service
	Applications: argoApplications,                        // adapter over the argocd package
})
plans, err := registry.Plans(catalog)
```

The ports are small interfaces in `internal/gates`:

| Port | Method | Satisfied by |
| --- | --- | --- |
| `TrackedChecks` | `TrackedCommits(ctx, repository, sha) ([]core.TrackedCommit, error)` | `*store.Store` |
| `LiveChecks` | `Checks(ctx, repository, sha, required) (core.ChecksEvaluation, error)` | `intake.LiveChecks`: GitHub checks read through intake's `CheckSource` and judged by `intake.Evaluate` |
| `FreezeReader` | `Blocking(ctx, core.Target) ([]core.Lock, error)` | `*locks.Service` |
| `ApplicationReader` | `Application(ctx, namespace, name) (gates.Application, error)` | an adapter over the Argo CD client, written in the composition root |

`gates.Application` carries the Application's sync status, revision, health status, health message and operation phase as the plain strings Argo CD reports; `gates` defines constants for the values it acts on (`SyncSynced`, `HealthHealthy`, `HealthDegraded`, `OperationRunning`, ...). The adapter returns an error wrapping `gates.ErrApplicationNotFound` when the Application does not exist. `Builtins.Now` (default `time.Now`) is the clock for soak, grace and schedule decisions, and `Builtins.DegradedGrace` (default `gates.DefaultDegradedGrace`, one minute) exists for tests; neither is a configuration key. `Live` is the only optional dependency; a missing required one is a startup error.

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

The semantics follow Argo CD sync windows (kind, schedule, duration, time zone), and expressions are parsed by the library Kubernetes CronJobs and Argo CD use (`robfig/cron`), restricted to five fields ([ADR 0008](decisions/0008-built-in-gates-and-deploy-schedules.md)). The parsing and window arithmetic live in `internal/windows`, which `config` also uses, so a configuration that validates cannot fail to build its gates at startup.

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

## Limits

- Messages are cut to 1024 bytes (on a UTF-8 boundary); NUL bytes are removed and invalid UTF-8 is replaced.
- Evidence must be valid JSON of at most 64 KiB and no string in it (key or value) may contain the NUL character `\u0000`, which Postgres `jsonb` cannot store. An escaped backslash followed by `u0000` (`"\\u0000"`) is ordinary text and allowed. Anything else is an evaluation error.
- The limits live in `core` (`core.CleanGateMessage`, `core.ValidateGateEvidence`) and are enforced by both the evaluator and `Store.RecordGateResult`, so any other writer of gate results gets the same cleaning and a validation error instead of a raw Postgres error.
- Evidence and messages are shown in the UI and mirrored to GitHub. Gates must never put secrets, tokens or credentials in them.

## Adding a gate type

A gate implements one method and decides only the verdict; the evaluator stamps the deployment, name, type, phase and timestamps:

```go
type Gate interface {
	Evaluate(ctx context.Context, req gates.Request) (gates.Evaluation, error)
}
```

It must honour `ctx`. A gate type is registered once at startup with the metrics kind it reports as, whether rollbacks skip it, and a factory that builds one instance per configured gate from the typed `config.App` and `config.Gate`:

```go
registry := gates.NewRegistry()
err := registry.Register(config.GateHealth, gates.Definition{
	Kind: metrics.GateKindArgoCD,
	New: func(app config.App, g config.Gate) (gates.Gate, error) { ... },
})
plans, err := registry.Plans(catalog)
```

`Plans` turns every app's `gates.before` and `gates.after` into a `Plan` of `Binding`s (name, type, kind, phase, timeout, skip-on-rollback and the built gate). A gate type with no registered definition, or a factory error, is a startup error naming the app, phase and gate, never a silently passing gate. All errors are reported together. `NewRegistry` returns an empty registry; `RegisterBuiltins` adds the [built-in types](#wiring).

## Observers

Something that must react to gate results, such as mirroring them to GitHub commit statuses, implements `Observer`:

```go
type Observer interface {
	GateRecorded(ctx context.Context, d core.Deployment, result core.GateResult) error
}
```

Observers are called after a result is committed, only when its status changed (including the first `pending` and `skipped`), so a pending gate polled every few seconds does not call the GitHub API on every poll. Observers are best effort: an error is logged at `warn` with the deployment and gate and never changes the verdict. Retrying a failed mirror is the observer's own responsibility.

## Metrics

Evaluations are recorded in `doorstep_gate_evaluation_duration_seconds` ([metrics.md](metrics.md)):

- `gate` is the kind from the gate type's registration (`ci`, `lock`, `schedule`, `argocd`, `soak`, ...), never the configured name;
- `phase` is the deployment kind (`deploy` or `rollback`), not `before`/`after`;
- one `passed` or `failed` observation when a gate reaches that verdict, with the time from `StartedAt` to `FinishedAt` plus the wall time of the call that produced the verdict (`StartedAt` is stamped after the first call returns, so a gate that decides on its first evaluation records the duration of that call, not 0; a timeout counts as `failed`);
- one `error` observation per evaluation error, with the wall time of that call;
- nothing for `pending` polls or `skipped` gates.

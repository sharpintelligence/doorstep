# 0011: Built-in gates and deploy schedules

- Status: Proposed
- Date: 2026-11-24

## Context

The gate framework (registry, evaluator, sticky results, timeouts and rollback skips; see [gates.md](../gates.md)) shipped without gate types. doorstep needs the gates its configuration already names (`checks`, `health`, `soak`) and two that hold deploys over time: manual freezes and recurring windows ("no deploys on Friday evening", "only during business hours").

Several choices here are costly to reverse. The schedule format becomes part of every configuration file. The Argo CD interface decides how the gates meet the `argocd` package being written in parallel. The health and soak policy decides when a deploy is rolled back automatically, and a false failure triggers a rollback of a healthy release. Constraints: gates see only their own previous result, all state must live in gate evidence (no new tables), several replicas may evaluate the same gate, the container image is distroless, and `core` imports nothing.

## Decision

- **Five built-in types**, registered by one `gates.RegisterBuiltins(registry, gates.Builtins{...})` call: `checks` (before, kind `ci`, skipped on rollback, default timeout `30m`), `lock` (before, kind `lock`, skipped on rollback, no timeout), `schedule` (before, new kind `schedule`, rollback bypass configured per gate, no timeout), `health` (after, kind `argocd`, runs on rollbacks, default timeout `10m`) and `soak` (after, kind `soak`, skipped on rollback, default timeout `duration` + `10m`). The default gates stay `before: [checks]`, `after: [health]`.
- **Deploy windows are a `schedule` gate configured inline**: `timezone` (default `UTC`), `apply_to_rollbacks` (default `false`) and `windows`, each `{effect: allow|deny, cron, duration, timezone, name}`. There is no top-level catalog of named schedules; YAML anchors share a window list between apps.
- **Semantics mirror Argo CD sync windows.** A window is active at *now* when a firing *t* satisfies *t* ≤ *now* < *t* + `duration`. Any active deny window holds the deploy; otherwise, if the gate has allow windows, one must be active. Holding is `pending`, never `failed`, so the deployment waits at the head of its queue. A pass is sticky, so windows stop the start of a deploy, not a deploy in flight.
- **Rollbacks pass schedules by default.** The schedule gate is registered without `SkipOnRollback` and returns `skipped for rollback` itself unless `apply_to_rollbacks: true`, so the registry and evaluator stay unchanged.
- **Cron parsing uses `github.com/robfig/cron/v3`**, parser only, five fields, no descriptors (`@daily`, `@every`), and `TZ=`/`CRON_TZ=` prefixes rejected so the zone comes only from configuration. It lives behind a leaf package, `internal/windows`, used by `config` for validation and by `gates` for evaluation. Time zones are IANA names; `Local` is rejected; `time/tzdata` is embedded in the binary.
- **Gates own their ports.** `TrackedChecks` (`*store.Store`), optional `LiveChecks` (`intake.LiveChecks`, composing intake's check source and evaluation rules), `FreezeReader` (`*locks.Service`) and `ApplicationReader`, which returns a gates-owned `Application` of plain strings (sync status, revision, health, health message, operation phase) and `ErrApplicationNotFound`. The composition root adapts the `argocd` package to it.
- **Health policy.** Passed when `Synced`, `Healthy` and no operation `Running`. Failed only when `Degraded` is observed one minute or more after it was first seen (`degraded_since` in evidence), with `degraded_since` carried through every state that is not synced and healthy and cleared only by a synced and healthy observation, so a workload flapping between `Degraded` and `Progressing` fails after the grace rather than at the timeout. Everything else is pending until the timeout. The Application's revision is not compared with the pin commit; that the pin is synced is the engine's syncing step.
- **Soak policy.** The soak gate reads the Application itself. Its clock starts at the first synced-and-healthy observation (`healthy_since`), is not reset by `Progressing` or `OutOfSync`, is reset by any `Degraded` observation, and is carried only from a `pending` previous result. It passes once `duration` has elapsed and the Application is healthy at that moment, and fails on the same one minute `Degraded` grace.
- **One injected clock.** Gates take `Now` from `Builtins` and keep timestamps in evidence as RFC 3339 UTC.

## Options considered

### Schedule as its own gate type with inline windows (chosen)

Keeps the whole definition in `config.Gate`, which is all a gate factory receives, and follows the existing catalog pattern of settings validated per type. Argo CD users already know the window model. The cost is repetition across apps, which YAML anchors cover; a named catalog can be added later without breaking inline windows.

### Fold windows into the `lock` gate or freeze locks

One concept for "deploys are held", but a manual freeze has an owner, a reason and an audit trail, while a window is configuration. Mixing them would blur both the metrics series and the API, and a window would need a synthetic owner.

### A top-level catalog of named schedules referenced by gates

Less repetition for many apps, but more configuration surface and cross-references to validate, and the factory signature would need the catalog. Deferred until there is demand.

### Hand-rolled cron parsing

No dependency, but a parser plus next-firing search is a few hundred lines of daylight saving edge cases. `robfig/cron` has no dependencies and is what Kubernetes CronJobs and Argo CD use, so expressions behave as users expect. It has no previous-firing function, but activity needs only one call: a window is active when the first firing after *now* − `duration` is not after *now*.

### Skip schedules on rollback through `SkipOnRollback`

`SkipOnRollback` is per type, so a per-gate opt-in would need a change to `Definition` and `Binding`, which parallel work also touches. Letting the gate return `skipped` itself uses what the evaluator already accepts.

### Use core types for Argo CD state

`core.Health` and `core.SyncStatus` do not exist yet and the `argocd` package is being written in parallel. Guessing their shape risks a merge collision; plain strings are what the Application status holds anyway, and the adapter is a few lines.

### Fail health on the first `Degraded`, or compare revisions

Failing at once rolls back a fix deployed to an already degraded app, because Argo CD briefly reports the old state after a sync. A revision comparison is wrong in a GitOps repository shared by several apps, where another app's pin moves the Application's revision.

### Soak keyed off the health gate's result, or reset on any non-healthy state

Gates cannot see other gates' rows, and reading Argo CD directly lets soak catch a delayed regression, which is its purpose. Resetting on every `Progressing` blip would never finish soaking an autoscaling app. `Degraded` is different: a soak that passed across a degraded stretch would certify a release that was not healthy for `duration`, so `Degraded` does reset the clock.

## Consequences

- `serve` needs one call and four dependencies to enable every built-in gate; the engine bead writes the Argo CD adapter.
- A `schedule` gate can keep a before phase pending for days. The engine should release a claimed deployment with a delay while its before phase is pending, rather than hold a lease.
- `lock` is opt-in, so existing configurations keep their gate rows and GitHub statuses unchanged. The queue claim remains the enforcement point for freezes ([ADR 0004](0004-freeze-locks.md)).
- `soak` now times out by default (`duration` + `10m`), so an Application that never becomes healthy fails the deploy instead of holding the target forever.
- The binary is about 450 KB larger for the embedded time zone database.
- A health regression shorter than one minute is tolerated, and an Application degraded for longer fails even while Argo CD is still converging. Intervening `Progressing` or `OutOfSync` observations do not extend the grace; only a synced and healthy observation ends it.
- Cron firings in a daylight saving gap are skipped and those in an overlap fire twice, as in `robfig/cron`, Kubernetes CronJobs and Argo CD; this is documented rather than corrected.
- Deploy windows, health grace and soak state survive restarts and replica changes because they live in evidence and configuration only; no migration is needed.

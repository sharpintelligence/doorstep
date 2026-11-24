# Gates

A gate is a check that decides whether a deployment may continue. Gates run in two phases:

- **before** gates must pass before a deploy starts (the pin is written). The default is `checks`: CI succeeded on the commit.
- **after** gates must pass after Argo CD has synced the pin for the deployment to count as successful. The default is `health`: the Argo CD Application is synced to the pin commit and healthy.

Which gates an app runs, their names and their timeouts come from the configuration file ([configuration.md](configuration.md#gates)). Gate results are recorded data, not control flow ([ADR 0001](decisions/0001-go-and-core-design.md), decision 4): every evaluation leaves a row explaining the verdict.

The code lives in `internal/gates`. Gate implementations (`checks`, `health`, `soak`) are added by later work; this package provides the data model, the registry and the evaluator they plug into.

## Data model

A gate result is the latest verdict of one named gate for one deployment, stored in `gate_results` ([database.md](database.md#gate_results)) and returned as `core.GateResult`:

| Field | Meaning |
| --- | --- |
| `DeploymentID` | the deployment; its commit is the deployment's commit, so a result is effectively *(commit, gate name, phase, details)*, and a rollback of the same commit gets its own results |
| `Name` | gate name from the configuration, unique within the app across both phases |
| `Type` | gate type from the configuration (`checks`, `health`, `soak`, ...) |
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

A gate's `timeout` from the configuration (default `30m` for `checks`, `10m` for `health`) is enforced by the evaluator for every gate type. When a gate is still `pending` and its `UpdatedAt - StartedAt` has reached the timeout, it is recorded as `failed` with the message `timed out after <timeout>`, keeping the gate's last evidence. A gate without a timeout (`soak`) never times out. Because both timestamps are database readings, the timeout is measured on the clock all replicas share and survives restarts.

### Errors

A gate that cannot reach a verdict (it returns an error, panics, returns an unknown status, invalid or oversized evidence, or gives no answer within 30 seconds) is treated as a transient problem. It is recorded as `pending` with the message `evaluation error: <reason>` and keeps its previous evidence, the error is logged at `warn` and counted in metrics, and the phase stays `pending` until the gate recovers or its timeout fails it. A Prometheus or GitHub blip therefore never fails a deploy by itself, but it is visible, and it cannot hold a deploy forever.

A gate call that gives no answer within 30 seconds is abandoned: the evaluator stops waiting and logs `gate call abandoned while still running` at `warn`. A gate that ignores `ctx` keeps running in the background, so while an abandoned call of a gate for a deployment has not returned, later passes do not call that gate for that deployment again; they record the evaluation error `previous call has not returned yet` instead. A stuck gate therefore costs at most one goroutine per deployment, and its timeout still fails it.

`Evaluate` itself returns an error only when the result store fails, the app has no gate plan (`ErrNoPlan`), the phase is invalid (`ErrInvalidPhase`) or `ctx` is cancelled. Nothing is recorded for a gate whose call was cut short by cancellation.

### Rollbacks

A rollback re-pins a digest that already passed CI and soak, but it still waits for Argo CD ([ADR 0001](decisions/0001-go-and-core-design.md), decision 5). Gate types are registered with `SkipOnRollback`; for a deployment of kind `rollback` those gates are recorded as `skipped` with the message `skipped for rollback` and never called. `skipped` counts as passed. The skip is a recorded result, so a rollback's history shows which gates it bypassed.

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

`Plans` turns every app's `gates.before` and `gates.after` into a `Plan` of `Binding`s (name, type, kind, phase, timeout, skip-on-rollback and the built gate). A gate type with no registered definition, or a factory error, is a startup error naming the app, phase and gate, never a silently passing gate. All errors are reported together. The registry ships empty; nothing is wired into `serve` yet.

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

- `gate` is the kind from the gate type's registration (`ci`, `argocd`, `soak`, ...), never the configured name;
- `phase` is the deployment kind (`deploy` or `rollback`), not `before`/`after`;
- one `passed` or `failed` observation when a gate reaches that verdict, with the time from `StartedAt` to `FinishedAt` plus the wall time of the call that produced the verdict (`StartedAt` is stamped after the first call returns, so a gate that decides on its first evaluation records the duration of that call, not 0; a timeout counts as `failed`);
- one `error` observation per evaluation error, with the wall time of that call;
- nothing for `pending` polls or `skipped` gates.

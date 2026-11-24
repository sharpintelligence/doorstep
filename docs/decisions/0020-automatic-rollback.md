# 0020: Automatic rollback

- Status: Accepted
- Date: 2026-11-24

## Context

[ADR 0017](0017-retry-before-rollback.md) made a failed deploy's `failed` event carry a durable verdict, `decision` and `rollback_eligible`, so that automatic rollback could act on it without parsing text. Operators of small clusters want a deploy that fails its after-deploy gates, or whose sync keeps failing because of the version, to be put back on the last good digest without waking someone up, while a release with database migrations must never be rolled back unattended ([ADR 0015](0015-migration-aware-releases.md)). The engine imports only `core` and the standard library and owns the failed event's schema; the rollback decision needs the app's row lock and `rollback.Repository`.

## Decision

1. Automatic rollback is opt-in per app with `auto_rollback: true`, off by default. There are no other knobs.
2. It triggers on exactly the failures whose event has `rollback_eligible: true`: a deploy that failed in `syncing` or `verifying` with a pin, whose decision is `version_caused` or `retries_exhausted`, or an after-gate failure of class `version`. Rollbacks are never eligible, which is the loop guard.
3. The decision lives in the engine, not in the failure policy. `retry.Policy` stays pure judgement; there is no new `FailureAction`. In `failFinal`, an eligible failure of an app the optional `AutoRollbacker` port reports as enabled goes to `FailAndRollBack` instead of a plain transition.
4. Failing the deploy, queuing the rollback, cancelling queued deploys of the blocked commit, creating the block and auditing are one transaction. `(*rollback.Service).Automatic` runs it under the app's row lock and calls back into the engine (`Fail func(RollbackPlan) (Transition, error)`) to build the failed transition once the outcome and the rollback's id are known. So the event schema and reason text stay in `engine`, and the decision that needs the lock stays in `rollback`. A compare-and-set failure on the deploy undoes everything.
5. The target is `LastSuccessfulDeployment` (newest succeeded, unblocked, with a digest). No target: outcome `no_target`. Target digest equal to the failing digest: outcome `same_version`, nothing rolled back or blocked, because the premise that the new version caused the failure is false. Unlike a manual rollback, which excludes the current digest and goes back to the previous distinct version, the automatic path passes no exclusion: going further back would block the last version known to work without a person deciding (see Options considered). A queued or running rollback: `rollback_in_progress`.
6. When `MigrationsBetween(target, to, failing)` returns anything, nothing is queued or blocked (`withheld_migrations`), and the failure reason keeps the original cause first and ends with `; automatic rollback withheld: release contains database migrations; roll back with --confirm-migration-rollback`.
7. A newer queued deploy does not suppress the rollback; it runs afterwards, as after a manual rollback.
8. No new notification kind, audit action or metric. `deploy.failed` carries the outcome in its message, followed by `deploy.queued` for the rollback; the audit row is `rollback.request` by `system:doorstep` with `"automatic": true` and `"trigger"`; `doorstep_rollbacks_total` counts `gate_failed` and `sync_failed`, from `rollback.Service` like manual rollbacks.
9. The failed event gains an optional `auto_rollback` object (`outcome`, `rollback_id`, `to_deployment_id`, `digest`, `migration_deployment_ids`). No store migration is needed.

## Options considered

- **A failure policy action** (`FailureActionRollBack` returned by `retry.Policy`). Rejected: the policy would need the app's auto-rollback setting, mixing configuration with classification, and the engine would have to re-validate the action against its own eligibility rule anyway.
- **Two steps: fail, then enqueue** through separate ports. Rejected: a crash or database error between them leaves a failed deploy with no rollback, or a rollback with a still-running deploy, and history would show an eligible failure that was silently not acted on.
- **A rollback port owned by the engine with its own store code.** Rejected: `rollback.Repository` already has every method the transaction needs (`LockTarget`, `LastSuccessfulDeployment`, `MigrationsBetween`, `EnqueueDeployment`, `TransitionDeployment`, `CreateBlock`, `RecordAudit`), and the manual path's lock order (app row, then deployments) is reused, so the two cannot deadlock.
- **Skipping to an older, different digest when the last success has the failing digest**, as a manual rollback does. Rejected for the unattended path: it would block the last known good version without a person deciding.
- **A list of triggers** (`auto_rollback: [gate_failed]`) so an operator can opt into after-gate failures only. Deferred: a sync failure is eligible only when the version was judged the cause or the retry budget is spent, which is the same evidence a failed gate gives, and one boolean keeps the first version simple. The flag can later accept a list without breaking `true`.
- **Holding the rollback when a newer deploy is queued.** Rejected: that deploy may wait behind `before` gates for hours while a version that failed its gates stays live.
- **A `deploy.rollback_started` notification kind.** Rejected: it touches core kinds, configuration validation, every formatter and the UI, and ADR 0017 already rejected parallel decision kinds; the existing sequence says the same.

## Consequences

- One rule covers both rollback paths: the same block, cancellation and audit shape, and the same queue priority.
- The engine port is small and its fake is trivial: it calls `Fail` with a scripted plan.
- The eligibility predicate decides both the flag and the action, so history shows an eligible failure without an `auto_rollback` outcome for an opted-in app only in the rejected-request case below.
- A deploy whose rollback was withheld, had no target or was the same version stays failed and pinned until a person acts; the notification says why.
- A transient database error during the transaction returns the step's error; the runner retries the claim and reaches the same decision, so no partial state appears.
- A compare-and-set conflict on the failing deploy is final (someone else moved it) and is not retried; a conflict while cancelling a queued duplicate is retried like the manual path.
- A request the rollback service rejects (`core.ErrInvalidRollback`, a programming error) is logged and the deploy fails without an automatic rollback instead of being re-run forever. That is the only case where an eligible failure of an opted-in app has no `auto_rollback` outcome.

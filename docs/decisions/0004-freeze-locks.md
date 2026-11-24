# 0004: Freeze locks stop new deploys, not recovery

- Status: Proposed
- Date: 2026-11-23

## Context

Operators need to stop deploys for a while: during a database migration, an incident or a holiday freeze, for one app or for everything. The bead asks that a lock have an owner, a reason and an optional expiry, and that locked apps keep queueing but not deploy.

"Not deploy" needs a precise boundary. A deployment passes through `resolving`, `pinning`, `syncing` and `verifying`, and a rollback is its own deployment of kind `rollback` queued at a higher priority ([ADR 0002](0002-rollback-is-a-new-deployment.md)). A freeze could stop every claim of a locked app, or only some. It also needs a global form, an expiry that works with several replicas, and an audit trail that outlives the lock row. The `locks` table from migration 00001 already has `owner`, `reason`, `expires_at` and `UNIQUE (app_id, kind)`, but its `app_id` is required.

## Decision

- A live freeze stops only the **start** of an ordinary deploy: the claim skips a queue entry whose deployment is `queued` and of kind `deploy` while an app freeze or the global freeze covers its app. Deployments in an active state stay claimable and finish. Deployments of kind `rollback` are never blocked. Intake is untouched, so locked apps keep queueing.
- The freeze is one more condition in the claim statement, and the post-claim re-check (a fresh snapshot while the app row is locked) also rejects a claim that a freeze committed a moment earlier now covers. Taking an app freeze locks the app row first, so an app freeze and a claim of that app are strictly ordered. A global freeze takes an exclusive transaction-level advisory lock that every claim takes in shared mode, so it waits for claims in progress (which count as started before it) and claims that start meanwhile wait for it, without claims serialising against each other. Once a lock request has succeeded, no deploy it covers starts.
- A global freeze is a `locks` row with a null `app_id` and kind `freeze`. A partial unique index allows one, and a check constraint keeps deployment locks per app. No sentinel app row is needed.
- Expiry is evaluated against the database clock everywhere. An expired row is treated as absent at once and deleted lazily, with an `expired` notification, by the next lock of its scope or by a once-a-minute sweep.
- Locking a held scope is a conflict that returns the holder; nothing is overwritten. Any authenticated caller may unlock any freeze, and the release is recorded with their identity. The owner is always the authenticated caller.
- Reminders for long-held locks are claimed with `UPDATE ... SET reminded_at = now() ... RETURNING`, so each is sent by one replica without leader election.
- Every lock, unlock, expiry and reminder is written to an append-only `lock_events` table in the same transaction.

## Options considered

### Block the start of ordinary deploys only (chosen)

Matches what a freeze is for: keep production as it is. A half-finished deployment is left to complete, because stopping it between pinning and syncing would leave Git and the cluster in an intermediate state, and its lease would expire and be re-claimed forever. Rollbacks keep their head-of-line priority, so a bad version can still be reverted during a freeze. Tools such as Shipit and GitHub's deploy locks behave the same way: locks stop new deploys, not recovery. The cost is that "frozen" does not mean "nothing changes": an operator who wants nothing at all to move must also wait for the in-flight deployment. Neither the lock API nor `doorstep locks` shows in-flight deployments today; the deployment history and the web UI, which come later, are where they become visible.

### Block every claim of a locked app

Simple to state, but it strands in-flight deployments mid-step and blocks rollbacks, which turns a freeze taken during an incident into a way to keep the incident going.

### Block rollbacks too, with an override

Adds a second kind of lock or a force flag for a case that is almost never wanted, and makes the safety path depend on someone remembering the override.

### Global lock as a sentinel app row, or a separate table

A sentinel app would leak into app listings, history and metrics. A separate table would duplicate the columns and give the claim two places to look. A nullable `app_id` with a partial unique index is the smallest change and keeps one query path.

## Consequences

- Migration 00003 makes `locks.app_id` nullable and adds `reminded_at`, `locks_global_check`, `locks_expires_at_check`, `locks_global_kind_key` and the `lock_events` table.
- Correctness of expiry does not depend on the sweep running; the sweep only cleans up and notifies.
- The engine does not enforce freezes; it reads `locks.Service.Blocking` to show why a deploy is waiting.
- A reminder can be lost if a replica dies between marking and sending it; the next one follows one interval later.
- Deployment-kind lock rows and their semantics are left to the engine work.
- This decision covers the API and the CLI. Lock and unlock controls in the web UI are left to the UI work, and the endpoints answer `401` until API tokens exist, so the feature cannot be used against a running server before both land.

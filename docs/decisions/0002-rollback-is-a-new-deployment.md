# 0002: A rollback is a new deployment

- Status: Accepted
- Date: 2026-11-23

## Context

[ADR 0001](0001-go-and-core-design.md), decision 5, says a rollback is a forward commit that re-pins the previously deployed digest and travels the same queue, lock and gate path as any deploy, ahead of waiting deploys. The first architecture document drew the rollback as states of the failing deployment instead: `failed → rolling_back → rolled_back | rollback_failed`. The two readings lead to different schemas, queue rules and engine code, and the store bead (doorstep-ehl.2.2) is about to fix the state list in migration 00001. The choice has to be made before the engine and rollback work starts, while changing it costs only an edit to an unmerged migration.

## Decision

A rollback is its own deployment row, not a phase of the deployment it reverts.

- It has `kind = rollback`, `rollback_of_id` pointing at the deployment it reverts, and an image that names the previous digest. It is queued with a higher priority than deploys, so it is the next head of line for its target.
- It moves through the same states as a deploy: `queued → resolving → pinning → syncing → verifying → succeeded`, or `failed` or `cancelled` on the way. Its own outcome is the outcome of the rollback. The engine skips the CI and soak gates for it, as ADR 0001 says, but still waits for Argo CD to report synced and healthy.
- The deployment being reverted ends in an ordinary terminal state. A deploy whose gates fail after pinning ends `failed`; the engine moves it to `failed`, deletes its queue entry and enqueues the rollback in one transaction. A deploy that is still in flight when an operator asks for a rollback is `cancelled`, as ADR 0001 says, and the rollback takes over its lock.
- There are no `rolling_back`, `rolled_back` or `rollback_failed` states. Every terminal state has no outgoing transitions. "Was this deployment rolled back, and did that work" is a read: the latest deployment whose `rollback_of_id` names it, and that deployment's state.
- A rollback that fails is not rolled back automatically. The engine records it and notifies; an operator decides what happens next.

## Options considered

### A new deployment (chosen)

One state machine, one queue path and one set of gates for deploys and rollbacks, which is what ADR 0001 asks for ("deploy and rollback share one code path"). Priority and head of line in the queue already express "a rollback goes first". History shows the rollback as its own row with its own events, gate results, pin commit and timestamps. The cost is that the link between a failure and its rollback is a foreign key rather than a state, so read models join on `rollback_of_id`.

### Rollback states on the failing deployment

`failed → rolling_back → rolled_back | rollback_failed` on the same row keeps a deploy and its rollback in one record. But the rollback would need a second copy of the pin, sync and verify steps as `rolling_back` sub-steps, or overload one state for all of them, so it could not share the deploy path. One row would carry two pin commits, two digests and two sets of gate results. `failed` would stop being terminal. And a rollback requested while a deploy is in flight, or of an older succeeded deployment, has no failed row to hang the states on.

## Consequences

- `core.State` and the `deployments` state check list nine states: `queued`, `resolving`, `pinning`, `syncing`, `verifying`, `succeeded`, `failed`, `cancelled`, `superseded`. The active states are `resolving`, `pinning`, `syncing` and `verifying`.
- The queue claims only deployments in `queued` or an active state, so a queue entry left behind by a terminal deployment is never handed out again.
- The UI, API and notifications show a rollback as a deployment of kind `rollback` linked to the one it reverts.
- Retrying a rollback is a new rollback deployment with `parent_id` set, like any retry.

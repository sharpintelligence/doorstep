# 0003: Queue batching and urgent deploys

- Status: Accepted
- Date: 2026-11-23

## Context

Deploys of one app run one at a time. A burst of merges should not turn into a burst of deploys that each wait for Argo CD and the soak gate, so several queued commits need to collapse into one deploy, but a single deploy should not carry an unbounded change set. Operators also need a way to ship a hotfix ahead of routine work. The engine, intake and UI all build on how these rules behave, so they are fixed here, before those are written.

## Decision

1. **Batching happens at claim time.** When a worker claims a `queued` deploy, it takes the first `max_batch` queued deploys of that app at the head's priority, ordered by deployment id. The newest of them runs; the rest become `superseded` (with `superseded_by_id`, `finished_at`, a transition event, and their queue entries deleted) in the same transaction. `max_batch` is per app, default 10, 1 to 100; 1 disables batching. Enqueue order is commit order.
2. **Urgent is a priority.** Priorities are `normal` (0), `urgent` (100) and `rollback` (200). When an urgent deploy is claimed it also supersedes every queued lower-priority deploy of the app with a lower deployment id, not bounded by `max_batch`. Newer deploys stay queued.
3. **One rule for both:** batch within the head's priority, then supersede older deploys of lower priority. Rollbacks never batch, are never superseded and never supersede.
4. **No preemption.** A deployment in an active state always outranks queued entries of its app, even after its lease expired. Urgent means next, not now.
5. **Enqueue is idempotent per app and commit.** A second request for a commit that already has a queued or in-flight deploy returns that deploy; an urgent request raises a queued one to urgent.

## Options considered

### Batch at enqueue

Superseding older entries as each new one arrives keeps the queue short, but the bound would mean nothing: with five queued and a limit of three there is no moment to decide that `c3` and then `c5` run. It would need extra state to remember window boundaries. Claim time is the only point where "the next deploy" is defined. Rejected.

### Urgent bounded by `max_batch`

Treating urgent exactly like batching keeps one bound, but a hotfix `c5` that jumped `c1`…`c4` with a limit of one would be followed by a deploy of `c4`, reverting the hotfix. The bound exists to keep routine change sets reviewable; an urgent deploy is an explicit operator override that already ships everything up to its commit. Rejected.

### Urgent preempts in-flight work

Cancelling an in-flight deploy for an urgent one would get a hotfix out a few minutes sooner, but the cancelled deploy may already have pinned, leaving git and the cluster mid-change, and the one-active-deployment index would reject the new one until the old one is terminal. Rejected; rollbacks, which do need to take over, are handled by the engine making the failing deployment terminal first ([ADR 0002](0002-rollback-is-a-new-deployment.md)).

### Ancestry-aware batching

Asking GitHub whether one commit contains another would make batching exact when merges arrive out of order. The queue has no GitHub access and the extra round trip sits on the claim path. Enqueue order is close enough for merges to one branch, which is what intake produces. Rejected for now.

## Consequences

- A merge burst produces at most one deploy per `max_batch` commits, and the newest commit always deploys eventually.
- Superseded deploys are terminal and visible in history with the deployment that replaced them; notifications can name them from the claim result.
- An urgent deploy ships everything older than it, which is what "deploy this commit now" means on a linear branch. Operators who need to skip work in between should cancel it.
- The claim transaction does more work when it batches (a window select and a supersede update), all on rows of one app that it already holds locked.
- Out-of-order enqueues (an older commit enqueued after a newer one) are treated as newer. Intake should enqueue in merge order.

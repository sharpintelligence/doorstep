# Deploy queue

Every deploy and rollback waits in a durable, per-app queue in Postgres. Deploys of one app run one at a time, in order; several merged commits can be collapsed into one deploy; an urgent deploy can jump ahead of routine ones; and the queue is visible through the HTTP API. This document describes the behaviour operators see and the contract the engine builds on. The decision record is [ADR 0003](decisions/0003-queue-batching-and-urgent.md); the SQL and schema are in [database.md](database.md#claiming-work).

## Packages

| Package | Role |
| --- | --- |
| `internal/store` | all SQL: `EnqueueDeployment`, `ExpediteDeployment`, `ClaimBatch` (and the non-batching `Claim`), `HeartbeatQueueEntry`, `ReleaseQueueEntry`, `CompleteQueueEntry`, `QueueEntries`, `QueueDepth`, `ListenQueue` |
| `internal/queue` | the adapter the engine consumes: priorities, lease length, per-app batch limit (`Queue`), and the worker loop (`Runner`) |
| `internal/api` | `GET /api/v1/apps/{app}/queue`; see [api.md](api.md) |

## Priorities

| Priority | Value | Used for |
| --- | --- | --- |
| `normal` | 0 | deploys from merges, polls, the API and the CLI |
| `urgent` | 100 | deploys an operator marks urgent (a hotfix) |
| `rollback` | 200 | rollback deployments ([ADR 0002](decisions/0002-rollback-is-a-new-deployment.md)) |

`queue.PriorityFor(kind, urgent)` maps a request to its priority: rollbacks are always `rollback`, whatever was asked.

## Order

Each app's queue is ordered by:

1. **In flight first.** A deployment that has started (`resolving`, `pinning`, `syncing`, `verifying`) always runs before anything queued behind it, even after its worker crashed and its lease expired. Jumping the queue means running *next*, never preempting.
2. **Priority**, highest first.
3. **Deployment id**, oldest first. Enqueue order stands in for commit order; the queue does not ask GitHub about ancestry.

Where batching needs to know which of two commits is newer, it uses the order in which each commit was **first seen**: the lowest deployment id of the app with that SHA. A redeploy of an older commit therefore keeps the age of its first deployment and is never treated as newer than commits that arrived after it. A commit doorstep has never seen before counts as the newest, and commits that arrive out of order (a webhook for `c2` delivered before the one for `c1`) are ordered as they arrived.

Only the head of an app's queue can be claimed, and nothing of an app is claimed while another of its entries holds a live lease. Different apps run in parallel.

## Batching

When a worker claims a `queued` deploy, the queue looks at the deploys waiting behind it at the same priority and takes the first `deploy.max_batch` of them (the head included, ordered by id). The one with the newest commit in that window runs; the older ones become `superseded`, with `superseded_by_id` naming the winner. All of this happens inside the claim transaction, so there is never a moment where a head is leased and then superseded.

With `max_batch: 3` and five queued commits `c1`…`c5`, the first claim deploys `c3` and supersedes `c1` and `c2`; the next deploys `c5` and supersedes `c4`. `max_batch: 1` turns batching within a priority off: every normal commit is deployed. It does not turn off urgent superseding (below), which is not bounded by `max_batch`. The limit is per app (`apps[].deploy.max_batch`, default 10, 1 to 100; see [configuration.md](configuration.md#apps)). Apps that are no longer in the configuration file are handled with a limit of 1.

A superseded deployment gets `finished_at`, its queue entry is deleted, and a `transition` event from `queued` to `superseded` is recorded with actor `doorstep` and the message `superseded by deployment <id>`. The claim result lists the superseded ids so the engine can notify about them.

Batching never applies to rollbacks, and never to a resumed in-flight deployment.

## Urgent deploys

An urgent deploy outranks every normal deploy of its app. When it is claimed it also supersedes **every** queued normal deploy of that app with a lower deployment id whose commit was first seen before the urgent commit, regardless of `max_batch`. Without that, a hotfix `c5` that jumped `c1`…`c4` would be followed by a deploy of `c4` that silently reverts it. Normal deploys enqueued after the urgent one stay queued and run after it.

An urgent redeploy of an older commit supersedes nothing newer than that commit: with `c9` and `c10` queued, an urgent redeploy of the already deployed `c8` runs first and `c9` and `c10` stay queued, so the newest commit still deploys afterwards. Several urgent deploys batch among themselves exactly like normal ones.

The general rule is one sentence: batch within the head's priority, then supersede older deploys of lower priority. Rollbacks are never superseded and never supersede anything.

An urgent deploy does not interrupt a deployment that is already in flight; it runs as soon as that one finishes.

## Enqueueing and deduplication

`Queue.Enqueue(ctx, deployment, EnqueueOptions{Urgent})` returns the deployment and whether it was created. For kind `deploy` it is idempotent per app and commit: if a deploy of the same commit SHA for the same app is still queued or in flight, that deployment is returned with `Created: false`. Webhook redeliveries and a poll racing a webhook therefore never queue a commit twice. A finished, cancelled or superseded deploy of the commit does not count, so redeploying an old commit works.

If the request is urgent and the existing deploy is still `queued` at a lower priority, it is raised to `urgent` and a `note` event (`priority raised to urgent`) is recorded with the requester as actor. `Queue.Expedite(ctx, id, actor)` does the same for a known deployment id and returns `core.ErrNotFound` unless the id is a queued deploy. Rollbacks are never deduplicated.

Enqueue serialises on the app's row (`SELECT ... FOR NO KEY UPDATE`), so concurrent requests for the same commit create one deployment.

A rollback request ([rollback.md](rollback.md)) queues its rollback at `rollback` priority and, in the same transaction, cancels the in-flight deploy it replaces and every queued deploy of the commit it blocks. Other queued deploys stay and run after the rollback. The enqueue itself never refuses a blocked version: intake refuses blocked commits before enqueueing, and the engine refuses blocked digests after resolving.

## Leases

A claim leases the entry to a worker for `queue.Options.Lease` (default 60 s), measured on the database clock.

| Call | Effect | Fails with `core.ErrLeaseLost` when |
| --- | --- | --- |
| `Heartbeat(ctx, id, worker)` | extends the lease to now + lease | the worker does not own the entry or the lease has already expired |
| `Release(ctx, id, worker, delay)` | clears the lease; the entry is claimable again after `delay` | the worker does not own the entry |
| `Complete(ctx, id, worker)` | deletes the entry | the worker does not own the entry |

An expired lease is never revived: by then another entry of the app (a rollback, say) may have been claimed, and reviving the old lease would give the app two live leases. A worker whose heartbeat reports `ErrLeaseLost` must stop. An expired lease makes the entry claimable again, with its attempt count incremented, so a crashed replica never wedges an app.

Every transition of a deployment to a terminal state deletes its queue entry in the same transaction (see [database.md](database.md#queue_entries)); `Complete` is the queue-side form of that for callers that only need to drop the entry.

## Wake-ups

A row trigger on `queue_entries` calls `pg_notify('doorstep_queue', <schema>)` when an entry is inserted or deleted, and when an update clears a lease or changes `priority` or `available_at`. Claims and heartbeats do not notify, so busy replicas do not wake each other every few seconds. Every writer, including future engine transitions and rollback enqueues, produces wake-ups without remembering to.

`Store.ListenQueue` (and `Queue.Listen`) holds one dedicated connection outside the pool, ignores notifications whose payload is another schema, and calls `wake` for each relevant one. When the connection drops it reconnects with backoff (1 s doubling to 30 s) and wakes once after every successful `LISTEN`, because notifications sent while it was away are lost. Notifications are only a latency hint: the runner also polls, so a missed notification costs at most one poll interval, and doorstep works behind a PgBouncer in transaction mode (where `LISTEN` does not work) with polling alone.

## Runner

`queue.Runner` is the worker loop. `serve` does not start it yet; the engine bead supplies the handler and starts it.

```go
q := queue.New(queue.Options{Store: st, MaxBatch: apps.MaxBatch, Logger: logger})
workerID, err := queue.NewWorkerID()
runner := queue.NewRunner(queue.RunnerOptions{
	Source:            q,
	Listen:            q.Listen,
	WorkerID:          workerID,
	Logger:            logger,
})
err = runner.Run(ctx, func(ctx context.Context, c queue.Claimed) (queue.Result, error) {
	// one engine step for c.Deployment
	return queue.Result{}, nil
})
```

- It claims until nothing is claimable or `Concurrency` (default 4) handlers are running, then waits for a wake-up or the poll tick (`PollInterval`, default 5 s). A finished handler also wakes the loop.
- Each claim gets a heartbeat goroutine at `HeartbeatInterval`, which defaults to a third of `Source.Lease()`; `Run` refuses to start when the interval is not shorter than the lease. On `ErrLeaseLost` it cancels the handler's context with `core.ErrLeaseLost` as the cause, and the runner neither releases nor retries the entry. Other heartbeat errors are logged and retried on the next tick.
- When the handler returns, the entry is released with `Result.RequeueAfter` as the delay. If the handler returned an error (or panicked) the delay is `Backoff(failures)` instead, where `failures` counts consecutive failures of that deployment in this process (default 5 s doubling to 5 minutes). A deployment that has not failed here for an hour is forgotten, so deployments finished by another replica do not accumulate. A release that reports `ErrLeaseLost` means the handler finished the deployment and deleted its entry; that is the normal end. Releasing an entry whose deployment is already terminal deletes the entry, so a handler that forgets to delete it does not leak a row.
- On shutdown the handler's context is cancelled and the entry is released with no delay, through a detached context bounded to 5 s, so another replica can take over at once instead of waiting for the lease to expire. `Run` returns nil after every handler has returned.
- `NewWorkerID` returns the hostname plus 8 random hex characters.

The handler contract for the engine: do one idempotent step (or the whole deployment) under the given context; when the deployment reaches a terminal state, delete its queue entry in the same transaction as the transition; otherwise return and let the runner release the entry so the next step is claimed.

## Observability

- `doorstep_queue_depth` reports entries whose deployment is `queued` and not leased, across all apps ([metrics.md](metrics.md)).
- `GET /api/v1/apps/{app}/queue`, needs the viewer role and shows an app's entries in run order ([api.md](api.md)).
- Superseded deployments and priority changes are in the deployment events.

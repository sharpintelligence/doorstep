# 0006: Notifications are best effort

- Status: Accepted
- Date: 2026-11-23

## Context

doorstep notifies Slack channels and generic webhooks about deploys, rollbacks and locks, and mirrors deployments as GitHub deployment statuses. These targets are outside doorstep's control: a Slack outage, a slow home-automation webhook or a GitHub App that lacks the *Deployments* permission must never hold up or fail a deploy. The deployment history in Postgres already records every transition durably. The question is how strongly notification delivery itself should be guaranteed, which decides whether it needs a schema change, a claimer and deduplication, and what the engine's `Notifier` port looks like.

## Decision

Notification delivery is in memory and best effort.

- The engine calls `Notify(ctx, core.Notification)`, which has no error result. `notify.Dispatcher` validates the notification, routes it and puts it on a queue without network I/O, then returns.
- Each target has its own bounded queue (64) and one worker, which keeps events in order per target and isolates a slow target from the others. A full queue drops the new notification, logs a warning and counts it.
- Each delivery is retried a bounded number of times (4 attempts, 10 seconds each, 1/2/4 second backoff with jitter, `Retry-After` up to 30 seconds), so one delivery takes about a minute at most and queues keep moving.
- On shutdown the dispatcher stops intake and drains until the shutdown timeout, then drops what is left.
- Outcomes are logged and counted in `doorstep_notifications_total{type, result}` so that lost notifications can be alerted on.
- There is no notification table and no migration.

## Options considered

### In memory, best effort (chosen)

No schema change, no claimer, no deduplication, and the `Notifier` port cannot fail. Ordering per target and isolation between targets come from the per-target queues. The cost is that notifications queued in a process that crashes, or beyond the shutdown deadline, are lost; the deployment history still has the transition, and the metric shows the loss.

### Durable outbox in Postgres

A `notifications` table written in the same transaction as each transition, delivered by a claimer with leases, gives at-least-once delivery across crashes and replicas. It needs a migration, a claim loop like the queue's, retry state per row, deduplication on the receiving side and pruning. For a homelab tool whose notifications are advisory, that is a lot of machinery for the rare crash between a transition and its message.

### Synchronous delivery in the engine

Sending from the engine step itself is simplest, but a slow or failing target would stretch or fail deploy steps, and the engine would need a policy for notification errors. It contradicts the rule that notifications never affect deploys.

## Consequences

- Receivers may miss a notification after a crash and must not treat notifications as the system of record; the API and history are.
- With several replicas, each replica sends notifications for the deployments it works on; order is only guaranteed per target within one process.
- GitHub deployment reporting is stateless (find-or-create by commit, environment and payload), so it survives restarts without stored IDs.
- Moving to an outbox later is a conscious change: a new ADR superseding this one, a migration, and a `Notifier` implementation that writes rows instead of queueing in memory. The port does not need to change.

# 0010: Webhooks plus a polling safety net

- Status: Accepted
- Date: 2026-11-24

## Context

doorstep learns about merged commits and finished CI from GitHub. Webhooks are fast and cheap, but many homelab installs sit behind NAT where GitHub cannot reach them, and even reachable installs miss deliveries: GitHub delivers at most once per event, does not retry failures by itself, gives up after 10 seconds and delivers out of order. doorstep must deploy the green head of each followed branch whichever way it hears about it, run with several replicas, and never act on an unsigned payload.

`intake` already makes tracking and enqueueing exactly-once and order-independent: a push is recorded only while it is the branch head, and settling a green commit enqueues it once under row locks. The open questions were how to deduplicate deliveries, what a poll should read, and how replicas share polling.

## Decision

- **Webhooks when configured, polling always.** `POST /webhooks/github` is mounted only when a webhook secret is configured, and verifies `X-Hub-Signature-256` over the raw body before anything else. A poller runs on every replica unless `intake.poll_interval` is `0`. Both call the same `intake.Service`.
- **Claim and release for deliveries.** After the signature check, deliveries that do intake work insert their `X-GitHub-Delivery` ID into `webhook_deliveries` with `ON CONFLICT DO NOTHING`; no insert means duplicate, answered `200` without work. If handling fails, the row is deleted and the response is `500`, so a manual redelivery is processed. Rows are kept for seven days. Deliveries are handled synchronously, detached from the request's cancellation and bounded to 25 seconds.
- **Head-only polling with no baseline.** Each cycle reads the head of every distinct followed repository and branch and hands a head it has not seen in this process to intake as a `poll` push, then re-evaluates open commits that were not evaluated in the last half interval. It does not list commits. The first cycle after an app is added or after downtime tracks and deploys the current head once green.
- **A timestamp claim elects the polling replica.** A single-row `intake_poll_state` table holds the start of the last cycle. Each replica ticks every `min(interval, 15s)` and runs `UPDATE ... SET polled_at = now() WHERE polled_at <= now() - interval RETURNING`; only the replica that gets a row polls.

## Options considered

### Delivery deduplication

- **Claim and release (chosen).** One statement before the work stops concurrent duplicates across replicas, and releasing on failure keeps GitHub's redeliver button useful. A crash between claim and completion leaves a claim behind, which the poller covers. Because intake is exactly-once by itself, the table only saves GitHub calls, so this simple scheme is enough.
- **Record after success.** Never strands a claim, but two replicas receiving a redelivery at the same moment both do the work.
- **No deduplication.** Correct, since intake is idempotent, but every redelivery repeats branch and check reads.

### What to poll

- **Branch head only (chosen).** Intake records a push only while it is the branch head and supersedes everything else, so the head is the only commit that can deploy. One call per repository and branch per cycle.
- **List commits since the last seen SHA.** More calls for the same outcome, plus persisted state per branch.
- **Establish a baseline on first poll.** Would avoid deploying a head that was already green when an app is added, but a freshly added app and a push missed during downtime look the same without more persisted state, and catching the latter is the point of the safety net. Converging the GitOps pin to the green head is doorstep's purpose, and it happens through the normal queue.

### Sharing the poll between replicas

- **Timestamp claim in Postgres (chosen).** One short statement, crash-safe (a dead claimer delays one interval), uses the database clock only, and follows the existing lock reminder claim.
- **Advisory lock held for the cycle.** Pins a pool connection across GitHub calls and complicates failure handling.
- **Every replica polls.** Correct, but multiplies API calls by the replica count.

## Consequences

- Installs GitHub cannot reach deploy within about one poll interval of CI finishing; reachable installs deploy within seconds and the poller mostly finds nothing.
- Adding an app, or restarting after downtime, deploys the branch head once green, even if nobody pushed. This is documented, not configurable.
- Installation cache eviction from `installation` events is local to the replica that received the delivery; others recover through the one-hour cache and `404`/`403` handling.
- The in-memory last-seen head is only an API-call saver; a restart costs one idempotent push per branch.
- Webhook secrets are read at startup, so rotating one needs a restart, as with the App private key.

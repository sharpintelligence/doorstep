# 0010: Webhooks plus a polling safety net

- Status: Accepted
- Date: 2026-11-24

## Context

doorstep learns about merged commits and finished CI from GitHub. Webhooks are fast and cheap, but many homelab installs sit behind NAT where GitHub cannot reach them, and even reachable installs miss deliveries: GitHub delivers at most once per event, does not retry failures by itself, gives up after 10 seconds and delivers out of order. doorstep must deploy the green head of each followed branch whichever way it hears about it, run with several replicas, and never act on an unsigned payload.

`intake` already makes tracking and enqueueing exactly-once and order-independent: a push is recorded only while it is the branch head, and settling a green commit enqueues it once under row locks. The open questions were how to deduplicate deliveries, what a poll should read, and how replicas share polling.

## Decision

- **Webhooks when configured, polling always.** `POST /webhooks/github` is mounted only when a webhook secret is configured, and verifies `X-Hub-Signature-256` over the raw body before anything else. A poller runs on every replica unless `intake.poll_interval` is `0`. Both call the same `intake.Service`.
- **Claim, complete and release for push deliveries.** After the signature check, `push` deliveries insert their `X-GitHub-Delivery` ID into `webhook_deliveries` and mark it completed after the work. A completed ID is a duplicate, answered `200` without work; an ID claimed but not completed is answered `409`, unless the claim is older than the handler timeout plus ten seconds, in which case the claimer crashed and the delivery is claimed again. If handling fails, the row is deleted and the response is `500`, so a manual redelivery is processed. Rows are kept for seven days. `check_run`, `check_suite` and `status` deliveries are not recorded, since re-evaluation is idempotent and costs no GitHub call for untracked commits. Deliveries are handled synchronously, detached from the request's cancellation and bounded to 25 seconds.
- **Head-only polling with no baseline.** Each cycle reads the head of every distinct followed repository and branch and hands a head to intake as a `poll` push unless the database shows intake already tracking it (every app on the branch has a row for it that is not superseded, and no app has another open commit), then re-evaluates open commits that were not evaluated in the last half interval. It does not list commits. The first cycle after an app is added or after downtime tracks and deploys the current head once green.
- **A lease elects the polling replica.** A single-row `intake_poll_state` table holds when the next cycle may start. Each replica ticks every `min(interval, 15s)` and runs `UPDATE ... SET next_poll_at = now() + lease WHERE next_poll_at <= now() RETURNING`; only the replica that gets a row polls. The cycle is bounded by `max(interval, 1m)` and the lease covers that plus five seconds; when the cycle ends the holder sets `next_poll_at = now() + interval`, so cycles never overlap and the interval is measured from the end of a cycle.

## Options considered

### Delivery deduplication

- **Claim, complete and release (chosen).** One statement before the work stops concurrent duplicates across replicas, and releasing on failure keeps GitHub's redeliver button useful. Recording completion separately lets a claim stranded by a crash expire after the handler timeout instead of answering `duplicate` for a delivery that was never processed, and lets a concurrent duplicate be told apart (`409`) from a finished one. Because intake is exactly-once by itself, the table only saves GitHub calls, which is why commit status events, the bulk of the traffic, skip it.
- **Record after success.** Never strands a claim, but two replicas receiving a redelivery at the same moment both do the work.
- **No deduplication.** Correct, since intake is idempotent, but every redelivery repeats branch and check reads.

### What to poll

- **Branch head only (chosen).** Intake records a push only while it is the branch head and supersedes everything else, so the head is the only commit that can deploy. One call per repository and branch per cycle.
- **List commits since the last seen SHA.** More calls for the same outcome, plus persisted state per branch.
- **Skip a head equal to the one this replica last saw.** Cheaper than asking the database, but wrong: if a webhook moves intake to a newer commit and the webhook for a force-push back is lost, the poller still remembers the old head, skips it and leaves it superseded. Comparing against intake's tracked rows has no such gap.
- **Establish a baseline on first poll.** Would avoid deploying a head that was already green when an app is added, but a freshly added app and a push missed during downtime look the same without more persisted state, and catching the latter is the point of the safety net. Converging the GitOps pin to the green head is doorstep's purpose, and it happens through the normal queue.

### Sharing the poll between replicas

- **Lease in Postgres (chosen).** One short statement to claim and one to finish, crash-safe (a dead claimer delays polling until its lease expires), uses the database clock only, and follows the existing lock reminder claim.
- **Advisory lock held for the cycle.** Pins a pool connection across GitHub calls and complicates failure handling.
- **Every replica polls.** Correct, but multiplies API calls by the replica count.

## Consequences

- Installs GitHub cannot reach deploy within about one poll interval of CI finishing; reachable installs deploy within seconds and the poller mostly finds nothing.
- Adding an app, or restarting after downtime, deploys the branch head once green, even if nobody pushed. This is documented, not configurable.
- Installation cache eviction from `installation` events is local to the replica that received the delivery; others recover through the one-hour cache and `404`/`403` handling.
- The poller compares branch heads against intake's durable state, not memory: a force-push back whose webhook was lost after a webhook superseded the commit is still caught, any replica can run any cycle, and a restart costs nothing extra. Checking costs two indexed queries per branch per cycle and no GitHub call.
- Webhook secrets are read at startup, so rotating one needs a restart, as with the App private key.

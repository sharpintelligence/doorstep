# Database

Postgres holds doorstep's operational state: the deploy queue, locks, deployment history, gate results and the commits waiting for CI. Git remains the source of truth for what should run (see [ADR 0001](decisions/0001-go-and-core-design.md), decision 3); nothing in the cluster is ever reconciled from this database.

The code lives in `internal/store` (pgx pool, transactions, migrations and repositories) with shared domain types in `internal/core`.

## Supported versions and privileges

- Postgres 15 or newer. doorstep is developed and tested on Postgres 18 (CI runs `postgres:18-alpine`). The schema uses nothing newer than identity columns, partial indexes and `SKIP LOCKED`.
- The role in the connection URL needs `CONNECT` on the database and `USAGE` and `CREATE` on the schema doorstep uses (normally `public`, or the first entry of `search_path`). It owns every table it creates, so no further grants are needed. It does not need `SUPERUSER`, `CREATEDB` or `CREATEROLE`.
- Advisory locks (`pg_advisory_lock`) must be available; they are on every standard Postgres.

The connection is configured with `DOORSTEP_DATABASE_URL`, `DOORSTEP_DATABASE_URL_FILE` or `database.url_file` in the configuration file; see [configuration.md](configuration.md#database).

## Migrations

Migrations are plain SQL files embedded in the binary (`internal/store/migrations`) and applied with [goose](https://github.com/pressly/goose) through its `Provider` API. goose records applied versions in a `goose_db_version` table in the same schema.

- **Forward-only.** Files contain only a `-- +goose Up` section. To undo a change, write a new migration. Downgrading doorstep across a schema change is not supported.
- **Numbering.** Files are named `NNNNN_description.sql` with a zero-padded, strictly increasing version (`00001_initial_schema.sql`, `00002_...`). Out-of-order versions are rejected.
- **Transactions.** Each file runs in its own transaction, so a failed migration leaves the schema at the previous version.
- **When they run.** `doorstep serve` applies pending migrations after connecting and before it opens its listener, so an instance is never ready on an old schema. `doorstep migrate` applies them on demand (for example as a Kubernetes Job before a rollout) and `doorstep migrate status` lists applied and pending versions.
- **Concurrent starts.** Migration runs hold a Postgres session advisory lock, so several replicas starting together apply each migration exactly once; the others wait (retrying every second, for up to two minutes) and then find nothing pending. The lock ID is derived from the schema being migrated (`store.MigrationLockID`, FNV-1a of `doorstep-migrations:` and `current_schema()`), matching the scope of `goose_db_version`: installations in different schemas of one database never wait for each other, and neither do parallel tests.
- **Newer schema.** Before applying anything, `doorstep serve` and `doorstep migrate` compare the database version with the newest migration the binary embeds. If the database is newer (an older binary after a downgrade), they fail with `database schema is newer than this doorstep binary` without touching the schema, instead of running against a schema they do not understand. `doorstep migrate status` reports the same error.

### Adding a migration

Each feature that needs storage adds its own numbered file in `internal/store/migrations` and the repository methods that use it. Keep these rules:

- Never edit a migration that has been merged; add a new one.
- Use `text` columns with a named `CHECK` constraint for enumerations, and add the value to the matching `core` constant list in the same change. The schema tests insert every `core` value, so Go and SQL cannot drift.
- Name constraints and indexes explicitly (`<table>_<column>_check`, `<table>_<columns>_idx`) so errors and later migrations can refer to them.
- Prefer operations that do not rewrite or long-lock large tables (`CREATE INDEX CONCURRENTLY` needs its own non-transactional migration with `-- +goose NO TRANSACTION`).

## Schema

All ids are `bigint GENERATED ALWAYS AS IDENTITY`. Timestamps are `timestamptz`.

- **Denormalised `app_id`.** `queue_entries`, `deployment_events` and `locks` carry the target's `app_id` next to `deployment_id`. `deployments` has `UNIQUE (id, app_id)`, and each of those tables references `deployments (id, app_id)` with a composite foreign key, so the copy can never disagree with the deployment it belongs to. Moving a deployment to another app is rejected while anything references it.
- **Foreign key indexes.** Every foreign key has an index that leads with its first column (partial, `WHERE col IS NOT NULL`, for the nullable lineage columns and `locks.deployment_id`), so cascades and `SET NULL` actions never scan a whole table. A schema test enforces this for every foreign key, including those added by later migrations.
- **`updated_at`.** A `BEFORE UPDATE` trigger (`set_updated_at()`) sets `updated_at` to `now()` on every update of `deployments`, `gate_results` and `tracked_commits`. Repository code never sets it. Tables added later that have an `updated_at` column attach the same trigger.

### `apps`

One row per deploy target, `core.Target{App, Environment}`. It holds identity only: `name`, `environment`, `created_at`, with `UNIQUE (name, environment)`. Repository, GitOps path and strategy live in the configuration file. Rows are never deleted, so history survives an app being removed from configuration.

### `deployments`

One row per deployment request, keyed by a store-assigned `id` (`core.DeploymentID`). A rollback is its own row of kind `rollback` that names the deployment it reverts in `rollback_of_id` ([ADR 0002](decisions/0002-rollback-is-a-new-deployment.md)).

| Column | Notes |
| --- | --- |
| `app_id` | the target |
| `kind` | `deploy` or `rollback` |
| `state` | one of the states in [architecture.md](architecture.md#state-machine-and-concurrency) |
| `source` | `webhook`, `poll`, `api`, `cli` or `system` |
| `requested_by` | free-form actor |
| `source_repository`, `commit_sha` | app repository commit; the SHA must be 40 lowercase hex characters |
| `image_repository`, `image_tag` | requested image (tags are the full commit SHA by convention) |
| `image_digest` | resolved digest; `sha256:` and 64 lowercase hex characters, or null until resolved |
| `previous_digest` | digest observed in git before pinning; a recorded observation only, git stays authoritative |
| `pin_commit_sha` | GitOps commit that pinned the digest |
| `gitops_repository` | GitOps repository `pin_commit_sha` belongs to; stored beside it because `apps` rows outlive the configuration that names the repository. Null or non-blank |
| `argocd_revision` | revision Argo CD reported as synced; free text (a commit SHA, a chart version or several sources). Null or non-blank |
| `parent_id`, `rollback_of_id`, `superseded_by_id` | lineage: retry of, rollback of, superseded by; `ON DELETE SET NULL` |
| `failure_reason` | human-readable reason for `failed` |
| `created_at`, `updated_at`, `started_at`, `finished_at` | lifecycle timestamps; `updated_at` is maintained by a trigger |

`deployments_app_id_commit_sha_idx` on `(app_id, commit_sha, id)` serves the enqueue duplicate check and the "first seen" lookup batching uses to order commits.

`image_digest`, `previous_digest`, `gitops_repository`, `pin_commit_sha` and `argocd_revision` are the deployment's lineage; they are written through `SetLineage` or with a transition (see [Transitions and lineage](#transitions-and-lineage)).


The partial index `deployments_last_succeeded_idx` on `(app_id, finished_at DESC, id DESC) WHERE state = 'succeeded'` serves the last-successful lookup used for rollbacks.

The partial unique index `deployments_one_active_per_app` on `app_id`, restricted to the active states (`resolving`, `pinning`, `syncing`, `verifying`), enforces at most one active deployment per target regardless of what the queue does.

### `queue_entries`

The claim table. A row exists from enqueue until the deployment needs no more work. Every transition to a terminal state deletes the deployment's entry in the same transaction (`TransitionDeployment` does this); the claim also ignores entries whose deployment is terminal, so a leftover entry can never be handed out again.

| Column | Notes |
| --- | --- |
| `deployment_id` | primary key, `ON DELETE CASCADE` |
| `app_id` | the target, denormalised for the head-of-line check and kept equal to the deployment's by the composite foreign key |
| `priority` | higher runs first: `normal` 0, `urgent` 100, `rollback` 200 (`core.Priority`) |
| `enqueued_at`, `available_at` | `available_at` in the future delays the entry (backoff) |
| `lease_owner`, `lease_expires_at` | both null, or both set |
| `attempts` | incremented on every claim |

Migration 00002 adds row triggers that call `notify_queue_change()`, which runs `pg_notify('doorstep_queue', TG_TABLE_SCHEMA)`: after every insert and delete, and after an update that clears a lease or changes `priority` or `available_at`. Claims and heartbeats do not notify. See [Wake-ups](#wake-ups).

### `locks`

`(app_id, kind)` is unique, with `kind` `deployment` or `freeze`, so one deployment lock and one manual freeze can coexist on a target. Columns: `owner`, `reason`, optional `deployment_id` (`ON DELETE CASCADE`), `created_at`, optional `expires_at` and `reminded_at` (when the last long-held reminder was sent). Released and expired locks are deleted; their audit trail is in [`lock_events`](#lock_events).

- **Global freeze.** A row with a null `app_id` is a global freeze. `locks_global_check` only allows that for kind `freeze` without a `deployment_id`, and the partial unique index `locks_global_kind_key` on `kind WHERE app_id IS NULL` allows one. Unique constraints treat nulls as distinct, so `locks_app_id_kind_key` does not cover global rows; the composite foreign key to `deployments` is `MATCH SIMPLE` and is not checked when `app_id` is null.
- **Expiry.** `locks_expires_at_check` requires `expires_at > created_at`. A lock is live while `expires_at` is null or after `now()`; every query compares against `now()`, so an expired row is ignored at once and deleted later. See [locks.md](locks.md#expiry).

### `lock_events`

The append-only audit of freeze locks: `app_id` (null for the global freeze), `action` (`locked`, `unlocked`, `expired`, `reminded`, `core.LockActions`), `owner` of the lock, `actor` (who unlocked it; empty for system actions), `reason`, `expires_at` and `occurred_at`. Indexed by `(app_id, id DESC)`. Each row is written in the same statement or transaction as the lock change it records. It is separate from `deployment_events`, which requires a deployment, because a freeze belongs to an app or to everything rather than to one deployment.

- **Retention.** Rows are kept indefinitely, like `deployment_events`. A freeze changes a handful of times a day at most, so the table stays small; append-only is a property of the API, so a later retention job can prune old rows without a schema change.
- **Deleting an app.** `app_id` references `apps` without an `ON DELETE` action, the same as `deployments`, `queue_entries` and `locks`. doorstep never deletes `apps` rows: removing an app from the configuration file leaves its row, history and audit in place. Deleting a row by hand fails while anything, including its lock audit, refers to it, so the audit cannot disappear by accident.

### `gate_results`

The latest result of each named gate for a deployment: `UNIQUE (deployment_id, name)`, upserted on re-evaluation. Columns: `status` (`pending`, `passed`, `failed`, `skipped`), `message`, `evidence jsonb`, `started_at`, `finished_at`, `updated_at`. Earlier evaluations are recorded as `gate` events.

### `deployment_events`

The append-only history: `deployment_id`, denormalised `app_id`, `type` (`transition`, `gate`, `lock`, `note`), optional `from_state` and `to_state`, `actor`, `message`, `data jsonb`, `occurred_at`. Indexed by `(deployment_id, id)` for a deployment's timeline and `(app_id, id DESC)` for a target's recent activity. Append-only is a property of the API (there are no update or delete methods), not of triggers, so retention pruning stays possible. Creating a deployment writes its first `transition` event (to `queued`, without `from_state`) in the same transaction; every later transition records both `from_state` and `to_state`. Lineage written outside a transition is recorded as a `note` event with the message `lineage recorded`.

### `tracked_commits`

Added by migration `00005_tracked_commits.sql`. One row per app and pushed commit that doorstep is watching CI for, written by `internal/intake` (see [architecture.md](architecture.md#merge-and-ci-detection)). Tracking is per app rather than per repository because several apps can share a source repository with different required checks.

| Column | Notes |
| --- | --- |
| `app_id` | the target |
| `repository`, `branch`, `commit_sha` | the app's configured repository and branch, and the pushed commit (40 lowercase hex characters) |
| `source`, `pushed_by` | how the push was observed (`webhook`, `poll`, `api`, `cli`, `system`) and by whom; copied onto the deployment |
| `checks_state` | `pending`, `success` or `failure` (`core.ChecksState`); starts `pending` |
| `checks` | the evidence of the last evaluation: a JSON array of `{name, kind, state, detail}` (`core.Check`) |
| `deployment_id` | the deployment created for the commit; `UNIQUE`, `ON DELETE SET NULL` |
| `enqueued_at` | when the deployment was created; set together with `deployment_id` (`tracked_commits_enqueued_check`) and kept if the deployment is later deleted, so the commit never deploys twice |
| `superseded_by_id` | the tracked commit that became the branch head after this one; `ON DELETE SET NULL` |
| `created_at`, `updated_at`, `evaluated_at` | `evaluated_at` is the time the saved checks were read from GitHub, not the time they were saved |

`UNIQUE (app_id, commit_sha)` keeps one row per app and commit: tracking a known commit again returns the existing row (`existing`), or reopens it when it was superseded and is the branch head again (`reopened`). `tracked_commits_commit_sha_idx` serves the lookup by SHA that every check run and status event performs (most of them for pull request commits nobody tracks), and the partial index `tracked_commits_open_idx` on `app_id` covers the open commits the poller lists.

A commit is open while both `enqueued_at` and `superseded_by_id` are null. Only open commits are evaluated. The rules that keep this consistent with several replicas:

- **Tracking and supersession.** `TrackCommit(ctx, commit, isHead)` locks the app's `apps` row (`FOR NO KEY UPDATE`) and calls `isHead` while holding it. If the commit is not the branch head it returns `stale` without writing. Otherwise it inserts the commit (`created`), reopens a superseded row (`reopened`) or finds the existing one (`existing`), and sets `superseded_by_id` on every other open commit of the app, all in one transaction. Pushes for one app are serialised and each sees the current head, so the branch head always wins regardless of the order in which pushes are delivered.
- **Settlement.** `SettleTrackedCommit(ctx, id, evaluation, enqueue)` locks the app row and then the tracked row and does nothing for a closed commit. It saves `checks_state`, `checks` and `evaluated_at` only when `evaluated_at` is null or older than the evaluation's read time, so an older snapshot never overwrites a newer one. When the stored state is then `success`, it calls `enqueue` with a store bound to the same transaction and stores the deployment id and `enqueued_at`. The evaluation, the deployment, its queue entry and the link commit together or not at all, so concurrent settlements create exactly one deployment and never deploy on a stale snapshot. Locking the app row first, in the same order as `TrackCommit`, keeps a push and a settlement from deadlocking; whichever commits first wins, and the other sees the commit closed.
- **Open commits.** `OpenTrackedCommits(ctx, followed, limit)` lists open commits of the given app, repository (case-insensitive) and branch triples, least recently evaluated first, so rows of apps that are no longer configured never take the poller's slots.

## Transactions

`Store.InTx(ctx, func(tx *store.Store) error)` runs the callback in a `READ COMMITTED` transaction and hands it a `Store` bound to that transaction, so every repository method works inside or outside a transaction. The transaction commits when the callback returns nil and rolls back on an error or a panic. Calling `InTx` on a transaction-bound store creates a savepoint. There is no automatic retry. `Migrate`, `MigrationStatus` and `Ping` are only available on the root store.

## Transitions and lineage

`Store.TransitionDeployment(ctx, store.Transition{...})` is the only way a deployment changes state after it is created. It is a compare-and-set on `(id, from_state)`:

1. Before touching the database it checks `From -> To` against the state machine (`core.ErrInvalidTransition`); that a move to `failed` carries a non-blank `FailureReason` and a move to `superseded` a `SupersededByID` naming another deployment, and that neither is set on any other transition; that `Data` is empty or a JSON object without lineage keys; and that the lineage patch is valid.
2. In one transaction (a savepoint when called inside `InTx`) it runs `UPDATE deployments ... WHERE id = $1 AND state = $from`. When no row matches, a follow-up read tells the two cases apart: a missing deployment is `core.ErrNotFound`, a deployment in another state is `core.ErrStateConflict` (the error names the current state). Exactly one of several racing workers wins; the others get `ErrStateConflict` and should re-read.
3. It appends a `transition` event with `from_state`, `to_state`, `actor`, `message` and `data`.
4. When `To` is terminal, it deletes the deployment's queue entry.
5. It returns the reloaded deployment.

Side effects owned by the transition, all on the database clock: `started_at` is set the first time the deployment enters an active state and never moved afterwards; `finished_at` is set on a terminal state. `failure_reason` and `superseded_by_id` are recorded with their transitions. Because a nested `InTx` is a savepoint, the engine can fail a deployment and enqueue its rollback (`CreateDeployment`) atomically, as [ADR 0002](decisions/0002-rollback-is-a-new-deployment.md) requires.

Lineage (`core.Lineage{Digest, PreviousDigest, GitOpsRepository, PinCommitSHA, ArgoCDRevision}`) is a patch: empty fields leave the column unchanged. It is written either

- by `Store.SetLineage(ctx, id, lineage, actor)` as soon as a step learns it, so a crash before the next transition does not lose it; this appends a `note` event whose `data` holds the fields that were set (`image_digest`, `previous_digest`, `gitops_repository`, `pin_commit_sha`, `argocd_revision`); or
- with a transition through `Transition.Lineage`, atomically with the state change; the same keys are merged into the transition event's `data`. `Transition.Data` must not use those keys itself, so a recorded value always means the column was written; a transition whose `Data` names one is rejected.

The last write wins while the deployment is not terminal, because a resumed step may legitimately report a different commit; the events keep every value auditable. Once a deployment is terminal its lineage is final: `SetLineage` returns `core.ErrStateConflict`, and a missing deployment `core.ErrNotFound`. An empty patch is rejected.

Transitions do not check the queue lease; the compare-and-set is the guard. Lease fencing belongs to the queue and can be added as an optional field later.

## History read models

These back the [HTTP API](api.md) and the engine's rollback lookup.

- `Store.ListDeployments(ctx, store.DeploymentFilter{App, Environment, States, Kinds, CommitSHA, Digest, Before, Limit})` returns a `DeploymentPage{Items, Next}` ordered by id, newest first. Every filter field is optional; `Digest` matches the resolved `image_digest`. Pagination is keyset on `id` (`Before` is the previous page's `Next`); the store fetches one extra row to know whether another page exists. `Limit` defaults to 50 and is clamped to 200. Unknown states or kinds, malformed commit SHAs or digests and negative cursors are rejected.
- `Store.GetDeploymentHistory(ctx, id)` returns a `DeploymentHistory`: the deployment, its newest 500 events listed oldest first (`EventsTruncated` reports that older ones were left out, so the outcome is never the part that is cut), its gate results ordered by name, and the deployments that roll it back (`rollback_of_id`) or retry it (`parent_id`). On the root store it reads everything in one `REPEATABLE READ`, read-only transaction so the parts agree; inside `InTx` it uses the caller's transaction.
- `Store.LastSuccessfulDeployment(ctx, target, excludeDigest)` returns the `succeeded` deployment of the target that finished last (`finished_at DESC, id DESC`) and has a resolved `image_digest`, or `core.ErrNotFound`. A succeeded deployment without a digest is skipped, because a rollback must pin a digest. Ordering by finish time matters because a high-priority rollback queued later can finish before an older deploy. A succeeded rollback counts: it is what is running. A non-empty `excludeDigest` skips deployments with that digest, so the automatic rollback of a failed deploy and an operator rollback of a succeeded one (skipping itself and any retry of the same bad digest) share one lookup. Git remains authoritative for the previous digest ([ADR 0001](decisions/0001-go-and-core-design.md)); this lookup supplies the commit and image fields for the rollback deployment.

Coverage today: the store records and serves every lineage field, but nothing in `serve` calls `SetLineage` or `TransitionDeployment` yet, and nothing writes `gate_results`; the deploy engine and the gate work wire them in. The trigger is recorded as `source` and `requested_by`; a reference to what triggered it (a webhook delivery id or a pull request) is not stored yet.

## Claiming work

`Store.Claim(ctx, workerID, lease)` picks the next deployment and leases it to the worker in one statement (a CTE with `FOR NO KEY UPDATE ... SKIP LOCKED` feeding an `UPDATE ... RETURNING`), then loads the deployment in the same transaction. An entry is eligible when:

- its deployment is `queued` or in an active state (`core.ClaimableStates`);
- `available_at <= now()`;
- it has no live lease (`lease_expires_at` is null or in the past);
- it is the head of its target: no other claimable entry for the same app ranks higher, where the rank is the row `(deployment is in an active state, priority, -deployment_id)` compared with `>`. An in-flight deployment therefore always outranks queued entries of its app, even after its lease expired, so a higher-priority entry can never be claimed while another deployment of the target is active (which `deployments_one_active_per_app` would reject);
- no other entry for the same app holds a live lease, even one whose deployment has just become terminal, because its worker may still be finishing a step;
- it is not frozen: its deployment is in an active state, or of kind `rollback`, or no live freeze covers its app (neither the app's own `freeze` row nor the global one). See [locks.md](locks.md#what-a-freeze-blocks).

Eligible entries are ordered by the same rank: `(d.state <> 'queued') DESC, priority DESC, deployment_id`, so resumed in-flight work is claimed before new work anywhere. Claiming sets `lease_owner`, sets `lease_expires_at = now() + lease` and increments `attempts`; the result carries the attempt number and lease expiry, and `Claim.Skipped` lists the targets the re-check (below) excluded on the way, each with its reason (`leased` or `frozen`), so the caller can log or count why a target did not move.

The head-of-line rule serialises work per target without a second mechanism. The live-lease rule stops a high-priority rollback entry from being claimed while the failing deployment is still leased. An expired lease makes its entry claimable again, so a crashed replica never wedges a target.

`SKIP LOCKED` also locks the target's `apps` row, so concurrent claimers skip a target another worker is claiming. Because a `READ COMMITTED` statement works from a snapshot taken when it starts, a claim can still race with one that committed a moment earlier; after claiming, a second statement (with a fresh snapshot, while the `apps` row is locked) checks that no other entry for the target holds a live lease, and if one does, the claim is rolled back and retried with that target excluded, so a contended target never makes `Claim` report an empty queue while other targets have work. Each retry excludes one more target, so the loop ends. The partial unique index on active deployments remains the final guard.

The same re-check also rejects the claim when a live freeze now covers a queued deploy, which catches a freeze committed between the claim's snapshot and its lease; the target is excluded and the loop continues. Taking an app freeze locks the app's `apps` row first (`FOR NO KEY UPDATE`), the row a claim locks with `SKIP LOCKED`, so an app freeze and a claim of that app are strictly ordered. A global freeze and every claim are ordered by a transaction-level advisory lock on a key derived from the schema name: each claim takes it shared (`pg_advisory_xact_lock_shared`), so claims never wait for each other, and taking a global freeze takes it exclusively (`pg_advisory_xact_lock`). A global freeze therefore waits for claims already in progress, which counted as started before it, and a claim that starts while a global freeze is being taken waits for it to commit and then sees it. A frozen head of line also holds every entry behind it, because the head-of-line rule does not look at freezes; a higher-priority rollback becomes the head and is claimed.

### Batching

`Store.ClaimBatch(ctx, workerID, lease, limit)` is `Claim` plus batching, and is what `queue.Queue` uses; `Claim` never batches. After the claim above succeeds, and only when the claimed head is a `queued` deployment of kind `deploy`, the same transaction (which still holds the app's row lock):

1. selects the window: queued deploys of the app at the head's priority with an id at or above the head's, ordered by id, `LIMIT max(limit(target), 1)`, locking their queue entries;
2. takes the deploy with the newest commit in the window as the winner, where a commit's age is the lowest deployment id of the app with that SHA ("first seen"; ties, which deduplication rules out, fall to the higher id);
3. adds the queued deploys of the app with a lower priority and an id below the winner;
4. drops every candidate whose commit was first seen at or after the winner's, so a redeploy of an older commit never supersedes a newer one;
5. marks the rest `superseded` (`superseded_by_id` = winner, `finished_at = now()`, only while still `queued`), deletes their queue entries and records one `transition` event each (`queued` to `superseded`, actor `doorstep`, message `superseded by deployment <id>`);
6. if the winner is not the head, leases the winner instead (attempts incremented, `available_at` pulled forward to `now()` if it was in the future).

The result carries the winner, its claim and the superseded ids. Rollbacks and in-flight heads never batch. Behaviour and rationale: [queue.md](queue.md#batching) and [ADR 0003](decisions/0003-queue-batching-and-urgent.md).

### Enqueue, leases and reads

- `EnqueueDeployment(ctx, d, priority)` locks the app's row with `FOR NO KEY UPDATE`, then for kind `deploy` returns an existing queued or in-flight deploy of the same commit SHA (`created = false`), raising it to the requested priority with a `note` event when it is still queued at a lower one. Otherwise it calls `CreateDeployment`. There is no unique index; the row lock serialises concurrent enqueues of one app. `CreateDeployment` stays the raw insert the engine uses for rollbacks.
- `ExpediteDeployment(ctx, id, priority, actor)` raises a queued deploy under the same lock; anything else is `core.ErrNotFound`.
- `HeartbeatQueueEntry` extends a lease only while `lease_owner` is the worker and `lease_expires_at > now()`; `ReleaseQueueEntry` clears the owner's lease and sets `available_at = now() + delay`, or deletes the entry when its deployment is already in a terminal state, so a handler that finished a deployment without deleting the entry cannot leave an orphaned row behind; `CompleteQueueEntry` deletes the owner's entry. Each returns `core.ErrLeaseLost` when no row matched.
- `QueueEntries(ctx, app)` lists the claimable entries of every environment of an app by name, ordered per environment by the claim rank (treating a live lease as in flight), with 1-based positions and the lease expiry only while it is live. `QueueDepth` counts entries whose deployment is `queued` and not leased.

### Wake-ups

`Store.ListenQueue(ctx, wake, logger)` opens one dedicated connection with the pool's settings (it uses no pool capacity), reads `current_schema()`, runs `LISTEN doorstep_queue` and calls `wake` for every notification whose payload is that schema, so several installations or test schemas in one database do not wake each other. It also calls `wake` after every successful `LISTEN`. When the connection fails it reconnects with backoff from 1 to 30 seconds and returns only when the context ends. Workers poll as well, so notifications are a latency hint only and transaction-mode poolers that drop `LISTEN` still work.

## Clock

Row timestamps, lease expiry, lock expiry and reminders, and every eligibility comparison use the database's `now()`. Several replicas must agree on when a lease expires, and the database is the only clock they share. This is the one exception to the injected `Clock` rule in [architecture.md](architecture.md#dependency-rules): the engine still uses an injected clock for its own timing, and passes lease lengths to the store as durations. Tests force a lease to expire by updating `lease_expires_at` directly.

## Testing

Integration tests use `internal/store/storetest`, which gives each test its own schema; see [development.md](development.md#integration-tests).

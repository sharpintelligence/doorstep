# Database

Postgres holds doorstep's operational state: the deploy queue, locks, deployment history, gate results, API tokens, sign-in sessions and the audit log. Git remains the source of truth for what should run (see [ADR 0001](decisions/0001-go-and-core-design.md), decision 3); nothing in the cluster is ever reconciled from this database.

The code lives in `internal/store` (pgx pool, transactions, migrations and repositories) with shared domain types in `internal/core`.

## Supported versions and privileges

- Postgres 15 or newer. doorstep is developed and tested on Postgres 18 (CI runs `postgres:18-alpine`). The schema uses nothing newer than identity columns, partial indexes and `SKIP LOCKED`.
- The role in the connection URL needs `CONNECT` on the database and `USAGE` and `CREATE` on the schema doorstep uses (normally `public`, or the first entry of `search_path`). It owns every table it creates, so no further grants are needed. It does not need `SUPERUSER`, `CREATEDB` or `CREATEROLE`.
- Advisory locks (`pg_advisory_lock`) must be available; they are on every standard Postgres.

The connection is configured with `DOORSTEP_DATABASE_URL`, `DOORSTEP_DATABASE_URL_FILE` or `database.url_file` in the configuration file; see [configuration.md](configuration.md#database).

## Migrations

Migrations are plain SQL files embedded in the binary (`internal/store/migrations`) and applied with [goose](https://github.com/pressly/goose) through its `Provider` API. goose records applied versions in a `goose_db_version` table in the same schema.

- **Forward-only.** Files contain only a `-- +goose Up` section. To undo a change, write a new migration. The schema is never migrated down; instead the previous release can run against the newer schema (see [Compatibility across one release](#compatibility-across-one-release)).
- **Numbering.** Files are named `NNNNN_description.sql` with a zero-padded, strictly increasing version (`00001_initial_schema.sql`, `00002_...`). Out-of-order versions are rejected.
- **Transactions.** Each file runs in its own transaction, so a failed migration leaves the schema at the previous version.
- **When they run.** `doorstep serve` applies pending migrations after connecting and before it opens its listener, so an instance is never ready on an old schema. `doorstep migrate` applies them on demand (for example as a Kubernetes Job before a rollout) and `doorstep migrate status` lists applied and pending versions.
- **Concurrent starts.** Migration runs hold a Postgres session advisory lock, so several replicas starting together apply each migration exactly once; the others wait (retrying every second, for up to two minutes) and then find nothing pending. The lock ID is derived from the schema being migrated (`store.MigrationLockID`, FNV-1a of `doorstep-migrations:` and `current_schema()`), matching the scope of `goose_db_version`: installations in different schemas of one database never wait for each other, and neither do parallel tests.
- **Newer schema.** Before applying anything, `doorstep serve` and `doorstep migrate` compare the database version with the newest migration the binary embeds. If the database is newer (the previous release during a rolling update, or after a downgrade), they read the compatibility floor `min_binary_schema` from [`schema_compatibility`](#schema_compatibility). When the binary's newest migration is at least the floor, they start in one-release compatibility mode: nothing is applied, and a warning `database schema is newer than this binary; running in one-release compatibility mode` is logged with `database_version`, `binary_version` and `min_binary_schema`. `doorstep migrate status` prints the same fact as a note above its table. Otherwise, or when the table or its row is missing, they fail with `database schema is newer than this doorstep binary` without touching the schema, naming the floor, instead of running against a schema they do not understand. Binaries older than migration `00012` have no compatibility mode and always fail against a newer schema.

### Adding a migration

Each feature that needs storage adds its own numbered file in `internal/store/migrations` and the repository methods that use it. Keep these rules:

- Never edit a migration that has been merged; add a new one.
- Use `text` columns with a named `CHECK` constraint for enumerations, and add the value to the matching `core` constant list in the same change. The schema tests insert every `core` value, so Go and SQL cannot drift.
- Name constraints and indexes explicitly (`<table>_<column>_check`, `<table>_<columns>_idx`) so errors and later migrations can refer to them.
- Prefer operations that do not rewrite or long-lock large tables (`CREATE INDEX CONCURRENTLY` needs its own non-transactional migration with `-- +goose NO TRANSACTION`).
- Keep the previous release working on the new schema ([Compatibility across one release](#compatibility-across-one-release)).

### Compatibility across one release

doorstep upgrades itself with a rolling update, and the way back from a bad release is reverting its pin commit ([self-hosting.md](self-hosting.md)). In both cases the previous release runs against a database the new release already migrated. Migrations therefore follow expand/contract:

- **Expand.** Release N adds tables, nullable columns, columns with defaults, indexes and enumeration values, and writes both the old and the new shape where readers of the old one still exist. Release N-1 must keep working against everything N adds.
- **Contract.** Only release N+1 may remove or narrow what release N no longer reads or writes: drop a table or column, rename, change a type, make a column `NOT NULL`, remove an enumeration value.

A contract migration is named `NNNNN_contract_<description>.sql` and raises the compatibility floor in the same file, to the newest migration version of the previous release tag:

```sql
UPDATE schema_compatibility SET min_binary_schema = 14;
```

Find that version with `git ls-tree --name-only <previous tag> internal/store/migrations/ | tail -1`. The previous release can still start; anything older now refuses to, which is the point. The supported downgrade is exactly one release; the floor is a guard set by hand, not an exact release counter.

`TestEmbeddedMigrationsFollowCompatibilityPolicy` in `internal/store` enforces the mechanical part without a database, for every migration after `00011` (the earlier ones predate the policy and are grandfathered): a file whose `Up` section contains `DROP TABLE`, `DROP COLUMN`, `RENAME`, `ALTER COLUMN ... TYPE`, `SET NOT NULL`, or `ADD COLUMN ... NOT NULL` without `DEFAULT` must be a contract file; a contract file must update the floor; and a floor must be at least `12`, at least the floor set by earlier files, and below the file's own version. Comments and string literals are ignored. Replacing a `CHECK` constraint is not flagged, because that is how enumeration values are added.

Review every migration for what a pattern cannot see:

- **Enumeration `CHECK` constraints.** Adding a value is an expand step, but the previous release must not crash when it reads a row with the new value (for example an unknown state in a `switch` or a strict decoder). Removing a value is a contract step.
- **Values old readers cannot parse.** New JSON shapes in `jsonb` columns, new formats in text columns and new meanings for existing values must be readable by the previous release, or written only once nothing older runs.
- **Indexes behind `ON CONFLICT`.** Dropping or changing a unique index or constraint that an `INSERT ... ON CONFLICT` of the previous release names breaks it, even though the statement looks like an expand.
- **`NOT NULL` and defaults.** A new required column needs a default the previous release's inserts can rely on; a default that differs from what the previous release writes changes its behaviour.
- **Backfills.** Large `UPDATE`s run inside the migration's transaction and hold locks while the old pod keeps serving; batch them or move them to a later release.
- **The trailer.** The pull request's squash-merge commit message carries `Doorstep-Migration: true`, so the self-hosted install flags the release ([self-hosting.md](self-hosting.md#configuration)).

## Schema

All ids are `bigint GENERATED ALWAYS AS IDENTITY`. Timestamps are `timestamptz`.

- **Denormalised `app_id`.** `queue_entries`, `deployment_events` and `locks` carry the target's `app_id` next to `deployment_id`. `deployments` has `UNIQUE (id, app_id)`, and each of those tables references `deployments (id, app_id)` with a composite foreign key, so the copy can never disagree with the deployment it belongs to. Moving a deployment to another app is rejected while anything references it.
- **Foreign key indexes.** Every foreign key has an index that leads with its first column (partial, `WHERE col IS NOT NULL`, for the nullable lineage columns and `locks.deployment_id`), so cascades and `SET NULL` actions never scan a whole table. A schema test enforces this for every foreign key, including those added by later migrations.
- **`updated_at`.** A `BEFORE UPDATE` trigger (`set_updated_at()`) sets `updated_at` to `now()` on every update of `deployments`, `gate_results`, `gate_reports` and `tracked_commits`. Repository code never sets it. Tables added later that have an `updated_at` column attach the same trigger.

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
| `requested_by` | the requesting actor as `core.Actor.String()`, such as `user:alice` or `token:ci-blog` |
| `source_repository`, `commit_sha` | app repository commit; the SHA must be 40 lowercase hex characters |
| `image_repository`, `image_tag` | requested image (tags are the full commit SHA by convention) |
| `image_digest` | resolved digest; `sha256:` and 64 lowercase hex characters, or null until resolved |
| `previous_digest` | digest observed in git before pinning; a recorded observation only, git stays authoritative |
| `pin_commit_sha` | GitOps commit that pinned the digest |
| `gitops_repository` | GitOps repository `pin_commit_sha` belongs to; stored beside it because `apps` rows outlive the configuration that names the repository. Null or non-blank |
| `argocd_revision` | revision Argo CD reported as synced; free text (a commit SHA, a chart version or several sources). Null or non-blank |
| `parent_id`, `rollback_of_id`, `superseded_by_id` | lineage: retry of, rollback of, superseded by; `ON DELETE SET NULL` |
| `failure_reason` | human-readable reason for `failed` |
| `migration`, `migration_reason` | whether the release is flagged as containing database migrations, and why (at most 200 characters, empty unless flagged; `deployments_migration_reason_check`). Set when the deployment is created and never changed ([rollback.md](rollback.md#migration-aware-releases)) |
| `created_at`, `updated_at`, `started_at`, `finished_at` | lifecycle timestamps; `updated_at` is maintained by a trigger |

`deployments_app_id_commit_sha_idx` on `(app_id, commit_sha, id)` serves the enqueue duplicate check and the "first seen" lookup batching uses to order commits.

The partial index `deployments_app_migration_idx` on `(app_id, id) WHERE migration AND kind = 'deploy'` serves `MigrationsBetween`.

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

The append-only audit of freeze locks: `app_id` (null for the global freeze), `action` (`locked`, `unlocked`, `expired`, `reminded`, `core.LockActions`), `owner` of the lock, `actor` (who unlocked it; empty for system actions), `reason`, `expires_at` and `occurred_at`. Indexed by `(app_id, id DESC)`. Each row is written in the same statement or transaction as the lock change it records. Who asked, with their role, is additionally in `audit_log` (`lock.acquire`, `lock.release`) for locks and releases, but not for expiry or reminders. It is separate from `deployment_events`, which requires a deployment, because a freeze belongs to an app or to everything rather than to one deployment.

- **Retention.** Rows are kept indefinitely, like `deployment_events`. A freeze changes a handful of times a day at most, so the table stays small; append-only is a property of the API, so a later retention job can prune old rows without a schema change.
- **Deleting an app.** `app_id` references `apps` without an `ON DELETE` action, the same as `deployments`, `queue_entries` and `locks`. doorstep never deletes `apps` rows: removing an app from the configuration file leaves its row, history and audit in place. Deleting a row by hand fails while anything, including its lock audit, refers to it, so the audit cannot disappear by accident.

### `gate_results`

The latest result of each named gate for a deployment: `UNIQUE (deployment_id, name)`, upserted on re-evaluation. The commit a result belongs to is the deployment's `commit_sha`; it is not copied, and a rollback of the same commit gets its own rows. Gate names are unique across both phases of an app, so the phase is an attribute, not part of the key. Gate semantics are described in [gates.md](gates.md).

| Column | Notes |
| --- | --- |
| `deployment_id` | `ON DELETE CASCADE` |
| `name` | configured gate name, not blank |
| `type` | gate type from the configuration (`checks`, `health`, `soak`, ...), not blank; no enumeration `CHECK`, because new gate types should not need a migration |
| `phase` | `before` or `after` |
| `status` | `pending`, `passed`, `failed` or `skipped` |
| `message` | short explanation, at most 1024 bytes as written by the evaluator |
| `evidence` | `jsonb` object, `{}` when there is none |
| `started_at` | set on insert, and reset to `now()` when a result moves from a verdict back to `pending`, so a gate timeout restarts |
| `finished_at` | set to `now()` when the status becomes a verdict, kept while the same verdict is recorded again, cleared while `pending` |
| `updated_at` | maintained by the trigger on every recording, including an unchanged `pending` poll; the gate timeout compares it with `started_at`, so both readings come from the database clock |

`Store.RecordGateResult` cleans the message and validates the evidence with the same limits as the evaluator ([gates.md](gates.md#limits)), then, in one transaction, inserts the row (`ON CONFLICT DO NOTHING`) or, when it exists, locks it with `SELECT ... FOR UPDATE`, reads the previous status and updates it. Concurrent writers of one gate are therefore serialised and exactly one of them sees each change of status. When the new status is final and a `gate_reports` row for the gate holds a different final verdict, the update is refused with `core.ErrGateReported`, so a timeout computed before a report arrived cannot overrule it. In the same transaction it appends a `gate` event to `deployment_events` whenever the status changed (including the first recording), with the message `<name> <status>` and `data` `{"gate", "type", "phase", "status"}`. An unchanged `pending` poll updates the row but writes no event. It returns the stored row and whether the status changed, and `core.ErrNotFound` for an unknown deployment. `Store.ListGateResults` returns a deployment's rows in creation order.

Migration `00006_gate_result_phase.sql` added `type` and `phase`. Both are `NOT NULL` with no default, so every writer must state them; the temporary defaults used to add the columns are dropped in the same migration.

### `gate_reports`

Results posted by other systems for gates of type `external` ([gates.md](gates.md#external)), one row per deployment and gate: `UNIQUE (deployment_id, gate)`. Only the API writes it; the `external` gate reads it and the evaluator turns it into the `gate_results` row, so the evaluator stays the only writer of gate results.

| Column | Notes |
| --- | --- |
| `deployment_id` | `ON DELETE CASCADE` |
| `gate` | configured gate name, not blank |
| `status` | `pending`, `passed` or `failed` |
| `message` | cleaned like gate messages |
| `url` | absolute `http` or `https` link, at most 2048 bytes, or empty |
| `details` | `jsonb` object of at most 32 KiB, `{}` when there is none |
| `reported_by` | the reporting actor (`token:ci-e2e`), not blank |
| `created_at`, `updated_at` | first report and latest change |

`Store.SaveGateReport(ctx, report, audit)` validates the report, then in one transaction inserts the row (`ON CONFLICT DO NOTHING`) or locks the existing one with `SELECT ... FOR UPDATE`. A `pending` report may be replaced; a `passed` or `failed` report is final, so the same verdict again returns the stored row unchanged and a different one returns `core.ErrGateDecided`. It also returns `core.ErrGateDecided` when the deployment's `gate_results` row for that gate already has a verdict; it reads that row with `SELECT ... FOR UPDATE`, so a report and a concurrent `RecordGateResult` of the same gate are serialised and cannot both decide it. The `gate.report` audit row is written in the same transaction, and only when the stored report actually changed. An unknown deployment is `core.ErrNotFound`. `Store.GetGateReport` reads one row, and `Store.ActiveDeploymentForCommit(ctx, target, sha)` returns the newest unfinished deployment of a target whose commit is `sha`, for the API's commit form.

### `deployment_events`

The append-only history: `deployment_id`, denormalised `app_id`, `type` (`transition`, `gate`, `lock`, `note`), optional `from_state` and `to_state`, `actor`, `message`, `data jsonb`, `occurred_at`. Indexed by `(deployment_id, id)` for a deployment's timeline and `(app_id, id DESC)` for a target's recent activity. Append-only is a property of the API (there are no update or delete methods), not of triggers, so retention pruning stays possible. Creating a deployment writes its first `transition` event (to `queued`, without `from_state`) in the same transaction; every later transition records both `from_state` and `to_state`. `from_state` equals `to_state` only for `syncing → syncing`, the engine's automatic retry of the same pin, whose `data` carries a `retry` object ([engine.md](engine.md#retry-before-rollback)); there is no check constraint on the pair, so retries needed no migration. Lineage written outside a transition is recorded as a `note` event with the message `lineage recorded`.

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
- **Blocked commits.** `enqueue` runs in a savepoint. When it returns an error matching `core.ErrBlocked` (intake found an active block on the commit), the savepoint is rolled back and the settlement commits with the evaluation saved and nothing enqueued, so the commit stays open; any other error rolls everything back.
- **Settlement.** `SettleTrackedCommit(ctx, id, evaluation, enqueue)` locks the app row and then the tracked row and does nothing for a closed commit. It saves `checks_state`, `checks` and `evaluated_at` only when `evaluated_at` is null or older than the evaluation's read time, so an older snapshot never overwrites a newer one. When the stored state is then `success`, it calls `enqueue` with a store bound to the same transaction and stores the deployment id and `enqueued_at`. The evaluation, the deployment, its queue entry and the link commit together or not at all, so concurrent settlements create exactly one deployment and never deploy on a stale snapshot. Locking the app row first, in the same order as `TrackCommit`, keeps a push and a settlement from deadlocking; whichever commits first wins, and the other sees the commit closed.
- **Open commits.** `OpenTrackedCommits(ctx, followed, limit)` lists open commits of the given app, repository (case-insensitive) and branch triples, least recently evaluated first, so rows of apps that are no longer configured never take the poller's slots.

### `blocks`

Added by migration `00008_blocks.sql`. One row per blocked version of an app, written by `internal/rollback` ([rollback.md](rollback.md#blocks), [ADR 0009](decisions/0009-blocks-in-postgres-mirrored-to-git.md)).

| Column | Notes |
| --- | --- |
| `app_id` | the target |
| `digest` | the blocked image digest (`sha256:` and 64 lowercase hex), or null |
| `commit_sha`, `source_repository` | the blocked app commit (40 lowercase hex) and its repository when known; `commit_sha` may be null. At least one of `digest` and `commit_sha` is set (`blocks_subject_check`) |
| `reason`, `created_by` | why and by whom; `created_by` is an actor string and never blank |
| `deployment_id`, `rollback_id` | the deployment that was rolled back and the rollback deployment, for blocks a rollback created; `ON DELETE SET NULL` |
| `created_at` | `now()` at insert |
| `block_commit_sha` | the GitOps pin commit that carries this block's `Doorstep-Block` trailer; null until mirrored |
| `cleared_at`, `cleared_by` | set together when the block is cleared (`blocks_cleared_check`) |
| `unblock_commit_sha` | the pin commit that carries `Doorstep-Unblock`; only on cleared rows (`blocks_unblock_check`) |

A block is active while `cleared_at` is null; cleared rows stay as history. Partial unique indexes allow one active block per app and digest (`blocks_active_digest_key`) and one active commit-only block per app and commit (`blocks_active_commit_key`); `blocks_active_commit_idx` serves the lookup by commit that intake makes for every green commit. `CreateBlock` locks the app row and extends a matching active block (filling in a missing digest, commit or rollback) rather than adding a second one. `ClearBlocks` clears every active block that matches a digest or a commit. `ActiveBlock` and `Blocked` look blocks up by digest or commit. `PendingBlockTrailers` lists the digests whose block or unblock has not reached git yet, and `MarkBlocksMirrored` records the pin commit that carried them.

### `api_tokens`

One row per API token ([authentication.md](authentication.md#api-tokens)). `name` is unique and matches `^[a-z0-9][a-z0-9._-]{0,62}$`; `token_hash` is the 32-byte SHA-256 of the token and unique, and the token itself is never stored. `role` is a `core.Role`. `created_by` is the creating actor (`cli:root`). `expires_at` is null for tokens that never expire; `last_used_at` is updated at most once a minute; `revoked_at` marks a revoked token, whose row is kept so its name cannot be reused. Lookup is an equality match on `token_hash` that also requires `revoked_at IS NULL` and an unexpired `expires_at`.

Migration `00010_gate_reports.sql` added the `reporter` role to the `role` checks of `api_tokens` and `auth_sessions` and to `audit_log.actor_role`.

### `auth_sessions`

One row per signed-in browser session. `token_hash` is the 32-byte SHA-256 of the session cookie and unique. The identity snapshot is `provider` (`oidc` or `github`), `subject` (the provider's stable ID), `username`, `name` and `email`, and `role` is the role decided at sign-in. `expires_at` is set from `auth.session_ttl` at creation and never extended; expired rows are ignored by lookups and deleted when a new sign-in starts. Deleting a row signs that session out immediately.

### `auth_logins`

One row per sign-in in progress, keyed by the 32-byte SHA-256 of its `state` (`state_hash`), with the `nonce`, the PKCE `pkce_verifier`, the path-only `return_to` and an `expires_at` 10 minutes after creation. The callback consumes it with `DELETE ... RETURNING`, so each state works once.

### `audit_log`

Who asked doorstep to do what ([authentication.md](authentication.md#audit-log)):

| Column | Notes |
| --- | --- |
| `occurred_at` | `now()` at insert |
| `actor_kind`, `actor_id`, `actor_name` | `core.Actor`: kind `user`, `token`, `cli` or `system`; `actor_id` is empty only for `system` |
| `actor_role` | the actor's `core.Role` when known, else null |
| `action` | dot-separated lowercase words (`^[a-z_]+(\.[a-z_]+)+$`), such as `deploy.request`; the known actions are constants in `core` |
| `app`, `environment` | the target, both set or both null |
| `deployment_id` | the deployment the action concerns, if any |
| `detail` | JSON object with action-specific details |

There are deliberately no foreign keys, so audit rows outlive the apps and deployments they mention. Indexes serve the newest-first listing (`occurred_at DESC, id DESC`), a target's activity (`app, environment, id DESC`), an actor's activity (`actor_kind, actor_id, id DESC`) and lookups by `deployment_id`. Unlike `deployment_events`, append-only is enforced by triggers: `audit_log_reject_change()` raises on every `UPDATE`, `DELETE` and `TRUNCATE`. A retention policy, if one is ever needed, is a deliberate migration that replaces the trigger.

### `webhook_deliveries`

GitHub `push` deliveries that doorstep has claimed, so a redelivery is answered without doing the work again ([github-app.md](github-app.md#webhooks)). `delivery_id` (the `X-GitHub-Delivery` header, 1 to 128 characters) is the primary key, with the `event` name, `claimed_at` and `completed_at` (null while the delivery is being handled; never before `claimed_at`).

- **Claim, complete and release.** `ClaimWebhookDelivery(ctx, id, event, staleAfter)` inserts the row, or takes over an existing row that was never completed and was claimed at least `staleAfter` ago (the attempt that claimed it crashed). It reports `claimed`, `completed` (a duplicate) or `in_progress` (another attempt is handling it). After the work, `CompleteWebhookDelivery` sets `completed_at`; on failure `ReleaseWebhookDelivery` deletes the row unless it is completed, so a manual redelivery is processed. The webhook handler passes its timeout plus ten seconds as `staleAfter`. It runs after the signature check and only for `push`, so pings, commit status events, ignored events and installation events are never stored. Tracking and settlement are exactly-once on their own (see [`tracked_commits`](#tracked_commits)), so this table only saves GitHub calls.
- **Retention.** `DeleteWebhookDeliveriesBefore(ctx, age)` deletes rows whose `claimed_at` is older than `now() - age`, served by an index on `claimed_at`. `serve` runs it hourly with a seven-day age whenever the webhook route is mounted; GitHub allows redelivery for about three days.

### `intake_poll_state`

A single row (`id boolean PRIMARY KEY DEFAULT true CHECK (id)`, inserted by the migration) whose `next_poll_at` (initially `-infinity`) is when the next intake poll cycle may start. `ClaimPoll(ctx, lease)` runs `UPDATE ... SET next_poll_at = now() + lease WHERE next_poll_at <= now() RETURNING next_poll_at`, and only the replica that gets the row runs the cycle, so replicas poll GitHub once per interval between them without holding a lock across GitHub calls. When the cycle ends, `FinishPoll(ctx, leasedUntil, every)` sets `next_poll_at = now() + every`, but only while the row still holds that replica's lease, so the interval is measured from the end of a cycle and a cycle that outlived its lease cannot reschedule another replica's. A replica that dies mid-cycle delays the next cycle until its lease expires.

### `schema_compatibility`

A single row (`singleton boolean PRIMARY KEY DEFAULT true`, `schema_compatibility_singleton_check CHECK (singleton)`) holding `min_binary_schema`, the compatibility floor: the oldest newest-migration a binary may embed and still start against this database in compatibility mode (`schema_compatibility_min_binary_schema_check`, positive). Migration `00012` creates it with `12`; contract migrations raise it ([Compatibility across one release](#compatibility-across-one-release)). It is read only when the database is ahead of the binary.

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
- `Store.LastSuccessfulDeployment(ctx, target, excludeDigest)` returns the `succeeded` deployment of the target that finished last (`finished_at DESC, id DESC`) and has a resolved `image_digest`, or `core.ErrNotFound`. A succeeded deployment without a digest is skipped, because a rollback must pin a digest. Ordering by finish time matters because a high-priority rollback queued later can finish before an older deploy. A succeeded rollback counts: it is what is running. A non-empty `excludeDigest` skips deployments with that digest, so the automatic rollback of a failed deploy and an operator rollback of a succeeded one (skipping itself and any retry of the same bad digest) share one lookup. A digest with an active block (`blocks`) is skipped too, so a rollback never picks a blocked version. Git remains authoritative for the previous digest ([ADR 0001](decisions/0001-go-and-core-design.md)); this lookup supplies the commit and image fields for the rollback deployment.
- `Store.CurrentDeployment(ctx, target)` returns the version a rollback replaces: the newest deployment of the target by `COALESCE(started_at, created_at) DESC, id DESC` that is in an active state, `succeeded`, or `failed` or `cancelled` with a `pin_commit_sha`, or `core.ErrNotFound`. Queued and superseded deployments and failures that never pinned are ignored.
- `Store.CurrentDeployments(ctx)` returns the same deployment for every target that has one, and `Store.LatestDeployments(ctx)` the newest deployment of every target by `id`, each in one query (a lateral join per app row that uses the `app_id` indexes), ordered by app name and environment. The web overview uses them so a poll costs the same number of queries however many apps are configured.
- `Store.MigrationsBetween(ctx, target, to, current)` returns the flagged `deploy` rows a rollback from `current` to `to` crosses, oldest first and at most 20: `migration` set, `commit_sha` not `to`'s commit, `id` greater than the first-seen id of `to`'s commit (the smallest id of the target's `deploy` rows with that commit) and at most `current.id`. Rollback rows never count. Called on the transaction-bound store by rollback requests and, for automatic rollbacks, by the engine.
- `Store.LastDeployCommit(ctx, target)` returns the commit of the target's newest `deploy` row by id, whatever its state, and whether there is one. Intake uses it as the base of migration detection.
- `Store.UnreleasedMigrations(ctx, target)` returns the ids, oldest first and at most 20, of the target's flagged `deploy` rows that are newer than its last `deploy` row that reached the cluster (`succeeded`, or with a `pin_commit_sha`) and have not reached it themselves. Intake carries their flags into the next release. It uses `deployments_app_migration_idx`.
- `Store.LockTarget(ctx, target)` creates the target's `apps` row if needed and locks it (`FOR NO KEY UPDATE`) for the rest of the caller's transaction, the same lock enqueueing, intake and freezes take.

Coverage today: the [deployment engine](engine.md) moves deployments through `TransitionDeployment` and records `pin_commit_sha`, `previous_digest`, `gitops_repository` and `argocd_revision` with its transitions (and `SetLineage` when a rollback pins again), and the gate evaluator writes `gate_results`. `Store.TimeInState(ctx, id, state)` returns how long ago a deployment last entered a state, as `now() - occurred_at` of the newest `transition` event to that state, computed in Postgres (`core.ErrNotFound` when there is none); the engine measures its retry deadlines and the remaining sync time with it, on the database clock. A `syncing → syncing` retry therefore restarts the sync deadline. `Store.RetryState(ctx, id)` returns the number of a deployment's `syncing → syncing` events carrying `retry`, and the newest one's `retry.delay_ms` and `retry.max_attempts`, which the engine subtracts and reports. `Store.Diagnoses(ctx, id, window, limit)` returns the `data->'diagnosis'` objects of the deployment's own transition events (oldest first) and, separately, those of earlier deployments of the same target with a different `image_digest` recorded within `window` (default 7 days, at most 50 rows), using the `(app_id, id DESC)` index; the retry policy uses them as evidence that a node, not a release, is at fault. The trigger is recorded as `source` and `requested_by`; a reference to what triggered it (a webhook delivery id or a pull request) is not stored yet.

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
- `QueueEntries(ctx, app)` lists the claimable entries of every environment of an app by name, ordered per environment by the claim rank (treating a live lease as in flight), with 1-based positions and the lease expiry only while it is live. An empty `app` lists every app, ordered by app name and then environment, with positions counted per app and environment; the web overview reads the whole queue this way in one query. `QueueDepth` counts entries whose deployment is `queued` and not leased.

### Wake-ups

`Store.ListenQueue(ctx, wake, logger)` opens one dedicated connection with the pool's settings (it uses no pool capacity), reads `current_schema()`, runs `LISTEN doorstep_queue` and calls `wake` for every notification whose payload is that schema, so several installations or test schemas in one database do not wake each other. It also calls `wake` after every successful `LISTEN`. When the connection fails it reconnects with backoff from 1 to 30 seconds and returns only when the context ends. Workers poll as well, so notifications are a latency hint only and transaction-mode poolers that drop `LISTEN` still work.

## Clock

Row timestamps, lease expiry, lock expiry and reminders, webhook delivery retention, the intake poll claim and every eligibility comparison use the database's `now()`. Several replicas must agree on when a lease expires, and the database is the only clock they share. This is the one exception to the injected `Clock` rule in [architecture.md](architecture.md#dependency-rules): the engine still uses an injected clock for its own timing, and passes lease lengths to the store as durations. Tests force a lease to expire by updating `lease_expires_at` directly.

## Testing

Integration tests use `internal/store/storetest`, which gives each test its own schema; see [development.md](development.md#integration-tests).

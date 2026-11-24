# Database

Postgres holds doorstep's operational state: the deploy queue, locks, deployment history and gate results. Git remains the source of truth for what should run (see [ADR 0001](decisions/0001-go-and-core-design.md), decision 3); nothing in the cluster is ever reconciled from this database.

The code lives in `internal/store` (pgx pool, transactions, migrations and repositories) with shared domain types in `internal/core`.

## Supported versions and privileges

- Postgres 15 or newer. doorstep is developed and tested on Postgres 18 (CI runs `postgres:18-alpine`). The schema uses nothing newer than identity columns, partial indexes and `SKIP LOCKED`.
- The role in the connection URL needs `CONNECT` on the database and `USAGE` and `CREATE` on the schema doorstep uses (normally `public`, or the first entry of `search_path`). It owns every table it creates, so no further grants are needed. It does not need `SUPERUSER`, `CREATEDB` or `CREATEROLE`.
- Advisory locks (`pg_advisory_lock`) must be available; they are on every standard Postgres.

The connection is configured with `DOORSTEP_DATABASE_URL` or `DOORSTEP_DATABASE_URL_FILE`; see [configuration.md](configuration.md#database).

## Migrations

Migrations are plain SQL files embedded in the binary (`internal/store/migrations`) and applied with [goose](https://github.com/pressly/goose) through its `Provider` API. goose records applied versions in a `goose_db_version` table in the same schema.

- **Forward-only.** Files contain only a `-- +goose Up` section. To undo a change, write a new migration. Downgrading doorstep across a schema change is not supported.
- **Numbering.** Files are named `NNNNN_description.sql` with a zero-padded, strictly increasing version (`00001_initial_schema.sql`, `00002_...`). Out-of-order versions are rejected.
- **Transactions.** Each file runs in its own transaction, so a failed migration leaves the schema at the previous version.
- **When they run.** `doorstep serve` applies pending migrations after connecting and before it opens its listener, so an instance is never ready on an old schema. `doorstep migrate` applies them on demand (for example as a Kubernetes Job before a rollout) and `doorstep migrate status` lists applied and pending versions.
- **Concurrent starts.** Migration runs hold a Postgres session advisory lock, so several replicas starting together apply each migration exactly once; the others wait (retrying every second, for up to two minutes) and then find nothing pending.
- **Newer schema.** After migrating, doorstep compares the database version with the newest migration it embeds. If the database is newer (an older binary after a downgrade), startup fails with `database schema is newer than this doorstep binary` instead of running against a schema it does not understand.

### Adding a migration

Each feature that needs storage adds its own numbered file in `internal/store/migrations` and the repository methods that use it. Keep these rules:

- Never edit a migration that has been merged; add a new one.
- Use `text` columns with a named `CHECK` constraint for enumerations, and add the value to the matching `core` constant list in the same change. The schema tests insert every `core` value, so Go and SQL cannot drift.
- Name constraints and indexes explicitly (`<table>_<column>_check`, `<table>_<columns>_idx`) so errors and later migrations can refer to them.
- Prefer operations that do not rewrite or long-lock large tables (`CREATE INDEX CONCURRENTLY` needs its own non-transactional migration with `-- +goose NO TRANSACTION`).

## Schema

All ids are `bigint GENERATED ALWAYS AS IDENTITY`. Timestamps are `timestamptz`.

### `apps`

One row per deploy target, `core.Target{App, Environment}`. It holds identity only: `name`, `environment`, `created_at`, with `UNIQUE (name, environment)`. Repository, GitOps path and strategy live in the configuration file. Rows are never deleted, so history survives an app being removed from configuration.

### `deployments`

One row per deployment request, keyed by a store-assigned `id` (`core.DeploymentID`).

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
| `parent_id`, `rollback_of_id`, `superseded_by_id` | lineage: retry of, rollback of, superseded by; `ON DELETE SET NULL` |
| `failure_reason` | human-readable reason for `failed` or `rollback_failed` |
| `created_at`, `updated_at`, `started_at`, `finished_at` | lifecycle timestamps |

The partial unique index `deployments_one_active_per_app` on `app_id`, restricted to the active states (`resolving`, `pinning`, `syncing`, `verifying`, `rolling_back`), enforces at most one active deployment per target regardless of what the queue does.

### `queue_entries`

The claim table. A row exists from enqueue until the deployment needs no more work; the queue code removes it then.

| Column | Notes |
| --- | --- |
| `deployment_id` | primary key, `ON DELETE CASCADE` |
| `app_id` | the target, denormalised for the head-of-line check |
| `priority` | higher runs first; rollbacks are enqueued with a higher priority |
| `enqueued_at`, `available_at` | `available_at` in the future delays the entry (backoff) |
| `lease_owner`, `lease_expires_at` | both null, or both set |
| `attempts` | incremented on every claim |

### `locks`

`(app_id, kind)` is unique, with `kind` `deployment` or `freeze`, so one deployment lock and one manual freeze can coexist on a target. Columns: `owner`, `reason`, optional `deployment_id` (`ON DELETE CASCADE`), `created_at` and optional `expires_at`. Released locks are deleted; the audit trail is in `deployment_events`.

### `gate_results`

The latest result of each named gate for a deployment: `UNIQUE (deployment_id, name)`, upserted on re-evaluation. Columns: `status` (`pending`, `passed`, `failed`, `skipped`), `message`, `evidence jsonb`, `started_at`, `finished_at`, `updated_at`. Earlier evaluations are recorded as `gate` events.

### `deployment_events`

The append-only history: `deployment_id`, denormalised `app_id`, `type` (`transition`, `gate`, `lock`, `note`), optional `from_state` and `to_state`, `actor`, `message`, `data jsonb`, `occurred_at`. Indexed by `(deployment_id, id)` for a deployment's timeline and `(app_id, id DESC)` for a target's recent activity. Append-only is a property of the API (there are no update or delete methods), not of triggers, so retention pruning stays possible. Creating a deployment writes its first `transition` event (to `queued`) in the same transaction.

## Transactions

`Store.InTx(ctx, func(tx *store.Store) error)` runs the callback in a `READ COMMITTED` transaction and hands it a `Store` bound to that transaction, so every repository method works inside or outside a transaction. The transaction commits when the callback returns nil and rolls back on an error or a panic. Calling `InTx` on a transaction-bound store creates a savepoint. There is no automatic retry. `Migrate`, `MigrationStatus` and `Ping` are only available on the root store.

## Claiming work

`Store.Claim(ctx, workerID, lease)` picks the next deployment and leases it to the worker in one statement (a CTE with `FOR NO KEY UPDATE ... SKIP LOCKED` feeding an `UPDATE ... RETURNING`), then loads the deployment in the same transaction. An entry is eligible when:

- `available_at <= now()`;
- it has no live lease (`lease_expires_at` is null or in the past);
- it is the head of its target: no other entry for the same app has a higher priority, or the same priority and a lower id;
- no other entry for the same app holds a live lease.

Eligible entries are ordered by `priority DESC, deployment_id`. Claiming sets `lease_owner`, sets `lease_expires_at = now() + lease` and increments `attempts`; the result carries the attempt number and lease expiry.

The head-of-line rule serialises work per target without a second mechanism. The live-lease rule stops a high-priority rollback entry from being claimed while the failing deployment is still leased. An expired lease makes its entry claimable again, so a crashed replica never wedges a target.

`SKIP LOCKED` also locks the target's `apps` row, so concurrent claimers skip a target another worker is claiming. Because a `READ COMMITTED` statement works from a snapshot taken when it starts, a claim can still race with one that committed a moment earlier; after claiming, a second statement (with a fresh snapshot, while the `apps` row is locked) checks that no other entry for the target holds a live lease, and if one does, the claim is rolled back and retried. The partial unique index on active deployments remains the final guard.

Freeze locks do not yet affect claiming; the locks work adds that as another condition.

## Clock

Row timestamps, lease expiry and every eligibility comparison use the database's `now()`. Several replicas must agree on when a lease expires, and the database is the only clock they share. This is the one exception to the injected `Clock` rule in [architecture.md](architecture.md#dependency-rules): the engine still uses an injected clock for its own timing, and passes lease lengths to the store as durations. Tests force a lease to expire by updating `lease_expires_at` directly.

## Testing

Integration tests use `internal/store/storetest`, which gives each test its own schema; see [development.md](development.md#integration-tests).

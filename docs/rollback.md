# Rollback and blocks

A rollback puts an app back on its previous good image digest. It is not a special mode: doorstep queues a new deployment of kind `rollback` that re-pins the earlier digest through the same queue, pin, Argo CD and verification path as any deploy ([ADR 0002](decisions/0002-rollback-is-a-new-deployment.md)), and it records a **block** on the version it replaced so that version is not deployed again automatically. Blocks live in Postgres and are mirrored into the GitOps repository as commit trailers ([ADR 0009](decisions/0009-blocks-in-postgres-mirrored-to-git.md)).

In the MVP a rollback is requested from the API or the CLI. The UI button comes later. `internal/rollback` implements everything on this page; the [deployment engine](engine.md#rollbacks) executes the queued rollback deployment.

## Requesting a rollback

```sh
doorstep rollback blog
doorstep rollback blog --to 41 --reason "checkout errors"
doorstep rollback blog --to 41 --confirm-migration-rollback
```

One request does all of the following in one Postgres transaction, under the app's row lock (the lock intake and the queue use for the same app), so it either happens completely or not at all:

1. **Refuse duplicates.** If a rollback of the app is already queued or in flight, the request fails with `409 rollback_in_progress` and names that rollback. Double clicks are safe.
2. **Find the current version** (the one being rolled back). It is the newest deployment of the app, by start time (creation time while not started), that is in flight, `succeeded`, or `failed`/`cancelled` after its pin commit landed. Queued and superseded deployments, and failures that never pinned, did not change what runs and are ignored. A deploy stuck in `verifying`, or one that failed after pinning, is exactly what operators roll back. If nothing qualifies the request fails with `409 no_rollback_target`.
3. **Pick the target.**
   - By default: the last `succeeded` deployment of the app (latest `finished_at`) that has a digest, whose digest differs from the current one and is not blocked. Successful rollbacks count, since they are what ran.
   - `--to <deployment-id>` (API `to_deployment_id`), or `to_digest` (API only, the newest succeeded deployment with that digest): the deployment must belong to the same app, be `succeeded`, have a digest, differ from the current digest and not be blocked. A blocked target fails with `409 blocked`; anything else fails with `400 invalid_request`.
   - Postgres history picks the target because only history knows a digest passed its gates; git still decides what is actually pinned when the rollback runs.
   - **Check for migrations.** When the rollback would cross a release flagged as containing database migrations and the request does not confirm it, the request fails with `409 migration_confirmation_required` and writes nothing: no cancel, no block, no audit row. See [Migration-aware releases](#migration-aware-releases).
4. **Cancel what the rollback replaces.** A current deployment that is still in flight is moved to `cancelled` with a compare-and-set. Queued deploys of the blocked commit are cancelled too. Other queued deploys (newer merges) are left alone and run after the rollback; to stop them as well, freeze the app with `doorstep lock`.
5. **Queue the rollback** at priority `rollback` (200), ahead of everything else for the app: kind `rollback`, `rollback_of_id` set to the current deployment, source `api`, `requested_by` the caller, the commit and image copied from the target, and the digest preset to the target's digest. A confirmed rollback across migrations is itself flagged (`migration: true`, reason `rolls back across deployments 44, 46`), so the history shows the risk and a `backup` gate with `apply_to_rollbacks` runs before it. The engine therefore never resolves a tag for a rollback and pins exactly the previous digest even if the tag moved. The rollback is always queued, even when the target digest is already pinned: the pin is then a no-op, and the rollback still waits for Argo CD to report synced and healthy and leaves a history row.
6. **Block the replaced version** (see below) with the reason given, or `rolled back by <actor>`.
7. **Audit** a `rollback.request` row naming the rollback deployment, with `rolled_back_deployment_id`, `to_deployment_id`, `digest`, `blocked_digest`, `blocked_commit`, `block_id`, `reason` and `cancelled` in its detail, plus `migration_confirmed: true` and `migration_deployment_ids` when the rollback crossed confirmed migrations.

If another writer changes one of the deployments between reading and cancelling it, the whole transaction is retried, at most three times; after that the request fails with `503 contended` and `Retry-After: 1`.

The `doorstep_rollbacks_total{app, reason="manual"}` counter is incremented for every accepted request ([metrics.md](metrics.md)).

## Blocks

A block says "do not deploy this version of this app automatically". It is keyed on the image digest, the app commit SHA, or both:

- A rollback blocks the current deployment's digest and, unless it equals the target's commit, its commit SHA. A deploy cancelled before its digest was resolved gets a commit-only block. A cancelled rebuild of the target's own commit that never resolved blocks nothing.
- `doorstep block <app> <digest|commit> --reason <text>` blocks by hand. The reason is required.
- Lookups match either key: a block on a digest stops that digest, a block on a commit stops that commit.
- An app has at most one active block per digest and one active commit-only block per commit. Blocking a version that already has an active block extends that block: a digest is added to a commit-only block of the same commit, and a commit is added to a digest block that has none. When the digest is already blocked together with a different commit (two commits built the same image), the new commit gets its own commit-only block instead, so both commits stay blocked; a rollback then reports and audits that commit-only block, since the digest was already blocked.

Blocks never stop rollbacks. They stop automatic deploys at two places:

- **Intake** refuses a green commit that has an active block on its commit SHA. The check runs inside the same transaction that would enqueue the deploy ([architecture.md](architecture.md#merge-and-ci-detection)), which takes the app row lock before it reads blocks, so it cannot race with a rollback request: either the rollback commits first and intake sees its block, or intake queues first and the rollback cancels that queued deploy. The evaluation is saved, nothing is queued, the tracked commit stays open and `intake.Result.Blocked` names the block; intake logs `commit is blocked; not enqueued`.
- **The engine** refuses a deploy whose resolved digest is blocked, after resolving and before pinning, and fails the deployment with a reason naming the block. This covers deploys requested by other paths and rebuilt images of an unblocked commit. Rollbacks skip the check.

A rollback target is never a blocked digest, and the default target selection skips blocked digests, so "v3 succeeded, v3 rolled back, then v2 rolled back" picks v1, not the blocked v3.

### Unblocking

```sh
doorstep blocks blog
doorstep unblock blog sha256:4f1c...
doorstep unblock blog 0123456789abcdef0123456789abcdef01234567
```

`unblock` clears every active block that matches the digest or commit given (a commit can have several digest blocks), records who cleared it and writes a `block.clear` audit row. Clearing does not deploy anything. A commit that intake refused stays open, so once unblocked it is queued by the next check event or poll while it is still the branch head and green. To deploy an older version explicitly, roll back to it with `--to`, or push it again.

`doorstep block` on a version that is already blocked returns the existing block (`200` instead of `201`) and writes no audit row: a request names a single digest or commit, so it never changes the existing block.

### Blocks in git

Postgres is the operational source of truth: a block or unblock takes effect in the request's transaction, whatever happens in git. Git is an eventually consistent mirror, so the GitOps history alone still tells which digests must not be deployed:

- A digest block that has not been written to git yet (`block_commit_sha` is null), and a cleared digest block whose block reached git but whose unblock has not (`unblock_commit_sha` is null), are **pending trailers** of the app.
- Before every pin of the app, the engine reads the pending trailers and adds them to the pin commit as `Doorstep-Block` and `Doorstep-Unblock` ([gitops.md](gitops.md#commit-messages)). After a pin that wrote a commit (`Changed`), it marks them mirrored with that commit's SHA.
- So a rollback's own pin commit carries `Doorstep-Block` for the digest it replaces, as [ADR 0001](decisions/0001-go-and-core-design.md) describes, and an unblock usually travels on the pin commit of the next deploy of that app.
- When the rollback target is already pinned, the pin writes no commit and the block stays pending until the app's next pin commit. No empty commits are made, so this works in pull request mode and under branch protection.
- A digest that is cleared and blocked again before either reached git only produces `Doorstep-Block`; the stale unblock is settled with it. Commit-only blocks are not mirrored: the trailer vocabulary has no commit form.

The lag is harmless for unblocks: until the unblock reaches git, git still says "blocked", which is the safe side. A block that never reaches git (because the app is never pinned again) exists only in Postgres; restoring doorstep from git alone would lose it. An explicit flush with an empty commit and a command that rebuilds blocks from the git history are possible follow-ups.

## Migration-aware releases

A release can contain database migrations. Rolling such a release back puts old code on a migrated schema, which may not work, so doorstep makes that an explicit decision. It is opt-in per app ([configuration.md](configuration.md#migrations)); the design is recorded in [ADR 0015](decisions/0015-migration-aware-releases.md).

### Detection

```yaml
apps:
  - name: blog
    migrations:
      labels: [db-migration]
      trailer: true
```

A commit counts as a migration when

- a pull request returned by GitHub's `commits/{sha}/pulls` for it is **merged**, has the app's branch as its base, and carries one of `labels` (compared case-insensitively). Open pull requests that merely contain the commit, and pull requests into other branches, do not count; or
- `trailer` is on (the default when the block exists) and its message has a line `Doorstep-Migration: true` or `Doorstep-Migration: yes` anywhere, case-insensitively. Leading list markers and trailing whitespace are tolerated, so the line survives squash-merge message rewriting.

The flag describes the **whole release**, not just its head commit. Intake drops stale pushes and supersedes commits that never went green, so a migration merged seconds before another pull request may never get a deployment of its own; checking only the head would miss it exactly when merges are busy. Intake therefore inspects every commit between the app's previously enqueued deploy commit (its newest `deploy` row) and the new head with one compare call (at most 50 commits), plus one `commits/{sha}/pulls` call per commit when labels are configured:

| Situation | What is inspected |
| --- | --- |
| no earlier deploy, or the head is the earlier commit | the head commit only |
| compare reports `behind` or `identical`, or answers `404` (force push, deleted history) | the head commit only |
| more than 50 commits | nothing: the release is flagged conservatively with `52 commits since the last release, too many to inspect` |
| otherwise | every commit in the range |

Detection runs in intake when a commit's checks become `success`, the last moment before it is queued, outside the Postgres transaction so rollback requests never wait on GitHub. Reads are shared within one evaluation pass. If GitHub cannot be read, the commit is **not** settled or queued: intake logs `reading migration flags failed; not enqueued` and the next check event or poll (at most a minute later) tries again. An operator who opted in wants the guard to be reliable, so a deploy with unknown migration status never slips through.

The result is stored on the deployment: `migration` and `migration_reason` (at most 200 characters), for example `label db-migration on #12 (abc1234); trailer Doorstep-Migration in def5678`. The API, the history and the backup gate read it from there. Deployments that were batched away (`superseded`) keep their flag, so batching loses nothing. Deploys requested through other paths are not inspected and are unflagged.

### What counts as crossing

A rollback from the current deployment to a target **crosses** a migration when a `deploy` row of the app

- is flagged,
- was created after the target's commit was first enqueued (the smallest id of the `deploy` rows with that commit),
- is not of the target's commit, and
- is no newer than the current deployment.

`(*store.Store).MigrationsBetween` answers this in one query inside the rollback transaction, at most 20 rows, oldest first. Using "first enqueued" ages commits the way the queue does, which keeps the answer right through batching, earlier rollbacks and re-enqueues. For example:

1. v1 deploys; v2 (flagged) deploys; rolling back to v1 crosses v2.
2. After that rollback (to v1), v3 deploys. Rolling back to v1 again still crosses v2, because v2 is newer than v1's first deploy.
3. Rolling back **to** v2 crosses nothing: v2's migration is what the schema already has. Rolling forward crosses nothing either. Rollback rows are never counted.

### Confirmation

Without confirmation a crossing rollback fails with `409 migration_confirmation_required`:

```json
{
  "error": {"code": "migration_confirmation_required", "message": "rolling back to deployment 41 crosses database migrations in deployment 44 (label db-migration on #12 (abc1234)); the old version may not work with the migrated schema. Confirm with confirm_migration_rollback=true (API) or --confirm-migration-rollback (CLI)"},
  "to": {"id": 41, "...": "..."},
  "migrations": [{"id": 44, "migration": true, "migration_reason": "label db-migration on #12 (abc1234)", "...": "..."}]
}
```

Repeat the request with `"confirm_migration_rollback": true` or `doorstep rollback blog --confirm-migration-rollback`. The `deployer` role is enough. The check runs after the target is chosen, so `rollback_in_progress`, `no_rollback_target` and `blocked` take precedence, and it is part of the same transaction as the rest of the request. A confirmed rollback is stored with `migration: true` and audits `migration_confirmed` and `migration_deployment_ids`.

### Automatic rollbacks

An automatic rollback after a failed gate must never cross a migration unattended. Before queuing one, the engine calls `MigrationsBetween` (or `(*rollback.Service).MigrationsCrossed`) with the target it picked and the failing deploy; when the answer is not empty it does not roll back and fails the deploy with `automatic rollback withheld: release contains database migrations; roll back with --confirm-migration-rollback`, and notifies as for any failure. A person then decides.

### Backup before deploy

A `backup` gate in `gates.before` asks an operator-owned webhook to back the database up before a flagged release is pinned, and holds the deploy until it reports success ([gates.md](gates.md#backup)). It runs for flagged deploys by default (`when: migration`), for every deploy with `when: always`, and for rollbacks only with `apply_to_rollbacks: true`.

## What the engine must do
## What the engine does

The [engine](engine.md) executes queued rollbacks like deploys and calls the `BlockList` port, which `*rollback.Service` implements ([architecture.md](architecture.md#engine-ports)):

- For kind `deploy`, it calls `Blocked(target, commit, digest)` right after resolving the digest; when it reports a block, it fails the deployment with a reason that names the block.
- Before `Pin`, it calls `PendingTrailers(target)` and passes them as `gitops.Request.Blocks` and `Unblocks`; after a `Pin` whose result has `Changed`, it calls `MarkMirrored(target, trailers, result.Commit)`. A `MarkMirrored` error is logged at `warn` and the trailers ride on the next pin commit again.
- For kind `rollback`, it uses the preset digest without resolving, the `checks`, `lock` and `soak` gates (and `schedule` without `apply_to_rollbacks`) record `skipped` ([gates.md](gates.md#rollbacks)), and it still waits for Argo CD to report synced and healthy.
- Before queuing an automatic rollback after a failed gate, it calls `MigrationsBetween(target, to, failing)` on the transaction-bound store (`to` is the `LastSuccessfulDeployment` it picked). When that returns any deployment, it queues nothing and fails the deploy with `automatic rollback withheld: release contains database migrations; roll back with --confirm-migration-rollback`, then notifies.
- It stops as soon as a transition returns `core.ErrStateConflict` or the context is cancelled.
- `deploy.queued` for the rollback and `deploy.cancelled` for the deployments the request cancelled are announced by `cli` right after the request commits, through `Engine.Queued` and `Engine.Cancelled`; `Cancelled` also closes the pull request of a cancelled deployment that had started pinning.

### The late-pin race

When a rollback cancels an in-flight deploy, the cancel is authoritative in Postgres: the worker's next transition fails with `ErrStateConflict`, and its next heartbeat loses the lease, which cancels the handler context in `queue.Runner`. But a worker already inside `Pin` can still land its commit for up to one heartbeat interval. Because the cancel removed its queue entry, the rollback does not wait for that lease and may run its own pin first, as a no-op if the target digest is still pinned; the cancelled worker's pin would then overwrite the rollback.

The engine closes the window cheaply: before a rollback moves to `succeeded`, it calls `Pin` again with `Repin` set ([gitops.md](gitops.md#restarts)). When the rollback's digest is still pinned, nothing is written and `Pin` returns the rollback's newest pin commit; if that is the one already recorded, the rollback succeeds. Otherwise (a late pin replaced the digest and `Pin` wrote it again, or an earlier claim wrote it again and was interrupted) the engine waits for Argo CD to sync that commit, runs the `after` gates again with `Reevaluate` (their verdicts are sticky, so `Evaluate` would only return the passes recorded for the replaced pin), records the commit and evaluates the gates for the new pin, all while staying in `verifying` (there is no transition back to `syncing`). The commit is recorded only after the gates were run again, so an interrupted claim repeats the wait and the re-evaluation instead of trusting the old verdicts. Re-pins are bounded by the 1 hour verify deadline ([engine.md](engine.md#error-classification-and-deadlines)). Delaying every rollback by a lease length was rejected; a final check costs one read of the pin file and its history. The interrupted worker also abandons its pull request when it sees that its deployment was cancelled.

## API

All endpoints are under `/api/v1/apps/{app}`, use the [error envelope](api.md#conventions) and audit in the same transaction as the change.

### `POST /api/v1/apps/{app}/rollback`

Role `deployer`. Body (optional; an empty body or `{}` is a one-click rollback, whether the request sends `Content-Length: 0` or a chunked body with no bytes; a non-empty body must be `application/json`):

```json
{"to_deployment_id": 41, "reason": "checkout errors", "confirm_migration_rollback": true}
```

| Field | Type | Notes |
| --- | --- | --- |
| `to_deployment_id` | integer | roll back to this succeeded deployment |
| `to_digest` | string | roll back to the newest succeeded deployment with this digest; at most one of the two |
| `reason` | string | optional, at most 500 characters, no control characters; recorded on the block |
| `confirm_migration_rollback` | boolean | confirms a rollback across releases flagged as containing database migrations; ignored when nothing is crossed |

`201 Created` with `Location: /api/v1/deployments/{id}`:

```json
{
  "deployment": {"id": 43, "kind": "rollback", "state": "queued", "rollback_of_id": 42, "...": "..."},
  "rolled_back": {"id": 42, "kind": "deploy", "state": "cancelled", "...": "..."},
  "block": {"id": 7, "app": "blog", "digest": "sha256:...", "commit": {"repository": "example-user/blog", "sha": "..."}, "reason": "checkout errors", "created_by": "user:oidc:3f2a", "deployment_id": 42, "rollback_id": 43, "created_at": "2026-11-24T12:00:00Z", "recorded_in_git": false},
  "cancelled": [42],
  "migrations": []
}
```

`deployment` and `rolled_back` use the [deployment fields](api.md#deployment-fields). `block` is `null` when nothing was blocked. `migrations` lists the flagged deployments a confirmed rollback crossed, and is empty otherwise.

| Status | Code | When |
| --- | --- | --- |
| 400 | `invalid_request` | both target fields, a malformed digest, a bad reason, or an explicit target of another app, not succeeded, without a digest or equal to the current version |
| 404 | `not_found` | the app is not configured |
| 409 | `no_rollback_target` | nothing has been deployed, or there is no earlier successful, unblocked digest |
| 409 | `rollback_in_progress` | a rollback of the app is queued or in flight; the body also has `deployment` |
| 409 | `blocked` | the explicit target's digest is blocked; the body also has `block` |
| 409 | `migration_confirmation_required` | the rollback crosses flagged releases and `confirm_migration_rollback` is not `true`; the body also has `to` and `migrations` |
| 503 | `contended` | deployments kept changing during the request; retry after `Retry-After` |

### `GET /api/v1/apps/{app}/blocks`

Role `viewer`. `200 {"blocks": [...]}` with the active blocks, newest first. A block has `id`, `app`, `environment`, `digest` (omitted for a commit-only block), `commit` (`repository` when known and `sha`; omitted for a digest-only block), `reason`, `created_by`, `deployment_id` and `rollback_id` (when set), `created_at`, and `recorded_in_git` (whether a pin commit carries its `Doorstep-Block` trailer yet).

### `POST /api/v1/apps/{app}/blocks`

Role `deployer`. Body `{"digest": "sha256:...", "commit_sha": "...", "reason": "..."}`: at least one of `digest` and `commit_sha`, and a reason (required, at most 500 characters). `201` with the new block and a `block.create` audit row, or `200` with the existing block when the version is already blocked. `400 invalid_request` for a missing or malformed field, `404` for an unknown app.

### `DELETE /api/v1/apps/{app}/blocks/{ref}`

Role `deployer`. `ref` is a `sha256:` digest or a 40-character lowercase commit SHA. `200 {"blocks": [...]}` with every block that was cleared (`cleared_at` and `cleared_by` set) and one `block.clear` audit row; `404 not_found` when nothing matching is blocked; `400 invalid_request` for a malformed ref. Like unlocking, it works for apps that were removed from the configuration.

## CLI

| Command | Does |
| --- | --- |
| `doorstep rollback <app> [--to <deployment-id>] [--reason <text>] [--confirm-migration-rollback] [-o table\|json]` | requests a rollback and prints `rollback 43 queued for blog: ghcr.io/example-user/blog@sha256:1111aaaa2222 (reverting deployment 42); blocked sha256:... (commit ...); cancelled 42`, plus `; confirmed rollback across migrations in deployment 44` after a confirmed crossing |
| `doorstep block <app> <digest\|commit> --reason <text>` | blocks a version by hand |
| `doorstep unblock <app> <digest\|commit>` | clears the matching blocks and prints each one |
| `doorstep blocks <app> [-o table\|json]` | lists the active blocks |

They are thin clients of the API above and share `--server`, `DOORSTEP_SERVER_URL`, `DOORSTEP_TOKEN` and `DOORSTEP_TOKEN_FILE` with `doorstep lock` ([configuration.md](configuration.md#doorstep-rollback-block-unblock-and-blocks)). `to_digest` is API only.

## Example

```sh
$ doorstep rollback blog --reason "5xx after the checkout change"
rollback 43 queued for blog: ghcr.io/example-user/blog@sha256:1111aaaa2222 (reverting deployment 42); blocked sha256:9f8e... (commit 0123...); cancelled 42

$ doorstep blocks blog
ID  DIGEST               COMMIT        BY              SINCE                 IN GIT  REASON
7   sha256:9f8e7d6c5b4a  0123456789ab  user:oidc:3f2a  2026-11-24T12:00:00Z  no      5xx after the checkout change

# fixed forward in a later commit; the bad commit may deploy again if it is ever the head
$ doorstep unblock blog sha256:9f8e7d6c5b4a...
unblocked sha256:9f8e... (commit 0123...) for blog
```

# Rollback and blocks

A rollback puts an app back on its previous good image digest. It is not a special mode: doorstep queues a new deployment of kind `rollback` that re-pins the earlier digest through the same queue, pin, Argo CD and verification path as any deploy ([ADR 0002](decisions/0002-rollback-is-a-new-deployment.md)), and it records a **block** on the version it replaced so that version is not deployed again automatically. Blocks live in Postgres and are mirrored into the GitOps repository as commit trailers ([ADR 0009](decisions/0009-blocks-in-postgres-mirrored-to-git.md)).

In the MVP a rollback is requested from the API or the CLI. The UI button comes later. `internal/rollback` implements everything on this page; the deploy engine executes the queued rollback deployment.

## Requesting a rollback

```sh
doorstep rollback blog
doorstep rollback blog --to 41 --reason "checkout errors"
```

One request does all of the following in one Postgres transaction, under the app's row lock (the lock intake and the queue use for the same app), so it either happens completely or not at all:

1. **Refuse duplicates.** If a rollback of the app is already queued or in flight, the request fails with `409 rollback_in_progress` and names that rollback. Double clicks are safe.
2. **Find the current version** (the one being rolled back). It is the newest deployment of the app, by start time (creation time while not started), that is in flight, `succeeded`, or `failed`/`cancelled` after its pin commit landed. Queued and superseded deployments, and failures that never pinned, did not change what runs and are ignored. A deploy stuck in `verifying`, or one that failed after pinning, is exactly what operators roll back. If nothing qualifies the request fails with `409 no_rollback_target`.
3. **Pick the target.**
   - By default: the last `succeeded` deployment of the app (latest `finished_at`) that has a digest, whose digest differs from the current one and is not blocked. Successful rollbacks count, since they are what ran.
   - `--to <deployment-id>` (API `to_deployment_id`), or `to_digest` (API only, the newest succeeded deployment with that digest): the deployment must belong to the same app, be `succeeded`, have a digest, differ from the current digest and not be blocked. A blocked target fails with `409 blocked`; anything else fails with `400 invalid_request`.
   - Postgres history picks the target because only history knows a digest passed its gates; git still decides what is actually pinned when the rollback runs.
4. **Cancel what the rollback replaces.** A current deployment that is still in flight is moved to `cancelled` with a compare-and-set. Queued deploys of the blocked commit are cancelled too. Other queued deploys (newer merges) are left alone and run after the rollback; to stop them as well, freeze the app with `doorstep lock`.
5. **Queue the rollback** at priority `rollback` (200), ahead of everything else for the app: kind `rollback`, `rollback_of_id` set to the current deployment, source `api`, `requested_by` the caller, the commit and image copied from the target, and the digest preset to the target's digest. The engine therefore never resolves a tag for a rollback and pins exactly the previous digest even if the tag moved. The rollback is always queued, even when the target digest is already pinned: the pin is then a no-op, and the rollback still waits for Argo CD to report synced and healthy and leaves a history row.
6. **Block the replaced version** (see below) with the reason given, or `rolled back by <actor>`.
7. **Audit** a `rollback.request` row naming the rollback deployment, with `rolled_back_deployment_id`, `to_deployment_id`, `digest`, `blocked_digest`, `blocked_commit`, `block_id`, `reason` and `cancelled` in its detail.

If another writer changes one of the deployments between reading and cancelling it, the whole transaction is retried, at most three times; after that the request fails with `503 contended` and `Retry-After: 1`.

The `doorstep_rollbacks_total{app, reason="manual"}` counter is incremented for every accepted request ([metrics.md](metrics.md)).

## Blocks

A block says "do not deploy this version of this app automatically". It is keyed on the image digest, the app commit SHA, or both:

- A rollback blocks the current deployment's digest and, unless it equals the target's commit, its commit SHA. A deploy cancelled before its digest was resolved gets a commit-only block. A cancelled rebuild of the target's own commit that never resolved blocks nothing.
- `doorstep block <app> <digest|commit> --reason <text>` blocks by hand. The reason is required.
- Lookups match either key: a block on a digest stops that digest, a block on a commit stops that commit.

Blocks never stop rollbacks. They stop automatic deploys at two places:

- **Intake** refuses a green commit that has an active block on its commit SHA. The check runs inside the same transaction that would enqueue the deploy ([architecture.md](architecture.md#merge-and-ci-detection)), so it cannot race with a rollback request. The evaluation is saved, nothing is queued, the tracked commit stays open and `intake.Result.Blocked` names the block; intake logs `commit is blocked; not enqueued`.
- **The engine** refuses a deploy whose resolved digest is blocked, after resolving and before pinning, and fails the deployment with a reason naming the block. This covers deploys requested by other paths and rebuilt images of an unblocked commit. Rollbacks skip the check.

A rollback target is never a blocked digest, and the default target selection skips blocked digests, so "v3 succeeded, v3 rolled back, then v2 rolled back" picks v1, not the blocked v3.

### Unblocking

```sh
doorstep blocks blog
doorstep unblock blog sha256:4f1c...
doorstep unblock blog 0123456789abcdef0123456789abcdef01234567
```

`unblock` clears every active block that matches the digest or commit given (a commit can have several digest blocks), records who cleared it and writes a `block.clear` audit row. Clearing does not deploy anything. A commit that intake refused stays open, so once unblocked it is queued by the next check event or poll while it is still the branch head and green. To deploy an older version explicitly, roll back to it with `--to`, or push it again.

`doorstep block` on a version that is already blocked returns the existing block (`200` instead of `201`) and writes no audit row.

### Blocks in git

Postgres is the operational source of truth: a block or unblock takes effect in the request's transaction, whatever happens in git. Git is an eventually consistent mirror, so the GitOps history alone still tells which digests must not be deployed:

- A digest block that has not been written to git yet (`block_commit_sha` is null), and a cleared digest block whose block reached git but whose unblock has not (`unblock_commit_sha` is null), are **pending trailers** of the app.
- Before every pin of the app, the engine reads the pending trailers and adds them to the pin commit as `Doorstep-Block` and `Doorstep-Unblock` ([gitops.md](gitops.md#commit-messages)). After a pin that wrote a commit (`Changed`), it marks them mirrored with that commit's SHA.
- So a rollback's own pin commit carries `Doorstep-Block` for the digest it replaces, as [ADR 0001](decisions/0001-go-and-core-design.md) describes, and an unblock usually travels on the pin commit of the next deploy of that app.
- When the rollback target is already pinned, the pin writes no commit and the block stays pending until the app's next pin commit. No empty commits are made, so this works in pull request mode and under branch protection.
- A digest that is cleared and blocked again before either reached git only produces `Doorstep-Block`; the stale unblock is settled with it. Commit-only blocks are not mirrored: the trailer vocabulary has no commit form.

The lag is harmless for unblocks: until the unblock reaches git, git still says "blocked", which is the safe side. A block that never reaches git (because the app is never pinned again) exists only in Postgres; restoring doorstep from git alone would lose it. An explicit flush with an empty commit and a command that rebuilds blocks from the git history are possible follow-ups.

## What the engine must do

The engine (bead doorstep-ehl.4.4) executes queued rollbacks like deploys and calls the `BlockList` port, which `*rollback.Service` implements ([architecture.md](architecture.md#engine-ports)):

- For kind `deploy`, call `Blocked(target, commit, digest)` right after resolving the digest; when it reports a block, fail the deployment with a reason that names the block.
- Before `Pin`, call `PendingTrailers(target)` and pass them as `gitops.Request.Blocks` and `Unblocks`; after a `Pin` whose result has `Changed`, call `MarkMirrored(target, trailers, result.Commit)`.
- For kind `rollback`, use the preset digest without resolving, skip the CI and soak gates, and still wait for Argo CD to report synced and healthy.
- Stop as soon as a transition returns `core.ErrStateConflict` or the context is cancelled.

### The late-pin race

When a rollback cancels an in-flight deploy, the cancel is authoritative in Postgres: the worker's next transition fails with `ErrStateConflict`, and its next heartbeat loses the lease, which cancels the handler context in `queue.Runner`. But a worker already inside `Pin` can still land its commit for up to one heartbeat interval. Because the cancel removed its queue entry, the rollback does not wait for that lease and may run its own pin first, as a no-op if the target digest is still pinned; the cancelled worker's pin would then overwrite the rollback.

The engine closes the window cheaply: before a rollback moves to `succeeded`, it reads `PinWriter.Current` and, if the pinned digest is not the rollback's digest, pins again (and waits for sync again). Delaying every rollback by a lease length was rejected; a final check costs one read.

## API

All endpoints are under `/api/v1/apps/{app}`, use the [error envelope](api.md#conventions) and audit in the same transaction as the change.

### `POST /api/v1/apps/{app}/rollback`

Role `deployer`. Body (optional; an empty body or `{}` is a one-click rollback):

```json
{"to_deployment_id": 41, "reason": "checkout errors"}
```

| Field | Type | Notes |
| --- | --- | --- |
| `to_deployment_id` | integer | roll back to this succeeded deployment |
| `to_digest` | string | roll back to the newest succeeded deployment with this digest; at most one of the two |
| `reason` | string | optional, at most 500 characters, no control characters; recorded on the block |

`201 Created` with `Location: /api/v1/deployments/{id}`:

```json
{
  "deployment": {"id": 43, "kind": "rollback", "state": "queued", "rollback_of_id": 42, "...": "..."},
  "rolled_back": {"id": 42, "kind": "deploy", "state": "cancelled", "...": "..."},
  "block": {"id": 7, "app": "blog", "digest": "sha256:...", "commit": {"repository": "example-user/blog", "sha": "..."}, "reason": "checkout errors", "created_by": "user:oidc:3f2a", "deployment_id": 42, "rollback_id": 43, "created_at": "2026-11-24T12:00:00Z", "recorded_in_git": false},
  "cancelled": [42]
}
```

`deployment` and `rolled_back` use the [deployment fields](api.md#deployment-fields). `block` is `null` when nothing was blocked.

| Status | Code | When |
| --- | --- | --- |
| 400 | `invalid_request` | both target fields, a malformed digest, a bad reason, or an explicit target of another app, not succeeded, without a digest or equal to the current version |
| 404 | `not_found` | the app is not configured |
| 409 | `no_rollback_target` | nothing has been deployed, or there is no earlier successful, unblocked digest |
| 409 | `rollback_in_progress` | a rollback of the app is queued or in flight; the body also has `deployment` |
| 409 | `blocked` | the explicit target's digest is blocked; the body also has `block` |
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
| `doorstep rollback <app> [--to <deployment-id>] [--reason <text>] [-o table\|json]` | requests a rollback and prints `rollback 43 queued for blog: ghcr.io/example-user/blog@sha256:1111aaaa2222 (reverting deployment 42); blocked sha256:... (commit ...); cancelled 42` |
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

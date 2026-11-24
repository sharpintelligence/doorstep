# Locks

A freeze lock stops new deploys from starting, for one app or for every app, while someone does something that must not race with a deploy: a database migration, an incident, a holiday freeze. A lock has an owner, a reason and an optional expiry, and anyone with API access can see who holds it and why.

The code lives in `internal/locks` (validation, notifications, the reminder loop), `internal/store/locks.go` (SQL) and `internal/api` (HTTP handlers); the design is recorded in [ADR 0004](decisions/0004-freeze-locks.md).

## What a freeze blocks

- **New deploys of the locked scope do not start.** A deployment of kind `deploy` that is still `queued` is not claimed while a live freeze covers its app, either an app freeze or the global freeze.
- **Intake is unaffected.** Webhooks, the API and the CLI keep queueing deploys for a locked app. When the lock is released or expires, the queue resumes in its usual order: within one app the head of line is claimed first, so a frozen head holds the deploys queued behind it.
- **Deploys already in progress finish.** A deployment that is `resolving`, `pinning`, `syncing` or `verifying` when the lock is taken runs to completion, including after a worker crash and a re-claim. Stopping it half way would leave Git and the cluster between two versions.
- **Rollbacks are never blocked.** A deployment of kind `rollback` is claimed under a freeze, and since rollbacks are queued at a higher priority it becomes the head of line ahead of the frozen deploys. A freeze is meant to keep production stable, and a rollback is how doorstep restores it ([ADR 0002](decisions/0002-rollback-is-a-new-deployment.md)).
- **Other apps are unaffected** by an app freeze. Apps are matched by name and environment.

A lock taken while a worker is already claiming the same app either waits for that claim (an app lock takes the app's row lock, which the claim holds) or is seen by the claim's re-check, so the claim backs off. A global lock committed in the same instant as a claim may let that one claim proceed; it counts as started before the freeze.

## Scope, owner and reason

| Scope | Created by | Blocks |
| --- | --- | --- |
| app | `POST /api/v1/apps/{app}/lock`, `doorstep lock <app>` | queued deploys of that app |
| global | `POST /api/v1/lock`, `doorstep lock --global` | queued deploys of every app |

At most one freeze exists per app, and one global freeze. An app freeze and the global freeze can coexist; both must be released before the app deploys again.

- **Owner** is the authenticated identity of the caller, taken from the API token. It cannot be set in the request, so it cannot be spoofed. At most 200 characters.
- **Reason** is required: 1 to 500 characters after trimming surrounding whitespace, valid UTF-8, without control characters (so it is one line in logs, notifications and the CLI table).
- **Expiry** is optional. `expires_in` is a Go duration (`2h`, `90m`) and `expires_at` an RFC 3339 time; at most one of them. The expiry must be in the future. Without one, the lock is held until someone releases it.

## Conflicts and unlocking

Locking a scope that already has a live freeze fails with `409` and the existing lock in the response. doorstep never overwrites or steals a lock silently; release it first if it should change hands.

Anyone who can call the API may release any freeze, not only its owner, so a lock forgotten by someone who went home can be cleared. The release records who did it. Releasing a scope that has no live freeze answers `404`.

## Expiry

Expiry is compared with the database clock (`now()` in Postgres), the one clock all replicas share ([database.md](database.md#clock)). An expired lock stops blocking deploys at its expiry time and disappears from listings at once, whether or not anything has cleaned it up yet. The row itself is deleted, with an `expired` audit record, by the next lock or unlock of the same scope or by the sweep that every replica runs once a minute; the sweep also sends the `expired` notification.

## Reminders

A long-held freeze is easy to forget. Once a lock has been held for `locks.reminder_interval` (default `24h`, environment `DOORSTEP_LOCK_REMINDER_INTERVAL`; see [configuration.md](configuration.md#runtime-settings)), the sweep sends a `reminded` notification, and repeats it every interval while the lock is still held. `0s` disables reminders; expired locks are still cleaned up.

The sweep claims due reminders with a single `UPDATE ... RETURNING` that sets `reminded_at`, so with several replicas each reminder is sent by exactly one of them, without leader election. The reminder is marked before it is sent: if the process dies in between, that reminder is lost and the next one follows one interval later, rather than every replica repeating it.

## Notifications and audit

Every change is a `core.LockEvent` with an action, the lock, the actor and the time:

| Action | When | Actor |
| --- | --- | --- |
| `locked` | a freeze was taken | the owner |
| `unlocked` | a freeze was released | whoever released it |
| `expired` | an expired freeze was cleaned up | none |
| `reminded` | a freeze has been held for another reminder interval | none |

`internal/locks` hands each event to a `locks.Notifier` after the change has committed. A notifier error is logged and never fails the request or the sweep. Until the notification channels are wired in, `serve` logs each event at debug level as `lock event`.

Independently of notifications, every change is written to the append-only `lock_events` table in the same transaction as the change itself ([database.md](database.md#lock_events)), so who locked or unlocked what, when and why survives after the lock row is gone. `Store.ListLockEvents` reads it newest first; the history and UI work exposes it.

## API

All lock endpoints are under `/api/v1`, are served on the metrics listener only until API authentication exists (see [api.md](api.md)), and require authentication: `Authorization: Bearer <token>`. A missing or rejected token answers `401` with `WWW-Authenticate: Bearer`. Responses are JSON; errors use one envelope:

```json
{"error": {"code": "not_found", "message": "blog is not locked"}}
```

| Code | Status | Meaning |
| --- | --- | --- |
| `invalid_request` | `400`, `413` | malformed body, unknown field, invalid reason or expiry; `413` when the body is over 64 KiB |
| `unauthorized` | `401` | missing or rejected credentials |
| `not_found` | `404` | unknown app (not in the configuration file), or nothing to unlock |
| `locked` | `409` | the scope is already locked; the body also carries `lock` |
| `unsupported_media_type` | `415` | a `POST` without `Content-Type: application/json` |
| `internal` | `500` | an unexpected error, logged on the server and not described in the response |

Request bodies must be a single JSON object with only the documented fields.

### `POST /api/v1/apps/{app}/lock` and `POST /api/v1/lock`

```sh
curl -sS -X POST https://doorstep.example.com/api/v1/apps/blog/lock \
  -H "Authorization: Bearer $DOORSTEP_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"reason": "database migration", "expires_in": "2h"}'
```

| Field | Required | Description |
| --- | --- | --- |
| `reason` | yes | why deploys are frozen |
| `expires_in` | no | Go duration until the lock expires |
| `expires_at` | no | RFC 3339 time at which the lock expires; not together with `expires_in` |

`{app}` must be an app in the configuration file. The answer is `201` with the lock:

```json
{"scope": "app", "app": "blog", "owner": "alice", "reason": "database migration", "created_at": "2026-11-23T12:00:00Z", "expires_at": "2026-11-23T14:00:00Z"}
```

A global lock has `"scope": "global"` and no `app`. `expires_at` is `null` for a lock without expiry. Times are UTC.

When the scope is already locked the answer is `409`:

```json
{"error": {"code": "locked", "message": "blog is already locked by bob: incident 42"}, "lock": {"scope": "app", "app": "blog", "owner": "bob", "reason": "incident 42", "created_at": "2026-11-23T11:00:00Z", "expires_at": null}}
```

### `DELETE /api/v1/apps/{app}/lock` and `DELETE /api/v1/lock`

Releases the freeze and answers `204` without a body, or `404` when the scope has no live freeze.

### `GET /api/v1/locks`

Lists live freezes, the global one first, then apps by name:

```json
{"locks": [
  {"scope": "global", "owner": "bob", "reason": "holiday freeze", "created_at": "2026-12-20T17:00:00Z", "expires_at": "2027-01-02T09:00:00Z"},
  {"scope": "app", "app": "blog", "owner": "alice", "reason": "database migration", "created_at": "2026-11-23T12:00:00Z", "expires_at": null}
]}
```

### Authentication

The API authenticates every request through the `api.Authenticator` port. API tokens are not issued yet; until they are, `serve` wires an authenticator that rejects every request, so the lock endpoints answer `401` and cannot be used remotely. The token work replaces that one seam (`apiAuthenticator` in `internal/cli/serve.go`).

## CLI

```sh
export DOORSTEP_SERVER_URL=https://doorstep.example.com
export DOORSTEP_TOKEN_FILE=~/.config/doorstep/token

doorstep lock blog --reason "database migration" --for 2h
doorstep lock --global --reason "holiday freeze" --until 2027-01-02T09:00:00Z
doorstep locks
doorstep locks -o json
doorstep unlock blog
doorstep unlock --global
```

```text
SCOPE   OWNER  SINCE                 EXPIRES               REASON
global  bob    2026-12-20T17:00:00Z  2027-01-02T09:00:00Z  holiday freeze
blog    alice  2026-11-23T12:00:00Z  -                     database migration
```

Flags and environment variables are listed in [configuration.md](configuration.md#doorstep-lock-unlock-and-locks).

## For the engine

`locks.Service.Blocking(ctx, target)` returns the live freezes that block a target (its app freeze and the global freeze, global first). The engine uses it to record the `lock` gate as pending with the owner and reason as evidence. The claim already skips frozen deploys, so the engine does not have to enforce the freeze itself. Deployment-kind lock rows are reserved for the engine.

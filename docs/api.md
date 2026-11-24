# HTTP API

doorstep's JSON API lives under `/api/v1/`. It is built by `internal/api` and mounted once by `serve`.

> **Metrics listener only, for now.** API authentication is being built separately. Until it is merged, the API is mounted only on the metrics listener (`--metrics-listen-addr`, default `:9090`) and never on the main listener, which receives GitHub webhooks and is usually reachable from the internet. When the metrics address is empty the API is not served at all and `serve` logs a warning. The queue shows commit SHAs, GitHub logins and deploy cadence, so keep the metrics listener on a private network.

## Conventions

- Responses are JSON (`Content-Type: application/json`) with `Cache-Control: no-store`.
- Timestamps are RFC 3339 in UTC.
- Errors use one envelope:

  ```json
  {"error": {"code": "not_found", "message": "app is not configured"}}
  ```

  | Status | Code | When |
  | --- | --- | --- |
  | 400 | `invalid_request` | malformed or invalid request body |
  | 401 | `unauthorized` | lock endpoints without valid credentials |
  | 404 | `not_found` | unknown resource or unknown API path |
  | 405 | `method_not_allowed` | wrong method; the `Allow` header lists the right ones |
  | 409 | `locked` | the app or the global scope is already frozen |
  | 415 | `unsupported_media_type` | a request body that is not `application/json` |
  | 500 | `internal` | anything else; details are logged, never returned |

The lock endpoints (`/api/v1/apps/{app}/lock`, `/api/v1/lock`, `/api/v1/locks`) are documented in [locks.md](locks.md#api).

## `GET /api/v1/apps/{app}/queue`

The deployments of an app that still need work, in the order they will run: in-flight first, then by priority, then oldest first (see [queue.md](queue.md#order)). Positions start at 1 for each environment.

Returns `200` when the app is in the configuration file or still has queue entries (an app removed from configuration while work was pending), and `404` otherwise. An app with nothing queued returns an empty `entries` list.

```json
{
  "app": "blog",
  "depth": 1,
  "entries": [
    {
      "position": 1,
      "deployment_id": 41,
      "environment": "prod",
      "kind": "deploy",
      "state": "syncing",
      "priority": "normal",
      "commit_sha": "4c1d0c3f6a8e2b9d7e5f3a1c0b9d8e7f6a5b4c3d",
      "source": "webhook",
      "requested_by": "octocat",
      "enqueued_at": "2026-11-23T10:02:11Z",
      "available_at": "2026-11-23T10:02:11Z",
      "attempts": 2,
      "in_flight": true,
      "lease_expires_at": "2026-11-23T10:05:40Z"
    },
    {
      "position": 2,
      "deployment_id": 44,
      "environment": "prod",
      "kind": "deploy",
      "state": "queued",
      "priority": "urgent",
      "commit_sha": "9e8d7c6b5a4f3e2d1c0b9a8f7e6d5c4b3a2f1e0d",
      "source": "api",
      "requested_by": "alice",
      "enqueued_at": "2026-11-23T10:04:59Z",
      "available_at": "2026-11-23T10:04:59Z",
      "attempts": 0,
      "in_flight": false,
      "lease_expires_at": null
    }
  ]
}
```

| Field | Meaning |
| --- | --- |
| `depth` | entries that are waiting, not in flight |
| `priority` | `normal`, `urgent`, `rollback`, or the number for any other value |
| `available_at` | a later time means the entry is backing off after a failure |
| `attempts` | how many times the entry has been claimed |
| `in_flight` | the deployment has started, or a worker holds a live lease on it |
| `lease_expires_at` | expiry of the live lease, or `null` when no worker holds one |

## Deploy history

Every deployment records its full lineage: the app commit it was built from, the image tag and the digest it resolved to, the digest it replaced, the GitOps repository and commit that pinned it, the Argo CD revision that synced it, its gate results, its outcome and who or what triggered it. Rollbacks and retries are deployments of their own that point back at the deployment they act on ([ADR 0002](decisions/0002-rollback-is-a-new-deployment.md)).

### `GET /api/v1/apps/{app}/deployments`

Lists the deployments of the app named `{app}`, newest first. Without `environment`, deployments of that app in every environment are returned. An app that has never been deployed (or is not configured) returns `200` with an empty list rather than `404`.

| Parameter | Meaning |
| --- | --- |
| `environment` | only this environment |
| `state` | only these states (`queued`, `resolving`, `pinning`, `syncing`, `verifying`, `succeeded`, `failed`, `cancelled`, `superseded`); repeat the parameter or separate values with commas |
| `kind` | only these kinds (`deploy`, `rollback`); repeatable or comma-separated |
| `commit` | only deployments of this app commit (40 lowercase hex characters) |
| `digest` | only deployments whose resolved image digest is this one (`sha256:` and 64 lowercase hex characters), to find which deployment shipped a digest |
| `limit` | page size, `1` to `200`, default `50` |
| `cursor` | the `next_cursor` of the previous page |

Unknown parameters, repeated single-value parameters and invalid values are rejected with `400`, so a typo never silently returns unfiltered history. The last successful deployment of a target is `?environment=prod&state=succeeded&limit=1`, ordered by id; the engine's rollback lookup orders by finish time instead (see [database.md](database.md#history-read-models)).

```json
{
  "items": [
    {
      "id": 42,
      "app": "web",
      "environment": "prod",
      "kind": "deploy",
      "state": "succeeded",
      "trigger": {"source": "webhook", "requested_by": "octocat"},
      "commit": {"repository": "acme/web", "sha": "0123456789abcdef0123456789abcdef01234567"},
      "image": {
        "repository": "ghcr.io/acme/web",
        "tag": "0123456789abcdef0123456789abcdef01234567",
        "digest": "sha256:…",
        "previous_digest": "sha256:…"
      },
      "gitops": {"repository": "acme/gitops", "commit_sha": "89abcdef0123456789abcdef0123456789abcdef"},
      "argocd": {"revision": "89abcdef0123456789abcdef0123456789abcdef"},
      "created_at": "2026-03-01T11:00:00Z",
      "updated_at": "2026-03-01T11:05:00Z",
      "started_at": "2026-03-01T11:00:02Z",
      "finished_at": "2026-03-01T11:05:00Z"
    }
  ],
  "next_cursor": "42"
}
```

#### Pagination

Pages are keyset-paginated on the deployment id: `next_cursor` is present only when more deployments exist, and passing it back as `cursor` (with the same filters) returns the next older page. Deployments created while you page appear at the front of the list and never shift or duplicate entries on later pages. Treat the cursor as opaque.

### `GET /api/v1/deployments/{id}`

Returns one deployment with its timeline. `{id}` must be a positive integer; the endpoint takes no query parameters. A deployment that does not exist returns `404`.

```json
{
  "deployment": {"id": 42, "…": "same shape as a list item"},
  "events": [
    {"id": 1, "type": "transition", "to": "queued", "actor": "octocat", "message": "queued from webhook", "data": {}, "occurred_at": "…"},
    {"id": 2, "type": "transition", "from": "queued", "to": "resolving", "actor": "worker-1", "data": {}, "occurred_at": "…"},
    {"id": 3, "type": "note", "actor": "worker-1", "message": "lineage recorded", "data": {"image_digest": "sha256:…"}, "occurred_at": "…"}
  ],
  "events_truncated": false,
  "gates": [
    {"name": "health", "status": "passed", "message": "Healthy", "evidence": {}, "started_at": "…", "finished_at": "…"}
  ],
  "rollbacks": [],
  "retries": []
}
```

- `events` are the newest 500 events of the deployment, listed oldest first, so the terminal transition and the latest lineage notes are always included; `events_truncated` is `true` when older events were left out.
- `gates` holds the latest result of each named gate, ordered by name.
- `rollbacks` are the deployments whose `rollback_of_id` is this deployment ("was it rolled back, and did that work"); `retries` are those whose `parent_id` is this deployment. Both are newest first.
- The deployment, its events, gates and related deployments are read from one database snapshot, so they always agree with each other.

## Deployment fields

| Field | Meaning |
| --- | --- |
| `id` | store-assigned deployment id |
| `app`, `environment` | the target |
| `kind` | `deploy` or `rollback` |
| `state` | current state; see [architecture.md](architecture.md#state-machine-and-concurrency) |
| `trigger.source` | what triggered it: `webhook`, `poll`, `api`, `cli` or `system` (rollbacks queued by doorstep) |
| `trigger.requested_by` | who triggered it, when known |
| `commit.repository`, `commit.sha` | the app commit being deployed |
| `image.repository`, `image.tag` | the requested image |
| `image.digest` | the digest the tag resolved to; absent until resolved |
| `image.previous_digest` | the digest found in Git before pinning, as observed; Git stays authoritative |
| `gitops.repository`, `gitops.commit_sha` | the GitOps repository and the commit that pinned the digest |
| `argocd.revision` | the revision Argo CD reported as synced; free text, as Argo CD revisions can be chart versions or several sources |
| `parent_id` | the deployment this one retries |
| `rollback_of_id` | the deployment this rollback reverts |
| `superseded_by_id` | the newer deployment that replaced this one before it started pinning |
| `failure_reason` | why it failed |
| `created_at`, `updated_at`, `started_at`, `finished_at` | RFC 3339 timestamps in UTC; `started_at` is when it first became active, `finished_at` when it reached a terminal state |

Optional fields and empty strings are omitted. `gitops` and `argocd` are always present as objects, empty until known.

## Extending the API

Each resource registers its routes in its own file (`queue.go` has `registerQueue`) with full-path `ServeMux` patterns under `api.Prefix`, and depends on small interfaces declared next to it, so handlers are tested with fakes and without Postgres. Shared helpers (`writeJSON`, `writeError`, `methodNotAllowed`) are in `respond.go`. `api.Options.Middleware` wraps the whole API in one place; authentication plugs in there.

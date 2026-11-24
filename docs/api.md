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
  | 404 | `not_found` | unknown resource or unknown API path |
  | 405 | `method_not_allowed` | wrong method; the `Allow` header lists the right ones |
  | 500 | `internal` | anything else; details are logged, never returned |

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

## Extending the API

Each resource registers its routes in its own file (`queue.go` has `registerQueue`) with full-path `ServeMux` patterns under `api.Prefix`, and depends on small interfaces declared next to it, so handlers are tested with fakes and without Postgres. Shared helpers (`writeJSON`, `writeError`, `methodNotAllowed`) are in `respond.go`. `api.Options.Middleware` wraps the whole API in one place; authentication plugs in there.

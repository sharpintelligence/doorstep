# Notifications

doorstep tells people and systems what it is doing in three ways:

- **Slack** messages through an incoming webhook (or any Slack-compatible receiver, such as Mattermost);
- **generic webhooks**: a signed, versioned JSON document posted to a URL of your choice;
- **GitHub deployments**: a deployment and its statuses on the application repository, so the commit, pull request and repository *Environments* page show what is running where.

Slack and webhook targets are configured in `notifications[]` and attached to apps with `apps[].notify` (see [configuration.md](configuration.md#notifications)). GitHub deployments need no target: they are reported for every app whenever the GitHub App is configured, unless the app opts out.

Notifications are advisory. A failed or slow notification never delays, fails or rolls back a deploy; the deployment history is the durable record ([ADR 0006](decisions/0006-notifications-are-best-effort.md)).

## Events

| Event | When | Sent to Slack and webhooks by default |
| --- | --- | --- |
| `deploy.queued` | a deployment was queued | no |
| `deploy.started` | a deployment (or rollback) started resolving its image | yes |
| `deploy.succeeded` | a deployment passed its gates | yes |
| `deploy.failed` | a deployment failed; a failed rollback is a `deploy.failed` of kind `rollback` and is shown more prominently | yes |
| `deploy.rolled_back` | a rollback succeeded, so the deployment it reverts is no longer running | yes |
| `deploy.cancelled` | a deployment was cancelled | no |
| `deploy.superseded` | a queued deployment was replaced by a newer one before it started pinning | no |
| `lock.changed` | a lock was acquired or released | yes |
| `lock.reminder` | a lock is still held | yes |

Set `events` on a target to choose its events, for example a channel that only hears about failures:

```yaml
notifications:
  - name: alerts
    type: slack
    webhook_url_file: /etc/doorstep/secrets/slack-alerts-url
    events: [deploy.failed, deploy.rolled_back]
```

Leaving `events` out means the default set above. An empty list, an unknown event or a repeated event is a configuration error.

A rollback is a separate deployment of kind `rollback` ([ADR 0002](decisions/0002-rollback-is-a-new-deployment.md)). When a deploy's gates fail, you see `deploy.failed` for the deploy, then `deploy.started` for the rollback, then either `deploy.rolled_back` (the rollback worked) or `deploy.failed` for the rollback (it did not, and someone has to act).

The engine decides which locks to announce: the intent is that manual freeze locks produce `lock.changed` and periodic `lock.reminder` events, while the per-deployment lock every deploy takes does not.

## Links

Set `server.public_url` (`--public-url`, `DOORSTEP_PUBLIC_URL`) to the address people use to reach doorstep, for example `https://doorstep.example.com`. Messages then link to `<public_url>/deployments/<id>`, the webhook payload carries that URL, and GitHub deployment statuses use it as their `log_url`. Without it these links are left out. Commit links always point at the GitHub web host derived from `github.api_url` (`github.com` for `api.github.com`, the host without `/api/v3` for GitHub Enterprise Server).

## Slack

Create an [incoming webhook](https://api.slack.com/messaging/webhooks) for the channel and store its URL in a Secret or environment variable. The URL is a credential: doorstep only reads it from `webhook_url_env` or `webhook_url_file`, re-reads it for every message (so a rotated Secret applies without a restart), never logs it and removes it from error messages.

Each message has a plain `text` line, used for push notifications and by Slack-compatible receivers, and one attachment whose colour bar shows the outcome at a glance:

| Colour | Events |
| --- | --- |
| blue | queued, started |
| green | succeeded |
| amber | rolled back |
| red | failed |
| dark red | a failed rollback |
| purple | lock acquired, lock reminder |
| grey | cancelled, superseded, lock released |

The attachment holds Block Kit blocks: the app and outcome (with a link to the deployment a rollback reverts), then fields for the commit (linked to GitHub), the short image digest, who requested it, how long it took and the failure reason (cut to 500 characters). Lock messages show the owner, reason, how long the lock has been held and when it expires. A *View deployment* link follows when `server.public_url` is set. All text from deployments, commits and people is escaped, so a commit message or failure reason cannot mention `@channel` or inject links.

The webhook URL must be an absolute `http` or `https` URL. Any host is accepted, so Mattermost, Rocket.Chat or a local relay work as long as they accept Slack's message format.

## Generic webhooks

doorstep sends `POST` with `Content-Type: application/json` and these headers:

| Header | Value |
| --- | --- |
| `User-Agent` | `doorstep/<version>` |
| `X-Doorstep-Event` | the event, such as `deploy.succeeded` |
| `X-Doorstep-Delivery` | a random 128-bit hex ID for this delivery, the same on every retry |
| `X-Doorstep-Signature-256` | `sha256=` and the hex HMAC-SHA256 of the raw request body, keyed with the target's secret; only when `secret_env` or `secret_file` is set |

The body (version 1):

```json
{
  "version": 1,
  "id": "4f1c2a9e8b7d6c5e4f3a2b1c0d9e8f7a",
  "type": "deploy.failed",
  "occurred_at": "2026-11-23T10:30:00Z",
  "app": "blog",
  "environment": "prod",
  "actor": "alice",
  "message": "optional free text",
  "deployment": {
    "id": 42,
    "kind": "deploy",
    "state": "failed",
    "source": "webhook",
    "requested_by": "alice",
    "commit": {"repository": "example-user/blog", "sha": "0123456789abcdef0123456789abcdef01234567"},
    "image": "ghcr.io/example-user/blog:0123456789abcdef0123456789abcdef01234567",
    "digest": "sha256:9b2c...",
    "previous_digest": "sha256:1a3c...",
    "pin_commit_sha": "89abcdef0123456789abcdef0123456789abcdef",
    "failure_reason": "health gate: Application blog is Degraded",
    "rollback_of_id": 41,
    "started_at": "2026-11-23T10:28:25Z",
    "finished_at": "2026-11-23T10:30:00Z",
    "url": "https://doorstep.example.com/deployments/42"
  },
  "lock": {
    "kind": "freeze",
    "owner": "bob",
    "reason": "database migration",
    "change": "acquired",
    "created_at": "2026-11-23T07:30:00Z",
    "expires_at": "2026-11-23T14:30:00Z"
  }
}
```

`deployment` is present for `deploy.*` events and `lock` for `lock.*` events. Optional fields that are empty are left out: `actor`, `message`, `source`, `requested_by`, `image`, the digests, `pin_commit_sha`, `failure_reason`, `rollback_of_id` (only on rollbacks), the timestamps, `url` (only with `server.public_url`), `lock.reason`, `lock.change` (only on `lock.changed`) and `lock.expires_at`. New fields may be added within version 1; a breaking change gets a new `version`. Examples of every event are in `internal/notify/testdata/webhook`.

### Verifying a delivery

Compute the HMAC over the exact bytes you received, before parsing them, and compare in constant time. Use `id` to drop duplicates and `occurred_at` to reject old deliveries; both are inside the signed body.

```python
import hashlib, hmac

def verify(secret: bytes, body: bytes, header: str) -> bool:
    expected = "sha256=" + hmac.new(secret, body, hashlib.sha256).hexdigest()
    return hmac.compare_digest(expected, header)
```

```go
func verify(secret, body []byte, header string) bool {
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return hmac.Equal([]byte("sha256="+hex.EncodeToString(mac.Sum(nil))), []byte(header))
}
```

The scheme is the same as GitHub's `X-Hub-Signature-256`, so tools such as [adnanh/webhook](https://github.com/adnanh/webhook) (`payload-hmac-sha256` with the `sha256=` prefix) and n8n can verify it without code.

Answer with any `2xx` status. A target that returns HTML or JSON in the body need not: doorstep reads at most 4 KiB of the response and only quotes the first 256 bytes in its logs.

## Delivery

> **Not wired into `serve` yet.** The dispatcher, targets and GitHub reporter described here are built and tested, and `doorstep notify test` uses them, but nothing emits notifications until the deployment engine lands (doorstep-ehl.4.4), which builds the dispatcher in `serve`, hands it to the engine and closes it with the shutdown timeout. Until then `notifications[]` and `apps[].notify` are validated but `serve` sends nothing.

- **Non-blocking.** The engine hands each notification to a dispatcher and carries on. The dispatcher checks it, picks the targets of its app plus the GitHub reporter, and puts it on each target's queue without any network I/O.
- **Per-target queues.** Every target has its own queue of 64 notifications and its own worker, so a slow Slack never delays GitHub statuses, and each target receives events in the order they happened (`started` before `succeeded`). When a queue is full the new notification is dropped, logged at `warn` and counted.
- **Retries.** Each delivery gets up to 4 attempts, 10 seconds each, with exponential backoff of about 1, 2 and 4 seconds (plus or minus 20%). A `Retry-After` header is honoured up to 30 seconds. Network errors, timeouts, `408`, `429`, `5xx`, GitHub rate limits and secrets that cannot be read are retried; other `4xx` and `3xx` answers are not. Redirects are never followed, so signed payloads and secret URLs stay on the configured host.
- **Shutdown.** On shutdown the dispatcher stops accepting notifications and keeps delivering what is queued until the shutdown timeout; whatever is left then is dropped and counted.
- **Not durable.** Queues live in memory. Notifications queued when a process crashes are lost, and with several replicas each sends the notifications for the deployments it works on. The deployment history in Postgres is the record to rely on.
- **Observability.** Failures are logged at `warn` with the target name, type, event, app, deployment ID, number of attempts and the error, never the URL or secret. `doorstep_notifications_total{type, result}` counts `sent`, `failed` and `dropped` deliveries per target type; see [metrics.md](metrics.md).

## GitHub deployments

When the GitHub App is configured, doorstep mirrors each deployment as a [GitHub deployment](https://docs.github.com/en/rest/deployments/deployments) on the application repository (`source.repo`). It needs the **Deployments: Read and write** repository permission; see [github-app.md](github-app.md#permissions).

| doorstep event | GitHub deployment status |
| --- | --- |
| `deploy.queued` | `queued` |
| `deploy.started` | `in_progress` |
| `deploy.succeeded`, `deploy.rolled_back` | `success` |
| `deploy.failed` | `failure` |
| `deploy.cancelled`, `deploy.superseded` | `inactive` |

Lock events are not reported to GitHub.

- **Environment.** The GitHub environment is the app name. Model environments as separate apps (`blog-staging`, `blog-prod`) and each gets its own GitHub environment, even though they share a repository. GitHub creates an environment the first time a deployment names it.
- **Deployment.** One GitHub deployment per doorstep deployment, for the full commit SHA, with task `deploy`, `auto_merge` off and no required status contexts (doorstep runs its own check gate). Its payload is `{"doorstep_deployment_id": 42, "kind": "deploy"}`, plus `rollback_of_id` for rollbacks.
- **Statuses.** Statuses are posted with `auto_inactive`, so a successful deployment marks the previous one in that environment inactive, and with the deployment's doorstep link as `log_url` when `server.public_url` is set.
- **Rollbacks.** A rollback gets its own GitHub deployment, described as `Rollback of #41`. Its `ref` and `sha` are the full SHA of the rollback's own commit, which is the commit whose image it re-pins (the last good deployment's), not the commit of the deployment it reverts. The reverted deployment keeps its `failure` (or `inactive`, if it was cancelled) status. When the rollback succeeds, `auto_inactive` marks the earlier successful deployments of the environment inactive and the rollback becomes the active deployment, so the environment shows the re-pinned commit, which is what is running.
- **Descriptions.** Status descriptions are short and fixed (`Deploying`, `Failed`, `Rollback failed`, `Superseded by #44`). GitHub shows them to everyone who can read the repository, which is everyone for a public one, so failure reasons, which can name internal hosts, are never posted there; follow `log_url` to doorstep for the details.
- **Restarts and replicas.** doorstep does not store GitHub deployment IDs. It looks the deployment up by commit, environment and task and matches `doorstep_deployment_id` in its payload, creating it only when none matches, and remembers up to 1024 IDs in memory until the deployment ends. Any replica, before or after a restart, finds the same deployment.
- **Skipped.** Deployments without a valid commit SHA or `owner/name` repository are not reported.

To turn GitHub deployments off for one app, for example a repository whose `on: deployment` workflows should not run, set `apps[].source.deployments: false`.

If the App lacks the permission, or is not installed on the repository, GitHub answers `403` or `404`. doorstep does not retry, logs one `cannot report deployments to github` warning per repository per hour with a hint, counts the failure and carries on deploying.

A `403` that is a rate limit is not a permission problem. When GitHub sends `x-ratelimit-remaining: 0` (the primary limit), a `Retry-After` header or a message about a secondary rate limit, doorstep retries like any transient error: it waits for `Retry-After`, or until `x-ratelimit-reset` for the primary limit, up to 30 seconds, and otherwise backs off as usual. The same applies to `429`. If every attempt is rate limited, the failure is logged as an ordinary delivery failure, without the permission hint.

Things to know about GitHub's side:

- Workflows with `on: deployment` or `on: deployment_status` triggers run for these events.
- Environment protection rules (required reviewers, wait timers) apply to environments used by GitHub Actions jobs, not to deployments created through the API, so they do not hold doorstep back.
- The repository sidebar and the *Environments* page show the latest active deployment per environment.

## Testing a target

```sh
doorstep notify test <notification-name> --config /etc/doorstep/config.yaml
```

Sends one synthetic `deploy.succeeded` to the named target, with a single attempt and the same secrets, links and HTTP client as the server. The test is sent even if the target's `events` do not include `deploy.succeeded`. The message names the first app that notifies the target and says that nothing was deployed; its deployment has ID 0, so it carries no deployment link. It prints `ok: sent deploy.succeeded to <name> (<type>)`, or the error (without the URL) and exits 1. It reads the same environment variables as `serve`, including the secrets and `DOORSTEP_PUBLIC_URL`.

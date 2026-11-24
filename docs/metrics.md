# Metrics

doorstep exposes Prometheus metrics at `GET /metrics`. The endpoint is unauthenticated, at most 4 scrapes run concurrently and each is cut off after 10 seconds. Scrape requests are logged at `debug`.

## Enabling and scraping

By default `/metrics` is served on a dedicated listener, `--metrics-listen-addr` (`DOORSTEP_METRICS_LISTEN_ADDR`, `server.metrics_listen_addr` in the configuration file, default `:9090`), and is not mounted on the main listener at all. The dedicated listener also answers `/healthz` and `/readyz`, with the same readiness checks as the main listener. The address must differ from `--listen-addr`.

The main listener is the one that receives public GitHub webhooks. Metrics reveal application names and build information and the endpoint is unauthenticated, so authentication is left to network placement: keep the metrics port off the public ingress.

To serve `/metrics` on the main listener instead, opt in explicitly with an empty address (`--metrics-listen-addr=""` or `DOORSTEP_METRICS_LISTEN_ADDR=""`). Do that only when the main listener is not reachable from the internet. The JSON API ([api.md](api.md)) is served only on the main listener, behind authentication. `/metrics`, `/healthz` and `/readyz` on the main listener are not behind authentication, so a scraper or probe that sends an `Authorization` header still gets an answer.

Prometheus `scrape_config`:

```yaml
scrape_configs:
  - job_name: doorstep
    static_configs:
      - targets: ["doorstep.doorstep.svc:9090"]
```

Prometheus Operator `ServiceMonitor` (the Service must expose a port named `metrics`):

```yaml
apiVersion: monitoring.coreos.com/v1
kind: ServiceMonitor
metadata:
  name: doorstep
spec:
  selector:
    matchLabels:
      app.kubernetes.io/name: doorstep
  endpoints:
    - port: metrics
      path: /metrics
      interval: 30s
```

## Metrics

The [deployment engine](engine.md#notifications-and-metrics) counts `doorstep_deployments_started_total` when a deployment starts (`queued → resolving`) and observes `doorstep_deployment_duration_seconds` for every final outcome it performs or announces: `succeeded` and `failed` for deploys, `rolled_back` and `rollback_failed` for rollbacks, `cancelled` and `superseded`; a deployment that never started is observed with a duration of zero. Both are recorded once per deployment, however many [retry attempts](engine.md#retry-before-rollback) it took; `doorstep_deployment_retries_total` counts each retry. Gate series come from the gate evaluator. `doorstep_rollbacks_total{reason="manual"}` is incremented by every accepted rollback request ([rollback.md](rollback.md)). `serve` creates every configured app's labelled series at zero on start (`Recorder.PrimeApp`) so `rate()` and `increase()` see the first event. `doorstep_migration_detection_failures_total` is incremented by intake. Every `doorstep_notifications_total`, `doorstep_webhook_deliveries_total` and `doorstep_intake_poll_cycles_total` series is created at zero on start.

| Metric | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `doorstep_deployments_started_total` | counter | `app` | Deployments that started. |
| `doorstep_deployment_duration_seconds` | histogram | `app`, `outcome` | Time from deployment start to a final outcome. Its `_count` is the number of finished deployments per outcome. |
| `doorstep_deployment_retries_total` | counter | `app`, `class` | Failed attempts that doorstep retried on the same version, by the classifier's verdict (`transient` or `unknown`). Many `transient` retries point at flaky nodes or a slow registry. `transient` and `unknown` are primed at zero for every app. |
| `doorstep_rollbacks_total` | counter | `app`, `reason` | Rollbacks started. |
| `doorstep_gate_evaluation_duration_seconds` | histogram | `app`, `gate`, `phase`, `result` | Time from a gate starting to wait to its verdict, or the duration of a call that failed (see [Labels](#labels)). Its `_count` is the number of verdicts and evaluation errors. |
| `doorstep_notifications_total` | counter | `type`, `result` | Notification deliveries. `sent` after the target accepted it (possibly after retries), `failed` when every attempt failed or the failure was permanent, `dropped` when it was never attempted because the target's queue was full or doorstep was shutting down. See [notifications.md](notifications.md#delivery). |
| `doorstep_webhook_deliveries_total` | counter | `event`, `result` | GitHub webhook deliveries received at `/webhooks/github`. `event` is `push`, `check_run`, `check_suite`, `status`, `installation`, `installation_repositories`, `ping` or `other`. `result` is `processed`, `ignored` (an event or ref doorstep does not act on), `duplicate` (a push delivery ID already handled), `in_progress` (`409`, a push delivery ID another request is handling), `rejected` (`400` or `413`), `unauthorized` (missing or wrong signature) or `failed` (`500`). A rising `unauthorized` count usually means the webhook secrets differ ([github-app.md](github-app.md#webhooks)). |
| `doorstep_intake_poll_cycles_total` | counter | `result` | Intake poll cycles this replica ran after claiming them, `succeeded` or `failed`. Replicas that did not win the claim count nothing, so sum across replicas. See [github-app.md](github-app.md#polling). |
| `doorstep_migration_detection_failures_total` | counter | `app` | Green commits of an app with a `migrations` policy that were not queued because their migration flags could not be read (GitHub or the database failed). Intake retries on the next event or poll, so a steadily rising count means the app's deploys are stuck, usually for a missing *Pull requests: read* permission. See [rollback.md](rollback.md#detection). |
| `doorstep_queue_depth` | gauge | none | Deployments waiting in the queue, read from the database at scrape time: queue entries whose deployment is `queued` and not leased, across all apps. In-flight deployments are not counted ([queue.md](queue.md#observability)). The read is cut off after 2 seconds; the series is omitted for a scrape if the database cannot be read, rather than reported as zero. |
| `doorstep_queue_depth_errors_total` | counter | none | Scrapes where the queue depth could not be read. The first failure of a streak is logged at `warn` and the recovery at `info`; further failures are only counted. |
| `doorstep_build_info` | gauge | `version`, `commit`, `go_version` | Always 1; identifies the running build. |
| `go_*`, `process_*` | various | | Standard Go runtime and process collectors. `process_*` is only available on platforms that support it (Linux). |

### Labels

`app` is the application name from the configuration and `gate` is the gate kind, never a user-defined instance name. Each gate type declares its kind when it is registered with the gate registry (for example `checks` as `ci`, `health` as `argocd`). A gate records one `passed` or `failed` observation when it reaches that verdict, measured from when it started waiting to the verdict, including the call that produced the verdict (so a gate that decides on its first evaluation records the duration of that call, and a timeout is a `failed` observation of about the timeout), and one `error` observation, with the duration of that call, each time it cannot be evaluated. Polls that leave a gate `pending` and gates skipped for a rollback record nothing. See [gates.md](gates.md#metrics). There are no labels for environment, repository, owner, commit, digest, deployment ID or error text, so the series count stays bounded.

The other labels are fixed sets. Any other value is recorded as `unknown`, except `event`, where any other webhook event is recorded as `other`.

| Label | Values |
| --- | --- |
| `outcome` | `succeeded`, `failed` (failed before anything was pinned, nothing to roll back), `rolled_back`, `rollback_failed`, `cancelled`, `superseded`, `unknown` |
| `reason` | `gate_failed`, `sync_failed`, `manual`, `unknown` |
| `class` | `transient`, `version`, `configuration`, `unknown` |
| `phase` | `deploy` (verifying a deployment), `rollback` (verifying a rollback), `unknown` |
| `gate` | `ci`, `lock` (a manual freeze), `schedule` (a deploy window), `argocd` (Argo CD synced and healthy), `soak`, `metrics` (a metric query), `manual` (a manual hold), `job` (a validation Job), `external` (a result posted through the API), `backup` (a pre-deploy backup webhook), `unknown` |
| `result` | gates: `passed`, `failed`, `error` (the gate could not be evaluated), `unknown`; notifications: `sent`, `failed`, `dropped`, `unknown`; webhooks: `processed`, `ignored`, `duplicate`, `rejected`, `unauthorized`, `failed`, `unknown`; poll cycles: `succeeded`, `failed`, `unknown` |
| `event` | `push`, `check_run`, `check_suite`, `status`, `installation`, `installation_repositories`, `ping`, `other` |
| `type` | `slack`, `webhook`, `github` (deployment statuses and the gate commit status mirror), `unknown` |

### Histogram buckets

| Metric | Buckets (seconds) |
| --- | --- |
| `doorstep_deployment_duration_seconds` | 5, 10, 30, 60, 120, 300, 600, 900, 1800, 3600 |
| `doorstep_gate_evaluation_duration_seconds` | 0.1, 0.5, 1, 5, 15, 60, 300, 900, 1800 |

## Several replicas

Counters and histograms are per process: sum them across replicas (`sum by (app) (...)`). The queue depth comes from the shared database, so every replica reports the same value; use `max(doorstep_queue_depth)`, not `sum`.

## PromQL examples

Deployment failure ratio per app over the last day (cancelled and superseded deployments are not failures):

```promql
sum by (app) (increase(doorstep_deployment_duration_seconds_count{outcome=~"failed|rolled_back|rollback_failed"}[1d]))
/
sum by (app) (increase(doorstep_deployment_duration_seconds_count[1d]))
```

p95 deployment duration:

```promql
histogram_quantile(0.95, sum by (app, le) (rate(doorstep_deployment_duration_seconds_bucket{outcome="succeeded"}[1d])))
```

Rollbacks per app and reason in the last week:

```promql
sum by (app, reason) (increase(doorstep_rollbacks_total[7d]))
```

Apps whose deploys needed retries in the last day, by verdict:

```promql
sum by (app, class) (increase(doorstep_deployment_retries_total[1d])) > 0
```

p95 gate latency per gate kind:

```promql
histogram_quantile(0.95, sum by (gate, le) (rate(doorstep_gate_evaluation_duration_seconds_bucket[1h])))
```

Notifications that are not getting through (failed or dropped in the last 30 minutes, by target type):

```promql
sum by (type) (increase(doorstep_notifications_total{result=~"failed|dropped"}[30m])) > 0
```

A `github` failure usually means the App lacks the *Deployments* or *Commit statuses* permission on a repository; the logs name it (`cannot report deployments to github` or `cannot mirror gates to github`).

Queue alerts:

```promql
max(doorstep_queue_depth) > 10
absent(doorstep_queue_depth)
```

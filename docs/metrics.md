# Metrics

doorstep exposes Prometheus metrics at `GET /metrics`. The endpoint is unauthenticated, at most 4 scrapes run concurrently and each is cut off after 10 seconds. Scrape requests are logged at `debug`.

## Enabling and scraping

By default `/metrics` is served on a dedicated listener, `--metrics-listen-addr` (`DOORSTEP_METRICS_LISTEN_ADDR`, `server.metrics_listen_addr` in the configuration file, default `:9090`), and is not mounted on the main listener at all. The dedicated listener also answers `/healthz` and `/readyz`, with the same readiness checks as the main listener. The address must differ from `--listen-addr`.

The main listener is the one that receives public GitHub webhooks. Metrics reveal application names and build information and the endpoint is unauthenticated, so authentication is left to network placement: keep the metrics port off the public ingress.

To serve `/metrics` on the main listener instead, opt in explicitly with an empty address (`--metrics-listen-addr=""` or `DOORSTEP_METRICS_LISTEN_ADDR=""`). Do that only when the main listener is not reachable from the internet. Until API authentication exists, the dedicated listener also serves the `/api/v1/` JSON API ([api.md](api.md)); with an empty address the API is disabled.

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

Deployment, rollback and gate series appear once the engine records into them. Until then `doorstep_build_info`, the queue depth series (`serve` reads it from the queue), the Go runtime and the process metrics are emitted. Once an application is known, its labelled series are created at zero (`Recorder.PrimeApp`) so `rate()` and `increase()` see the first event.

| Metric | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `doorstep_deployments_started_total` | counter | `app` | Deployments that started. |
| `doorstep_deployment_duration_seconds` | histogram | `app`, `outcome` | Time from deployment start to a final outcome. Its `_count` is the number of finished deployments per outcome. |
| `doorstep_rollbacks_total` | counter | `app`, `reason` | Rollbacks started. |
| `doorstep_gate_evaluation_duration_seconds` | histogram | `app`, `gate`, `phase`, `result` | Time spent evaluating a gate. Its `_count` is the number of evaluations. |
| `doorstep_queue_depth` | gauge | none | Deployments waiting in the queue, read from the database at scrape time: queue entries whose deployment is `queued` and not leased, across all apps. In-flight deployments are not counted ([queue.md](queue.md#observability)). The read is cut off after 2 seconds; the series is omitted for a scrape if the database cannot be read, rather than reported as zero. |
| `doorstep_queue_depth_errors_total` | counter | none | Scrapes where the queue depth could not be read. The first failure of a streak is logged at `warn` and the recovery at `info`; further failures are only counted. |
| `doorstep_build_info` | gauge | `version`, `commit`, `go_version` | Always 1; identifies the running build. |
| `go_*`, `process_*` | various | | Standard Go runtime and process collectors. `process_*` is only available on platforms that support it (Linux). |

### Labels

`app` is the application name from the configuration and `gate` is the gate kind, never a user-defined instance name. Each gate type declares its kind when it is registered with the gate registry (for example `checks` as `ci`, `health` as `argocd`). A gate records one `passed` or `failed` observation when it reaches that verdict, measured from when it started waiting to the verdict (so a timeout is a `failed` observation of about the timeout), and one `error` observation, with the duration of that call, each time it cannot be evaluated. Polls that leave a gate `pending` and gates skipped for a rollback record nothing. See [gates.md](gates.md#metrics). There are no labels for environment, repository, owner, commit, digest, deployment ID or error text, so the series count stays bounded.

The other labels are fixed sets. Any other value is recorded as `unknown`.

| Label | Values |
| --- | --- |
| `outcome` | `succeeded`, `failed` (failed before anything was pinned, nothing to roll back), `rolled_back`, `rollback_failed`, `cancelled`, `superseded`, `unknown` |
| `reason` | `gate_failed`, `sync_failed`, `manual`, `unknown` |
| `phase` | `deploy` (verifying a deployment), `rollback` (verifying a rollback), `unknown` |
| `gate` | `ci`, `lock`, `argocd` (Argo CD synced and healthy), `soak`, `metrics` (a metric query), `manual` (a manual hold), `unknown` |
| `result` | `passed`, `failed`, `error` (the gate could not be evaluated), `unknown` |

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

p95 gate latency per gate kind:

```promql
histogram_quantile(0.95, sum by (gate, le) (rate(doorstep_gate_evaluation_duration_seconds_bucket[1h])))
```

Queue alerts:

```promql
max(doorstep_queue_depth) > 10
absent(doorstep_queue_depth)
```

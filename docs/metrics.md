# Metrics

doorstep exposes Prometheus metrics at `GET /metrics`. The endpoint is unauthenticated, at most 4 scrapes run concurrently and each is cut off after 10 seconds. Scrape requests are logged at `debug`.

## Enabling and scraping

By default `/metrics` is served on the main listener (`--listen-addr`, default `:8080`), so a homelab needs no extra configuration.

Set `--metrics-listen-addr` (`DOORSTEP_METRICS_LISTEN_ADDR`) to serve it on a dedicated address instead, for example `:9090`. The endpoint is then not mounted on the main listener at all. The dedicated listener also answers `/healthz` and `/readyz`.

Use the dedicated listener whenever the main listener is reachable from the internet (for GitHub webhooks, for instance). Metrics reveal application names and build information, and authentication is left to network placement: keep the metrics port off the public ingress.

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

Deployment, rollback, gate and queue series appear once the engine and queue features record into them. Until then only `doorstep_build_info`, the Go runtime and the process metrics are emitted.

| Metric | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `doorstep_deployments_started_total` | counter | `app` | Deployments that started. |
| `doorstep_deployment_duration_seconds` | histogram | `app`, `outcome` | Time from deployment start to a final outcome. Its `_count` is the number of finished deployments per outcome. |
| `doorstep_rollbacks_total` | counter | `app`, `reason` | Rollbacks started. |
| `doorstep_gate_evaluation_duration_seconds` | histogram | `app`, `gate`, `phase`, `result` | Time spent evaluating a gate. Its `_count` is the number of evaluations. |
| `doorstep_queue_depth` | gauge | none | Deployments waiting in the queue, read from the database at scrape time. Omitted for a scrape if the database cannot be read (a warning is logged), rather than reported as zero. |
| `doorstep_build_info` | gauge | `version`, `commit`, `go_version` | Always 1; identifies the running build. |
| `go_*`, `process_*` | various | | Standard Go runtime and process collectors. `process_*` is only available on platforms that support it (Linux). |

### Labels

`app` is the application name from the configuration and `gate` is the gate kind (`health`, `metrics` or `soak`), never a user-defined instance name. There are no labels for environment, repository, owner, commit, digest, deployment ID or error text, so the series count stays bounded.

The other labels are fixed sets. Any other value is recorded as `unknown`.

| Label | Values |
| --- | --- |
| `outcome` | `succeeded`, `failed` (failed before anything was pinned, nothing to roll back), `rolled_back`, `rollback_failed`, `cancelled`, `superseded`, `unknown` |
| `reason` | `gate_failed`, `sync_failed`, `manual`, `unknown` |
| `phase` | `deploy` (verifying a deployment), `rollback` (verifying a rollback), `unknown` |
| `result` | `passed`, `failed`, `error` (the gate could not be evaluated), `unknown` |

### Histogram buckets

| Metric | Buckets (seconds) |
| --- | --- |
| `doorstep_deployment_duration_seconds` | 5, 10, 30, 60, 120, 300, 600, 900, 1800, 3600 |
| `doorstep_gate_evaluation_duration_seconds` | 0.1, 0.5, 1, 5, 15, 60, 300, 900, 1800 |

## Several replicas

Counters and histograms are per process: sum them across replicas (`sum by (app) (...)`). The queue depth comes from the shared database, so every replica reports the same value; use `max(doorstep_queue_depth)`, not `sum`.

## PromQL examples

Deployment failure ratio per app over the last day:

```promql
sum by (app) (increase(doorstep_deployment_duration_seconds_count{outcome!="succeeded"}[1d]))
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

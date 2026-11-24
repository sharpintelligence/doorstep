# 0013: Prometheus query gate

- Status: Proposed
- Date: 2026-11-24

## Context

The `health` and `soak` gates only see what Argo CD reports. A release can be synced, healthy and still serve more errors or slower responses than the one it replaced. doorstep needs an after gate that reads application metrics and fails the deploy, which rolls it back automatically, when they get worse.

Several choices here are hard to change later. The configuration keys become part of every file that uses the gate. The pass and fail semantics decide when a release is rolled back, and a false failure rolls back a good release. The client decides which dependencies sit on the engine path and where credentials can go. Constraints: gates see only their own previous result, state lives in gate evidence (no gate-owned tables), several replicas may evaluate the same gate, the evaluator gives a gate call 30 seconds, and secrets are never inline in the configuration.

## Decision

- **A `prometheus` gate type**, after phase only, metrics kind `metrics`, skipped on rollbacks (a rollback re-pins a digest that already passed, and a metric failing during a rollback has no good outcome, the same reasoning as `soak`). It lives in `internal/gates` and is registered by its own `gates.RegisterPrometheus` call over a `MetricsQuerier` port, so it stays optional and separate from `RegisterBuiltins`.
- **Servers are a top-level named list**, `prometheus: [{name, url, bearer_token_env | bearer_token_file | username + password_env | password_file, allow_http_auth}]`, and a gate names one with `server`, which may be left out when exactly one is configured. Credentials are bound to the URL they are configured with, read on every request and checked at startup like other secret references. Redirects are not followed. Credentials over plain `http` to a non-loopback host are rejected unless `allow_http_auth: true` says the path is trusted.
- **Count-based sampling with probe vocabulary.** The gate takes at most one instant-query sample per `interval` (default `1m`, `10s` to `1h`) after `initial_delay` (default `0s`, at most `24h`). It passes after `success_threshold` (default `5`) consecutive successful samples and fails after `failure_threshold` (default `3`) consecutive failing samples; each streak resets the other. The default timeout is `initial_delay` + `success_threshold` × `interval` + `10m`, and a timeout that could never pass is rejected at load. The duration that each sample measures is the range selector in the query.
- **A sample succeeds when every configured comparison holds**: `condition` (`"<op> <number>"`, parsed in `config`) and `baseline`. At least one is required.
- **The baseline is one fixed number**: the mean of the same query over [*anchor* - `window`, *anchor*] at step `interval`, where *anchor* is the deployment's start (creation as a fallback), computed once and stored in evidence. A sample passes when it is within *baseline* ± \|*baseline*\| × `ratio` ± `absolute` in the configured `direction` (`increase`, the default, means higher is worse). An empty baseline is skipped and reported (`baseline.no_data: ignore`, the default) or fails the gate (`fail`).
- **No data and errors never pass.** An empty result or NaN is no data: by default it counts neither way and the timeout bounds the wait; `no_data: fail` counts it as a failure. A query Prometheus rejects as `bad_data` (`400` or `422`) fails the gate at once, because it fails the same way every time. Other network errors, error responses and wrong result shapes (anything but a scalar or a single series) leave the gate `pending` with the reason, recorded in evidence with the attempt time so the interval throttle still applies; they count neither way, and an outage that outlasts the timeout fails the gate.
- **All state is in evidence**, carried only from a `pending` previous result: start time, last attempt, streaks, the baseline and the last 10 samples, with values as strings because JSON cannot hold NaN or infinities. A SHA-256 of the full query detects a changed query, since the stored query is cut to the 1024 byte message limit. Times are compared with a 5 second clock skew tolerance between replicas.
- **A hand-written standard library client**, `internal/promquery`: `POST` form requests to `/api/v1/query` and `/api/v1/query_range`, a 10 second timeout per request, a 1 MiB response limit, typed API errors. No PromQL parsing: the configuration checks only size and encoding, and Prometheus reports a bad query.
- **No database migration.**

## Options considered

### Time-window semantics ("good for 5 minutes")

Natural to describe, but ambiguous when Prometheus is down for part of the window: does an unreadable minute count? Counting samples makes every counted success a real reading, is deterministic and is trivially testable. Kubernetes probes already taught the audience `success_threshold` and `failure_threshold`.

### Fixed thresholds only

Simple, but an absolute error rate that is fine for one app is an outage for another, and users would hard-code today's level. A pre-deploy baseline answers "did this release make it worse?" without tuning. Thresholds remain available and combine with the baseline as a ceiling.

### An `offset` baseline (compare with the same query `offset 1h`)

Needs no extra query, but the comparison point keeps moving: after an hour the "baseline" includes post-deploy data, and daily seasonality makes one offset wrong for another. One range query before the deploy, averaged to remove single-sample noise, is fixed for the whole deployment and costs one extra request. A configurable offset was dropped as unneeded.

### Ratio-only margins

`baseline × 1.2` is zero when the baseline is zero, so the first error after a clean period fails the deploy. Adding an `absolute` margin fixes that without special cases.

### A `pass` option for no data

Convenient for low-traffic apps, but a misspelt metric would pass every deploy forever. `or vector(0)` in PromQL states the same intent explicitly, per query.

### Per-series evaluation

Evaluating each series (per pod, per route) needs label matching between baseline and samples and a rule for series that appear or vanish. Requiring one series, aggregated in PromQL, keeps one rule for thresholds and baselines and makes the intent visible in the query.

### Returning Go errors for Prometheus failures

The evaluator's error path keeps the previous evidence, so the attempt time would not be recorded and a Prometheus that is down would be queried on every poll by every replica. Recording the attempt as a `pending` result with the reason gives the same outcome (pending until the timeout) with a durable throttle. The cost is that these failures do not appear as `error` observations in the gate duration metric.

### `prometheus/client_golang/api` or the PromQL parser

The official API client pulls `prometheus/common` model types into the engine path for two endpoints and one JSON envelope. Validating PromQL at load would need `prometheus/prometheus`, a very large module. The hand-written client is a few hundred lines with its own tests; a bad query fails the gate at once with Prometheus's own message.

### Per-gate URLs

Shorter for a single app, but credentials would then travel with whatever URL a gate names, and one global token could be sent to another host. Named servers bind each credential to one URL.

## Consequences

- Operators can roll back on error rates, latency or any metric they already collect, with no gate-specific tables and no new dependencies.
- The gate is only as good as its query. A query with a syntax error fails the first deploy that uses it, which rolls that deploy back. A query that returns no data waits until its timeout; one that returns several series stays pending with a message saying to aggregate it. Both are visible in the gate result.
- A first deployment of a new app has no baseline; by default it is checked against the condition only and says so in its message.
- `X-Scope-OrgID` and other custom headers, a CA file or mutual TLS, before-phase use (holding deploys while an alert fires), a baseline offset, a variance-aware baseline (a margin of `k` standard deviations of the window, z-score style, instead of a fixed ratio or absolute margin), per-series evaluation and query templating are not supported yet; each can be added without changing the existing keys.
- Prometheus failures do not show up as evaluation errors in metrics; they are visible in gate results and the UI.

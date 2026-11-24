# 0014: External gates and the commit status mirror

- Status: Proposed
- Date: 2026-11-24

## Context

Two related needs extend the gate system ([gates.md](../gates.md)):

- Tools outside doorstep, such as an end-to-end test job, a load test or a change approval system, must be able to decide a gate. They know the repository and commit, not doorstep's internals, and they authenticate with an API token.
- GitHub should show deploy state next to CI. Every gate result should appear as a commit status on the deployed commit.

Constraints from earlier decisions: the evaluator is the only writer of `gate_results` and its results are sticky ([ADR 0001](0001-go-and-core-design.md), [ADR 0011](0011-built-in-gates-and-deploy-schedules.md)); roles are one ordered list and every action is authorised by one comparison ([ADR 0007](0007-authentication-and-roles.md)); every audited action writes its audit row in the action's transaction; and reporting to GitHub is best effort and never holds a deploy ([ADR 0006](0006-notifications-are-best-effort.md)).

## Decision

1. **Reports, not results.** A posted result is stored in a new table, `gate_reports`, one row per deployment and gate, written only by the API. A gate type `external` reads that row through a port and returns it as its verdict. The evaluator stays the only writer of `gate_results`. A `pending` report may be replaced; a `passed` or `failed` report is final, so the same verdict again is an idempotent success and a different one is a conflict, as is any report for a gate whose recorded result is already final. The report and its `gate.report` audit row are written in one transaction, and identical retries write nothing.
2. **A `reporter` role.** Roles become `viewer < reporter < deployer < admin`. Posting a report needs `reporter`, so a CI token can post results but cannot roll back, freeze or cancel. Deployers and admins can post too, which is an audited manual override for a stuck external gate.
3. **Two endpoints over one service.** `POST /api/v1/deployments/{id}/gates/{gate}` is canonical; `POST /api/v1/apps/{app}/commits/{sha}/gates/{gate}` resolves the newest unfinished deployment of the app whose own commit is `sha`. Acceptance is checked against the configuration: the gate must be an `external` gate of the deployment's app, the deployment unfinished, a rollback only when the gate sets `apply_to_rollbacks`, and an after-deploy gate only while the deployment is `verifying`.
4. **A latest-wins, best-effort mirror.** `notify.GateStatuses` is a gate observer that posts `doorstep/<app>/<phase>/<gate>` commit statuses. It keeps only the latest desired status per repository, commit and context, queues the key once (at most 1024 keys) and delivers keys in order from one worker with the notification retry policy. There is no durable outbox.
5. **A reserved context prefix.** Commit statuses whose context starts with `doorstep/` belong to doorstep. Intake ignores them when it decides whether a commit is green, the `status` webhook ignores them, and `source.required_checks` may not name them.

## Options considered

### Writing `gate_results` from the API

The simplest storage: the API upserts the gate result directly. It races with the evaluator, which computes a pending candidate and upserts it over the posted verdict; it bypasses the observers and metrics, so the commit status would never change; and `Reevaluate` would clobber it. A report table makes the gate a pure, idempotent read, so restarts, `Reevaluate`, sticky results, timeouts, events, metrics and the mirror all work unchanged. The cost is that a verdict takes effect on the engine's next poll instead of at once, a few seconds at most.

### Token scopes instead of a role

A per-token scope column (`gates:report`) would allow tokens that can report but not read, or that can report only some gates. It adds a column, CLI flags and a second authorisation concept next to roles, for little benefit at homelab scale, and sessions would need a separate answer. A dedicated role needs no change to tokens, principals or `api.Caller`, and works for tokens and sessions alike. Follow-up: a per-gate `reporters` allowlist in the configuration, naming the tokens or users allowed to report a given gate, can narrow this later without a schema change.

### Durable outbox for commit statuses

A table of pending statuses delivered by a claimer would survive crashes. Commit statuses are a view of state doorstep already records durably; the next change of the gate rewrites the status. As with notifications, an outbox is a lot of machinery for the rare crash between a gate change and its status, so the mirror is in memory and best effort.

### A queue of every status change

Queuing each change, as the notification dispatcher does, keeps every intermediate state but lets a slow GitHub build up a backlog, and a dropped or reordered delivery could leave an older state (`pending`) on top of a newer one (`success`). Coalescing to the latest status per context bounds memory by the number of contexts and makes the final state the one GitHub ends up with.

### Not reserving the prefix

Without it, with `required_checks` empty (every reported status must pass), doorstep's own pending `doorstep/<app>/before/checks` status would hold the `checks` gate forever, and a failed after-deploy status would block retries and every other app tracking the same commit. Listing `required_checks` avoids the loop, but the default must be correct, so the prefix is reserved.

## Consequences

- External tools can hold or fail a deployment with a token that cannot do anything else, and every change they make is audited.
- After-deploy external gates without a timeout can hold a live deployment indefinitely; the documentation recommends always setting one, and before-deploy external gates may wait for days by design.
- The mirror needs *Commit statuses: Read and write* on application repositories; repositories that cannot grant it opt out with `source.statuses: false`. Missing permissions are logged once per repository per hour and never affect deploys.
- A crash can lose the latest status of a gate until it next changes. Gate results in doorstep stay authoritative.
- Any integration that posts statuses under `doorstep/` is ignored by intake; that namespace is doorstep's.

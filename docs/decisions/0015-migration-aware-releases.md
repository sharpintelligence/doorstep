# 0015: Migration-aware releases

- Status: Proposed
- Date: 2026-11-24

## Context

A rollback re-pins an earlier image ([ADR 0002](0002-rollback-is-a-new-deployment.md)). When the release being rolled back ran database migrations, the earlier image meets a schema it was not written for. Rolling back is still often right (the migration was additive, the new code is the problem), but it must be a decision a person makes knowingly, and an automatic rollback after a failed gate must not make it for them. Operators also want a fresh database backup taken right before such a release goes out.

Constraints: doorstep never writes to the cluster, git is the source of truth for what is pinned, intake drops stale pushes and supersedes commits that never went green (so a release can contain merges that never had a deployment of their own), the gate evaluator calls every pending gate of a phase on every pass with a 30 second cap per call, and `gates` may not import `notify`.

## Decision

- **The flag describes the whole release.** When a commit's checks turn green, intake inspects every commit between the app's previously enqueued deploy commit (its newest `deploy` row) and the new head: one GitHub compare call (at most 50 commits) and, when labels are configured, one `commits/{sha}/pulls` call per commit. Without a usable base (no earlier deploy, compare `behind`, `identical` or `404`) only the head is inspected. A range of more than 50 commits is flagged conservatively.
- **Recognition is opt-in per app** (`apps[].migrations`): pull request labels, counted only on merged pull requests into the app's branch, and a `Doorstep-Migration: true|yes` commit trailer, on by default when the block exists. Without the block nothing is detected and no extra API calls are made.
- **Detection fails closed.** It runs in `intake.Service.settle`, outside the Postgres transaction, only for a `success` evaluation. On any GitHub error the commit is neither settled nor enqueued and the next check event or poll retries.
- **The flag lives on the deployment** (`deployments.migration`, `migration_reason`), not on tracked commits. Superseded deployments keep their rows, so batching loses nothing.
- **Crossing is pure SQL.** A rollback from `current` to `to` crosses the target's flagged `deploy` rows created after `to`'s commit was first enqueued, not of `to`'s commit, and no newer than `current` (`Store.MigrationsBetween`). Rolling back to a flagged release, or forward, crosses nothing.
- **Confirmation.** A crossing rollback without `confirm_migration_rollback` (CLI `--confirm-migration-rollback`) answers `409 migration_confirmation_required` and writes nothing. The `deployer` role suffices. A confirmed rollback is itself flagged and audits the crossed deployments.
- **Automatic rollbacks across a flagged release are withheld**: the engine fails the deploy with a reason telling the operator how to roll back by hand, and notifies.
- **The backup is a webhook gate.** A `before` gate of type `backup` POSTs a signed JSON request to an operator-configured endpoint and polls by re-POSTing it with a stable `Idempotency-Key` until the receiver reports success. It runs for flagged releases by default, for rollbacks only on request, and is *deferred*: the evaluator neither calls nor records it until every earlier gate of the phase has passed or been skipped in that pass. It fails closed on its timeout (default 30 minutes).

## Options considered

- **Head commit only.** One call fewer, but busy merge periods are exactly when a migration commit is superseded without its own deployment, so the guard would be wrong when it matters most. Rejected.
- **Flag on tracked commits as well.** A second source of truth and a change to the settle signature, for no reader that needs it. Rejected.
- **Fail open on GitHub errors.** Keeps deploys moving during a GitHub outage, but an operator who opted in relies on the guard; a stalled deploy with a loud log line is noticed, a silently unflagged one is not. Rejected.
- **A generic `confirm` field.** It would become a blanket override for every future prompt. A field named for the risk mirrors the CLI flag and cannot be reused by accident.
- **doorstep runs the backup as a Kubernetes Job.** It would make doorstep write to the cluster, against its core principle. An outbound request to an operator-owned endpoint is not a cluster write; whatever runs the backup (a Job launcher, a NAS script, a managed database API) belongs to the operator.
- **A synchronous webhook, or a callback route.** The evaluator caps a gate call at 30 seconds, so a long synchronous request is impossible; a callback needs a new authenticated inbound route. One idempotent endpoint polled with the same request serves trivial receivers (answer `200` when done) and asynchronous ones (`202`, then `200`). Operators who prefer push-style completion can use the external gate API separately.
- **No deferral.** The evaluator would fire the backup while CI is still running or a deploy window holds the deploy for days, producing a stale backup and burning its timeout.

## Consequences

- Rollbacks across migrations need one extra, explicit step; automatic recovery is not available for flagged releases.
- Apps that opt in need *Pull requests: read* on their application repositories for labels, and their deploys stop while GitHub cannot be read.
- Over-flagging (a long range, a lenient trailer match) costs only a confirmation or a backup.
- Deploys requested by other paths are unflagged until those paths add detection.
- Backup receivers must be idempotent on the key, or answer within 20 seconds.

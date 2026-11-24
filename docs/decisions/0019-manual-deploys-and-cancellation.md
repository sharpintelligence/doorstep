# 0019: Manual deploys and cancellation

- Status: Proposed
- Date: 2026-11-24

## Context

Merges deploy themselves through intake. Operators still need to deploy a specific commit by hand (redeploy a known good build, push a hotfix ahead of the queue) and to stop a deployment that is queued or running. The API, the web UI and the CLI all need these two actions, and they must behave identically. [ADR 0001](0001-go-and-core-design.md) makes the merge the approval and [ADR 0007](0007-authentication-and-roles.md) says no role can make doorstep deploy an unmerged commit. GitHub serves commits of forks and pull requests through the parent repository's API, so knowing that a SHA exists in the repository proves nothing about review. Releases may be flagged as database migrations ([ADR 0015](0015-migration-aware-releases.md)); the backup gate and the rollback confirmation rely on that flag. The engine already stops quietly when a compare-and-set loses, and closes pull requests of cancelled deployments that had started, from a post-commit announcement.

## Decision

1. **One command service, `internal/deploys`, modelled on `internal/rollback`.** `Deploy(Request) (Result, error)` and `Cancel(CancelRequest) (Deployment, error)` run over a `Repository` port with `InTx` that `*store.Store` satisfies, and write their audit row (`deploy.request`, `deploy.cancel`) in the same transaction as the change. It imports only `core` and `store`. `cli` wraps it in `announcingDeploys`, which calls `Engine.Queued` for a created deployment and `Engine.Cancelled` after a cancel, once the transaction committed. The API, the UI and therefore the CLI receive the same wrapped service.
2. **Only commits on the followed branch.** The commit must be a full 40 character SHA reachable from the app's configured branch in its source repository: `cli` implements the `Commits` port with the branch head and a GitHub compare (`HeadContainsBase`). An unknown commit is `404`, a commit not on the branch `409 commit_not_on_branch`. Required checks are not re-read at request time; the `before`-phase `checks` gate still applies when the deploy starts.
3. **Migrations and blocks as for merges.** Migration detection is intake's own, exported as `intake.Service.MigrationFlag` and reached through a `Migrations` port. It runs before the transaction and fails closed: if it or GitHub cannot answer, nothing is queued and the request answers `503` with `Retry-After`. Inside the transaction, after locking the app's row, a block on the commit refuses the request with `409 blocked`.
4. **Deduplicated, optionally urgent, attributed to the API.** The deploy is queued through `EnqueueDeployment` at normal or urgent priority; an active deploy of the same commit is returned instead (`200`, `created: false`, priority raised when urgent). The source is `api` for all three entry points; `requested_by` tells a user from a token.
5. **Any unfinished deployment can be cancelled.** Cancel reads the deployment, refuses a terminal one with `409 not_cancellable`, locks the app's row and transitions from the state it read to `cancelled` with the message `cancelled by <actor>[: reason]`, retrying up to three times on a state conflict. Deploys and rollbacks alike; no state is excluded.
6. **Endpoints.** `POST /api/v1/apps/{app}/deployments` (`{commit_sha, urgent, reason}`) and `POST /api/v1/deployments/{id}/cancel` (`{reason}`), both for `deployer`; `doorstep deploy` and `doorstep cancel` call them.

## Options considered

### Accept any commit that exists in the repository

Simple, but a deployer could then ship a commit from a fork or an unmerged pull request, which ADR 0001 rules out. The compare primitive already exists for Argo CD ancestry checks, so the guard costs two GitHub calls.

### Re-evaluate required checks at request time

Duplicates the `checks` gate and would refuse a redeploy whose old check runs have expired from GitHub. The gate runs anyway before anything changes.

### Skip migration detection for manual deploys, or fail open

A hand-deployed release that skipped the flag would bypass the backup gate and the rollback confirmation, the two guards operators opted into. Reusing intake's detection keeps one implementation; failing closed matches how intake treats merges.

### A new source value such as `ui` or `manual`

Needs a schema migration for a check constraint and buys nothing that `requested_by` and the audit row do not already record. `api` is what rollback requests use.

### Restrict cancel to queued deploys

The engine already treats `cancelled` as reachable from every active state and handles the lost compare-and-set; refusing would only push operators towards a rollback when they want the deploy to stop.

## Consequences

- Manual deploys need the GitHub App to read the source repository and, for apps with migration detection, its pull requests; during a GitHub outage manual deploys are refused rather than unguarded.
- A cancelled deployment that already pinned leaves its version in git until a rollback or a newer deploy replaces it; the UI and the docs say so.
- No schema migration and no configuration: the endpoints exist whenever the engine runs.
- The API, UI and CLI cannot drift apart, because there is one service and one decorator.

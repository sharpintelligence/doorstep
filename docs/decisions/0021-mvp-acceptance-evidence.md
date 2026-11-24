# 0021: MVP acceptance evidence

- Status: Proposed
- Date: 2026-11-24

## Context

The MVP promise is: an app repository merges to `main`, CI pushes an image tagged with the commit SHA, doorstep resolves the digest, commits it to the GitOps repository, waits for Argo CD to report the Application synced and healthy, records the deploy with its lineage, notifies Slack, and a rollback through the API or the CLI restores the previous version. Queue, locks, the UI, Prometheus gates and automatic rollback build on that path.

Proving it against real systems needs a GitHub App installed on an organization, a container registry and a cluster running Argo CD. CI has none of them: the repository is public, so App credentials would be exposed to workflows that fork pull requests can influence, and secrets are not available to fork pull requests at all. doorstep writes pins through the GitHub REST Contents API, so a fake GitHub cannot back a real Argo CD, which needs a git remote to clone. Each package already has unit tests and several integration tests run `serve` end to end, but no single test followed the promise clause by clause, and none drove the rollback through the CLI or proved the Slack wiring.

## Decision

1. The MVP is accepted on three pieces of evidence:
   - the audit table below, which maps each clause of the promise to the code that implements it and the tests that prove it;
   - one hermetic acceptance test, `TestMVPDeploysAMergedPullRequestAndRollsBack` in `internal/cli/mvp_acceptance_test.go`, which runs the real `serve` composition root through `cli.Run` against real Postgres and the GitHub, registry and Argo CD fakes, and drives the real `doorstep rollback` and `doorstep blocks` commands;
   - a manual walkthrough, [mvp-walkthrough.md](../mvp-walkthrough.md), for what fakes cannot prove: a real GitHub App, a real registry such as ghcr.io and a real Argo CD.
2. The acceptance test follows real CI ordering. The push webhook arrives while the required check is still running and the image does not exist, and nothing is queued; the image is pushed, the check completes, and the `check_run` webhook queues exactly one deploy. Polling is off, so the webhook hand-off is the only thing that can start the deploy.
3. Lineage is asserted through the public API (`GET /api/v1/deployments/{id}` and `GET /api/v1/apps/{app}/deployments`), not store rows, because that is what users and tools read.
4. Slack is asserted on wiring, not formatting: a `type: slack` target with default events receives a `succeeded` message with the commit and deployment links and a `rolled back` message for the rollback. The exact rendering stays golden-tested in `internal/notify/testdata/slack`.
5. CI does not run a kind cluster with Argo CD.

## Audit

| Clause | Code path | Proving tests |
| --- | --- | --- |
| A merge to `main` reaches doorstep | `intake.WebhookHandler` verifies the signature and dispatches `push` to `(*intake.Service).HandlePush`, which tracks the commit with pending checks | `TestMVPDeploysAMergedPullRequestAndRollsBack` (forged signature rejected with `401` and nothing tracked; signed push tracked, nothing queued), `internal/intake/webhook_test.go`, `TestRunServeIntake` |
| CI turns green and the commit is queued once | `check_run`, `check_suite` and `status` webhooks call `(*intake.Service).Reevaluate`, which reads the required checks and enqueues the deployment with the image tag set to the full commit SHA | `TestMVPDeploysAMergedPullRequestAndRollsBack` (one deploy per commit after `check_run`; redelivery after the rollback queues nothing), `internal/intake/evaluate_test.go`, `internal/intake/service_test.go` |
| The digest is resolved | `engine` resolving step calls the registry resolver (`internal/registry`), waiting while the tag does not exist yet (`ErrImageNotReady`) | `TestMVPDeploysAMergedPullRequestAndRollsBack` (`image.digest` equals the pushed digest), `internal/registry/registry_test.go`, `internal/engine/engine_test.go` |
| The digest is committed to the GitOps repository | `engine` pinning step calls `gitops.Writer`, which edits the kustomization and writes one commit with the `Doorstep-*` trailers ([ADR 0005](0005-pin-commits-and-trailers.md)) | `TestMVPDeploysAMergedPullRequestAndRollsBack` (pin file content, `Doorstep-App`, `-Deployment`, `-Kind`, `-Digest`, `-URL` trailers), `internal/gitops/commit_test.go`, `internal/gitops/message_test.go` |
| Argo CD reports synced and healthy | `engine` syncing step calls `(*argocd.Watcher).WaitSynced`, which watches the Application until it has synced a revision containing the pin commit and is healthy | `TestMVPDeploysAMergedPullRequestAndRollsBack` (`argocd.revision` equals the pin commit), `TestEngineFailsOnSyncFailure`, `internal/argocd/watcher_test.go` |
| The deploy is recorded with its lineage | The store records each transition and the lineage columns; `internal/api/deployments.go` and `dto.go` expose trigger, commit, image (tag, digest, previous digest), GitOps commit, Argo CD revision, timestamps and events | `TestMVPDeploysAMergedPullRequestAndRollsBack` (every field through the API, events `queued` to `succeeded`, history newest first), `internal/api/history_test.go` |
| Slack is notified | `internal/cli/notify.go` builds a `notify.Slack` sink per `type: slack` target with deployment and commit links; the engine announces each transition ([ADR 0006](0006-notifications-are-best-effort.md)) | `TestMVPDeploysAMergedPullRequestAndRollsBack` (posts to the configured webhook path with both links), `internal/notify/slack_test.go` (golden messages), `internal/cli/notify_test.go` |
| A rollback through the API restores the previous version | `POST /api/v1/apps/{app}/rollback` calls `(*rollback.Service).Rollback`, which queues a rollback deployment and blocks the replaced digest; the engine re-pins the previous digest and mirrors the block as a `Doorstep-Block` trailer ([ADR 0002](0002-rollback-is-a-new-deployment.md), [ADR 0009](0009-blocks-in-postgres-mirrored-to-git.md)) | `TestEngineDeploysAndRollsBack`, `internal/api/rollback_test.go`, `internal/rollback/service_test.go` |
| A rollback through the CLI restores the previous version | `doorstep rollback` (`internal/cli/rollback.go`) posts to the same endpoint; `doorstep blocks` lists the block | `TestMVPDeploysAMergedPullRequestAndRollsBack` (rollback succeeds, `kind`, `rollback_of_id`, digests, `requested_by` of the token, `Doorstep-Rollback-Of` and `Doorstep-Block` trailers, block recorded in git, a `rolled back` Slack message), `internal/cli/rollback_api_test.go` |

The audit found no broken wiring on the MVP path. It found production API that only tests used, which was removed or moved into test files.

## Options considered

- **A kind cluster with Argo CD in CI.** Rejected. Argo CD needs a git remote that reflects doorstep's writes, and doorstep writes through the GitHub REST API, so the job would need either a git server behind a GitHub API emulation or real GitHub with App secrets on a public repository. It would add four to six minutes of cluster and Argo CD start-up and a new source of flakiness to every pull request, for a path (watching the Application's status) that `argocdtest` already exercises at the Kubernetes API level.
- **Real GitHub and ghcr.io from CI.** Rejected: secrets on a public repository are unavailable to fork pull requests and are a liability if a workflow is ever misconfigured.
- **A separate acceptance package that runs a built binary.** Rejected: the serve harness lives unexported in `package cli` and `cmd/doorstep` only forwards to `cli.Run`, so a new package would duplicate the harness without testing more code.
- **Relying on the existing engine tests.** Rejected: they pre-seed green checks and the image, post the rollback to the API directly and use only the generic webhook sink, so the CI hand-off, the CLI and Slack were unproven.

## Consequences

- One named test fails when any clause of the promise regresses, and CI's `check` job cannot skip it because it sets `DOORSTEP_TEST_REQUIRE_DATABASE=true`.
- The real GitHub App, registry authentication and Argo CD's git polling are proven only by the manual walkthrough, which must be repeated by hand when those integrations change.
- The fakes are now load-bearing for acceptance: a behaviour difference between `githubtest`, `registrytest` or `argocdtest` and the real service would hide a defect until the walkthrough is run.

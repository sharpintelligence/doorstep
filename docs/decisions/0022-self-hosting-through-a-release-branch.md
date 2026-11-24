# 0022: Self-hosting through a release branch

- Status: Accepted
- Date: 2026-11-24

## Context

doorstep should deploy its own upgrades like any other app, so its releases go through the same queue, gates and history. Three facts constrain how:

- doorstep resolves `image:<full commit SHA>` for the head of `source.branch` ([ADR 0001](0001-go-and-core-design.md)), and fails a deploy whose image does not appear within the resolve deadline (15 minutes).
- The release workflow builds images only for version tags. Each release pushes `:vX.Y.Z` and `:<40-character commit SHA>` ([releasing.md](../releasing.md#image-tags)); no image exists for an untagged commit on `main`.
- A broken doorstep cannot roll itself back. The upgrade path has to keep working when the new version is unhealthy, and there has to be a way back that does not need doorstep at all.

## Decision

1. No engine or configuration change. The `:<SHA>` tag that every release already pushes is the mapping from commit to image.
2. The self-hosting app follows a dedicated `release` branch of `sharpintelligence/doorstep` (`source.branch: release`). The branch is only ever fast-forwarded to tagged release commits, so every commit doorstep sees at its head has an image.
3. A maintainer advances `release` by hand as a step of the release checklist (`git push origin 'vX.Y.Z^{commit}:refs/heads/release'`), protected by a branch ruleset that restricts updates and blocks force pushes and deletions. The release workflow is unchanged.
4. The self-hosting example (`deploy/kustomize/examples/self-hosted`) requires the checks `check`, `lint-manifests` and `image`, flags migration releases with the `Doorstep-Migration: true` commit trailer (labels cannot work, because pull requests merge into `main`, not into `release`), waits up to 15 minutes for the sync, and runs `health` and a 10 minute `soak` after the deploy, with `auto_rollback: true`.
5. Handover relies on what exists: the base's `RollingUpdate` with `maxUnavailable: 0` and `maxSurge: 1` keeps the old pod until the new one is ready; the old pod keeps its claim, releases it on shutdown, and the new replica resumes the deployment from its persisted state. A hard kill is covered by lease expiry. Tests pin both the manifest settings and the resumption.
6. The manual escape hatch is `git revert` of the pin commit in the GitOps repository, which Argo CD applies without doorstep. Afterwards the operator cancels the self-deployment and blocks the bad version, so history and rollback targeting stay honest ([self-hosting.md](../self-hosting.md#escape-hatch)).
7. Dogfooding before a final tag is a documented checklist: tag `vX.Y.Z-rc.N`, fast-forward `release` to it, confirm the self-deployment succeeded, then tag `vX.Y.Z` on the same commit. It applies once a self-hosted install exists; no workflow job or GitHub environment is added until then.
8. Self-hosting through doorstep is for the project's own install and for forks or mirrors that build their own images: intake, required checks and status mirroring need the operator's GitHub App installed on the source repository. Other installs keep upgrading by bumping `?ref=` and the digest.

## Options considered

- **Follow `main`.** Rejected: every merge would be enqueued and fail after the resolve deadline, because no image exists for untagged commits.
- **Publish an image for every `main` commit.** Rejected for now: it needs `packages: write` in CI, produces unsigned images or a second signing path, and needs a promote or retag flow. It is the route to continuous self-deploys later.
- **A tag template in `image` or tag-based intake.** Rejected: new configuration and new intake code for one app, in the release that freezes the configuration format.
- **Manual deploys only (`doorstep deploy doorstep <sha>`).** Rejected as the sole path: a manual deploy still requires the commit to be reachable from the followed branch, so a release branch is needed anyway; with it, manual deploys keep working as well.
- **A `release` branch (chosen).** It reuses intake, required checks (check runs are per SHA and CI already ran on `main`), batching, migration detection over the commit range and manual deploys, with no code change.
- **Let the release workflow advance `release`.** Deferred: a person deciding when doorstep upgrades itself is the safer default, the workflow keeps its least-privilege job layout, and a manual push cannot break CI. A small `promote` job can automate it once a self-hosted install exists.
- **A dogfood job in the release workflow.** Deferred: there is no environment to deploy to yet, and a job that cannot run would be dead configuration. A prerelease tag is the only way to get a signed image of an untagged commit with the existing pipeline.

## Consequences

- doorstep's own releases get the same queue, gates, history, notifications and rollback as any other app, and the soak gate proves the new version's engine works, because once the old pod is gone only the new engine can finish the soak.
- Upgrading is a deliberate act (fast-forwarding `release`), not a side effect of merging.
- The final tag of a dogfooded release rebuilds the same commit, so `:<sha>` then points at the final build, which differs from the candidate only in its version string. Digest pins stay valid; `doorstep deploy doorstep <sha>` moves the install to the final digest if wanted.
- doorstep pins only the image digest. Changes to the base manifests still need the overlay's `?ref=` bumped by hand or by a dependency bot.
- A new version whose pod becomes ready but whose engine is broken cannot be rolled back by doorstep; that is what the escape hatch is for.

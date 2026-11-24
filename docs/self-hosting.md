# Self-hosting

doorstep can manage its own deployment like any other app: a new release goes through the queue, the gates and the history, and is pinned in the GitOps repository that Argo CD applies. This page explains the setup, what happens during a self-deploy, the safeguards, and the manual way back when doorstep cannot help itself. The decisions behind it are [ADR 0022](decisions/0022-self-hosting-through-a-release-branch.md) and [ADR 0023](decisions/0023-one-release-schema-compatibility.md).

## Who can use it

Self-deploys need the full intake path for the source repository: webhooks, required check runs and commit statuses all go through the GitHub App installation of the repository doorstep follows ([github-app.md](github-app.md)). That works for:

- the project's own install, whose GitHub App is installed on `sharpintelligence/doorstep`;
- forks and mirrors that build and publish their own images with the same tag scheme, followed with their own App.

An install that cannot install its GitHub App on `sharpintelligence/doorstep` cannot follow it. Keep upgrading such an install by bumping the overlay's `?ref=` and image digest together, by hand or with a dependency bot ([install.md](install.md#upgrades)).

## The release branch and image tags

doorstep deploys `image:<full commit SHA>` for the head of the app's `source.branch`. Release images are built only for version tags, and each release pushes two tags, `:vX.Y.Z` and `:<40-character commit SHA>` ([releasing.md](releasing.md#image-tags)). The SHA tag is the mapping from commit to image, so no configuration or code is special to self-hosting.

What makes it work is the branch: the self-hosted app follows `release`, not `main`. `release` is only ever fast-forwarded to tagged release commits by a maintainer ([releasing.md](releasing.md#the-release-branch)), so every commit doorstep sees there has an image. Following `main` would enqueue every merge, and each deploy would fail after the 15 minute resolve deadline because untagged commits have no image.

Check runs belong to a commit, not a branch, so the checks CI ran on `main` satisfy `required_checks` when the same commit reaches `release`. Batching works as for any app: if `release` jumps across several releases while a deploy is queued, the newest one is deployed.

## Configuration

`deploy/kustomize/examples/self-hosted` is a complete overlay. Its `config.yaml` adds doorstep as an app of itself:

```yaml
apps:
  - name: doorstep
    source:
      repo: sharpintelligence/doorstep
      branch: release
      required_checks: [check, lint-manifests, image]
    image:
      repository: ghcr.io/sharpintelligence/doorstep
    gitops:
      repo: example-user/homelab
      path: apps/doorstep
    argocd:
      sync_timeout: 15m
    migrations:
      trailer: true
    gates:
      after:
        - type: health
        - type: soak
          duration: 10m
    auto_rollback: true
    notify: [ops]
```

- `required_checks` names the CI jobs of `.github/workflows/ci.yml` that build and test the commit.
- `gitops.path` is the directory in your GitOps repository that holds this overlay. The default kustomize pin edits its `images` entry for `ghcr.io/sharpintelligence/doorstep`, the same entry [install.md](install.md#pinning-the-image) tells you to set.
- `sync_timeout: 15m` covers the startup probe's 300 seconds for connecting and migrating, plus image pulls and Argo CD's reconcile interval.
- `migrations.trailer` flags releases that contain database migrations. Pull request labels cannot work here: a label counts only on a pull request whose base is `source.branch`, and doorstep's pull requests merge into `main`, not `release`. Pull requests that add a migration therefore carry `Doorstep-Migration: true` in their squash-merge commit message ([CONTRIBUTING.md](../CONTRIBUTING.md#database-migrations)). Detection looks at every commit since the previously enqueued deploy, so a jump across several releases is flagged when any of them migrated.
- The `soak` gate matters more here than for other apps; see [What happens during a self-deploy](#what-happens-during-a-self-deploy).
- The image is public, so the overlay needs no registry credentials.

doorstep pins only the image digest. A release that changes the base manifests also needs the overlay's `?ref=` bumped; the release notes say when.

The Argo CD Application that applies the overlay lives in the `argocd` namespace, so it is not part of the overlay (the overlay's `namespace: doorstep` would move it). Automated sync with self-heal is what makes the escape hatch work without doorstep:

```yaml kubernetes
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: doorstep
  namespace: argocd
spec:
  project: default
  source:
    repoURL: https://github.com/example-user/homelab.git
    targetRevision: main
    path: apps/doorstep
  destination:
    server: https://kubernetes.default.svc
    namespace: doorstep
  syncPolicy:
    automated:
      prune: true
      selfHeal: true
```

The app's `argocd.application` defaults to its name, `doorstep`, and `argocd.namespace` to `argocd`, which match this Application.

## What happens during a self-deploy

1. A maintainer fast-forwards `release` to a release commit. Intake sees the push, waits for the required checks and queues a deploy of `ghcr.io/sharpintelligence/doorstep:<sha>`.
2. The running doorstep (the old version) claims it, resolves the tag to a digest, pins the digest in `apps/doorstep/kustomization.yaml` and waits for Argo CD in `syncing`.
3. Argo CD applies the new pod template. The base's `RollingUpdate` with `maxUnavailable: 0` and `maxSurge: 1` starts a new pod next to the old one. The new pod applies pending migrations under the migration advisory lock ([database.md](database.md#migrations)) and only then opens its listener, so it becomes ready (`/readyz`) on the migrated schema. The old pod keeps running and keeps its claim on the deployment.
4. Once the new pod is ready, Kubernetes stops the old one. Its shutdown releases the claim at once, and the new pod's engine claims the deployment and resumes the sync wait from the state stored in Postgres; nothing is resolved or pinned twice ([engine.md](engine.md#resuming-a-sync-wait)). If the old pod is killed without a clean shutdown, its lease expires after 60 seconds and the new pod claims the deployment then.
5. The new engine sees the Application synced and healthy, moves the deployment to `verifying` and runs the `health` and `soak` gates.

Once the old pod is gone only the new version's engine can finish the soak, so a self-deployment that reaches `succeeded` proves the new engine works, not just that its pod passed a readiness probe. The engine integration tests cover both handovers, a clean shutdown and an expired lease, against a real Postgres.

## Safeguards

- **Rolling update, never `Recreate`.** The old version keeps running until the new one is ready. A new version that never becomes ready (it crashes, cannot reach Postgres, fails a migration) leaves the old pod serving, and the old engine fails the deployment after its retries. Tests in `deploy/kustomize` fail if the base or any example changes the strategy or the readiness probe.
- **One-release schema compatibility.** During a rollout the old pod runs against the schema the new pod migrated, and after a revert the previous version starts against it. That works because migrations are backward compatible for one release and the previous binary starts in compatibility mode instead of refusing a newer schema ([database.md](database.md#compatibility-across-one-release)).
- **Automatic rollback, within limits.** With `auto_rollback: true`, a self-deploy that fails its sync or its after gates is rolled back by whichever engine is running, which in the common case (new pod never ready) is the old one. Automatic rollbacks are withheld across releases flagged as containing migrations ([rollback.md](rollback.md#automatic-rollbacks-and-migrations)); roll back by hand with `--confirm-migration-rollback` after checking that the previous release can run on the migrated schema. Automatic rollback cannot help when the new version becomes ready but its engine is broken, because then nothing is left to act on the failure. That case is what the escape hatch is for.

When doorstep is still working, roll back like any app: `doorstep rollback doorstep`. Use the escape hatch only when doorstep itself is too broken to do that.

## Escape hatch

The way back does not need doorstep. Argo CD applies whatever the GitOps repository says, so reverting the pin commit restores the previous digest.

1. **If the API still answers**, stop doorstep from acting on its own deployment first: cancel the self-deployment and freeze the app, so no newer queued deploy starts while you work.

   ```sh
   doorstep cancel <deployment-id> --reason "broken self-deploy; reverting by hand"
   doorstep lock doorstep --reason "self-deploy reverted by hand"
   ```

2. **Revert the pin commit** in the GitOps repository. The deployment's page in the web UI and `GET /api/v1/deployments/{id}` show it as the pin commit; without doorstep, it is the newest commit whose trailers name the app and the deployment ([gitops.md](gitops.md); [ADR 0005](decisions/0005-pin-commits-and-trailers.md)):

   ```sh
   git log -n 1 --all-match --grep '^Doorstep-App: doorstep$' --grep '^Doorstep-Kind: deploy$'
   git revert <pin-commit>
   git push
   ```

   Argo CD's automated sync with self-heal applies the revert. Without automated sync, run `argocd app sync doorstep`, or apply the overlay directly with `kubectl apply -k apps/doorstep`. If the release migrated the database, the previous binary starts in schema compatibility mode and logs a warning; that is expected.

3. **Clean up once the previous version is up.** The revert commit descends from the pin commit, so if the deployment is still active, the restored engine resumes it, sees the Application synced to a revision that contains the pin commit and healthy, and would record the bad release as `succeeded` once its gates pass. The health gate does not compare revisions, because other apps' pins move them, and the soak's 10 minutes are the window to act. So:

   - cancel the self-deployment if it is still active (`doorstep cancel <deployment-id>`; finished deployments answer `not_cancellable`);
   - block the bad release so intake does not queue its commit again and it is never chosen as a rollback target: `doorstep block doorstep <commit-sha> --reason "..."` and `doorstep block doorstep <digest> --reason "..."`;
   - release the freeze (`doorstep unlock doorstep`) after a fixed release is on `release`.

If the previous binary refuses to start with `database schema is newer than this doorstep binary`, the release raised the compatibility floor (a contract migration) and the previous version cannot run on the migrated schema. Do not lower the floor by hand. Roll forward to a fixed release, or restore the database backup taken before the upgrade and then revert.

## Dogfooding releases

Before a final tag, the project deploys a release candidate to its own install through doorstep; [releasing.md](releasing.md#dogfooding) has the checklist. It applies once a self-hosted install exists.

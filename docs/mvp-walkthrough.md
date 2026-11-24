# MVP walkthrough

This walkthrough proves doorstep's core promise against real systems: a pull request merged to `main` is built by CI, deployed through a GitOps repository and Argo CD, announced in Slack, and rolled back from the command line. CI proves the same flow against fakes in `TestMVPDeploysAMergedPullRequestAndRollsBack` ([ADR 0021](decisions/0021-mvp-acceptance-evidence.md)); run this walkthrough when you change how doorstep talks to GitHub, a registry or Argo CD, or before a release that touches those paths.

The examples use the placeholder account `example-org`, an app called `blog` and doorstep at `https://doorstep.example.com`. Replace them with your own.

## Prerequisites

- A Kubernetes cluster with Argo CD installed in the `argocd` namespace.
- A Postgres database doorstep can reach.
- A GitHub account or organization where you can create a GitHub App and two repositories:
  - `example-org/blog`, the application repository, with GitHub Actions enabled;
  - `example-org/homelab`, the GitOps repository Argo CD syncs from.
- A Slack workspace where you can add an incoming webhook.
- The `doorstep` CLI, `kubectl` and `curl` on your machine.

## 1. Create the GitHub App

Follow [github-app.md](github-app.md#create-the-app):

- Webhook URL `https://doorstep.example.com/webhooks/github`, a secret from `openssl rand -hex 32`, SSL verification on.
- The repository permissions listed under [Permissions](github-app.md#permissions): contents read and write on the GitOps repository; contents and checks read, commit statuses and deployments read and write on the application repository.
- Subscribe to the **Push**, **Check run**, **Check suite** and **Status** events.
- Install the App on both repositories and download a private key.

## 2. Build images tagged with the commit SHA

doorstep deploys the image tagged with the full commit SHA of the merge commit, so CI must push exactly that tag. In `example-org/blog`, add `.github/workflows/build.yml`:

```yaml
name: build
on:
  push:
    branches: [main]
  pull_request:
permissions:
  contents: read
  packages: write
jobs:
  test:
    runs-on: ubuntu-24.04
    steps:
      - uses: actions/checkout@v4
      - run: make test
  image:
    needs: test
    if: github.event_name == 'push'
    runs-on: ubuntu-24.04
    steps:
      - uses: actions/checkout@v4
      - uses: docker/login-action@v3
        with:
          registry: ghcr.io
          username: ${{ github.actor }}
          password: ${{ secrets.GITHUB_TOKEN }}
      - uses: docker/build-push-action@v6
        with:
          push: true
          tags: ghcr.io/example-org/blog:${{ github.sha }}
```

The check run of the `test` job is called `test`; that is the name doorstep's `required_checks` refers to. Pin the actions by commit SHA in a real repository. Protect `main` so changes land through pull requests.

The image job runs after the check doorstep waits for, so the check can turn green before the image exists. doorstep handles that order: the deploy waits in `resolving` until the tag appears.

## 3. Prepare the GitOps repository and Argo CD

In `example-org/homelab`, add `apps/blog/kustomization.yaml` with an `images` entry named after the image repository; doorstep adds or replaces its `digest` ([gitops.md](gitops.md#kustomize-strategy)):

```yaml
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - deployment.yaml
images:
  - name: ghcr.io/example-org/blog
    newTag: initial
```

`apps/blog/deployment.yaml` holds a normal Deployment that uses `image: ghcr.io/example-org/blog`. Create an Argo CD Application named `blog` that follows the directory with automated sync:

```yaml
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: blog
  namespace: argocd
spec:
  project: default
  source:
    repoURL: https://github.com/example-org/homelab.git
    targetRevision: main
    path: apps/blog
  destination:
    server: https://kubernetes.default.svc
    namespace: blog
  syncPolicy:
    automated:
      prune: true
      selfHeal: true
    syncOptions:
      - CreateNamespace=true
```

Argo CD polls git every three minutes by default. Add a GitHub webhook from `example-org/homelab` to Argo CD, or lower `timeout.reconciliation`, to keep deploys fast; doorstep's default `argocd.sync_timeout` is ten minutes ([argocd.md](argocd.md)).

## 4. Install doorstep

Follow the [quick start in install.md](install.md#quick-start) with the homelab overlay. Put the Slack incoming webhook URL in the `slack-webhook-url` key of the `doorstep` Secret. Use this `config.yaml`:

```yaml
server:
  public_url: https://doorstep.example.com

github:
  app_id: "123456"
  private_key_file: /etc/doorstep/secrets/github-app-private-key.pem
  webhook_secret_file: /etc/doorstep/secrets/github-webhook-secret

notifications:
  - name: chat
    type: slack
    webhook_url_file: /etc/doorstep/secrets/slack-webhook-url

apps:
  - name: blog
    source:
      repo: example-org/blog
      required_checks: [test]
    image:
      repository: ghcr.io/example-org/blog
    gitops:
      repo: example-org/homelab
      path: apps/blog
    notify: [chat]
```

`server.public_url` (or `DOORSTEP_PUBLIC_URL`) makes notifications and pin commits link to the deployment. Validate the file with `doorstep config validate --config config.yaml` before applying it, and expose `https://doorstep.example.com` as described in [Exposing the HTTP port](install.md#exposing-the-http-port).

Create two API tokens from a shell that can reach the database ([authentication.md](authentication.md)):

```sh
export VIEWER=$(doorstep token create --name walkthrough-viewer --role viewer)
export DOORSTEP_TOKEN=$(doorstep token create --name walkthrough-deployer --role deployer)
export DOORSTEP_SERVER_URL=https://doorstep.example.com
```

Check the integrations before the first merge:

| Command | Expected |
| --- | --- |
| `doorstep notify test chat` | A test message in the Slack channel |
| `doorstep image resolve ghcr.io/example-org/blog <sha of an existing build>` | A `sha256:` digest |
| `doorstep argocd status blog` | The Application, its sync and health status and revision |

## 5. Deploy a merged pull request

Open a pull request in `example-org/blog`, wait for `test` to pass and merge it. Then check each step:

| Step | Expected result |
| --- | --- |
| GitHub delivers the push | The App's **Advanced** page shows a `push` delivery answered `200`. The commit is tracked but not queued while `test` runs on `main`. |
| CI turns green | A `check_run` delivery answered `200`; the deploy is queued. |
| Slack | `blog: deploy #1 started` |
| GitOps commit | `example-org/homelab` has a new commit on `main` that sets `digest:` in `apps/blog/kustomization.yaml`, with trailers `Doorstep-App: blog`, `Doorstep-Deployment: 1`, `Doorstep-Kind: deploy`, `Doorstep-App-Commit`, `Doorstep-Image`, `Doorstep-Digest` and `Doorstep-URL` (later deploys also carry `Doorstep-Previous-Digest`) |
| Argo CD | The `blog` Application is `Synced` and `Healthy` at that commit |
| Slack | `blog: deploy #1 succeeded`, linking to the commit in `example-org/blog` and to `https://doorstep.example.com/deployments/1` |

Read the lineage:

```sh
curl -fsS -H "Authorization: Bearer $VIEWER" https://doorstep.example.com/api/v1/deployments/1
```

The `deployment` object shows `state: succeeded`, `trigger.source: webhook` with the GitHub user who merged in `requested_by`, `commit.sha` of the merge commit, `image.tag` equal to that SHA and the resolved `image.digest`, `gitops.commit_sha` of the pin commit and `argocd.revision` equal to it. `events` lists the transitions `queued`, `resolving`, `pinning`, `syncing`, `verifying` and `succeeded`. The [API reference](api.md) describes every field.

Merge a second pull request and wait for `deploy #2 succeeded`. Its `image.previous_digest` is the first deploy's digest.

## 6. Roll back from the CLI

```sh
doorstep rollback blog --reason "walkthrough"
```

| Step | Expected result |
| --- | --- |
| CLI | `rollback 3 queued for blog: ghcr.io/example-org/blog@sha256:… (reverting deployment 2); blocked sha256:…` |
| GitOps commit | A commit that sets the first digest again, with `Doorstep-Kind: rollback`, `Doorstep-Rollback-Of: 2` and `Doorstep-Block: <digest of deploy 2>` |
| Argo CD | Synced and healthy at the rollback commit; the pods run the first image |
| Slack | `blog: deploy #2 rolled back`, saying that rollback #3 re-pinned the image that ran before deploy #2 |
| `doorstep blocks blog` | The digest of deploy 2, `IN GIT yes`, reason `walkthrough` |

While the block stands, doorstep will not deploy that digest automatically. Clear it once a fix is ready:

```sh
doorstep unblock blog <digest of deploy 2>
```

## Troubleshooting

| Symptom | Where to look |
| --- | --- |
| Deliveries fail with `401` or `404` | The webhook secret ([github-app.md](github-app.md#troubleshooting)) |
| A green commit is never queued | `required_checks` must match the check run name exactly; with no webhooks the poller picks it up within `intake.poll_interval` |
| Deploy waits in `resolving` | `doorstep image resolve ghcr.io/example-org/blog <sha>`; the tag must be the full 40-character SHA and the registry credentials must allow reads ([configuration.md](configuration.md#registry-credentials)) |
| Deploy fails in `pinning` | The `images` entry name must equal `image.repository`; branch protection on the GitOps branch needs `deploy.mode: pull_request` ([gitops.md](gitops.md)) |
| Deploy times out in `syncing` | `doorstep argocd status blog`; Argo CD's repository URL must point at the GitOps repository and refresh after doorstep's commit ([argocd.md](argocd.md)) |
| No Slack message | `doorstep notify test chat`, and the app's `notify` list; notifications are best effort and failures are logged ([notifications.md](notifications.md)) |

# 0001: Go, and doorstep's core design

- Status: Accepted
- Date: 2026-11-23

## Context

doorstep is a deployment engine for homelabs and small teams running Argo CD. Argo CD keeps the cluster matching git; doorstep decides what goes into git and when. It watches app repositories' CI, keeps a deploy queue and a lock per app, writes new image digests into the GitOps repository (as a direct commit or a pull request, per app), waits for Argo CD to report synced and healthy, checks gates, and rolls back by committing the previous version.

Argo CD answers "does the cluster match git". It does not answer which build should be in git, in what order, who holds the lock, whether the deploy worked, and what to do when it did not. Homelab users fill that gap with image updater bots or hand-edited tags, which leave no queue, no record and no rollback.

#### Principles

- Merge is the approval. A change that is merged to an app's main branch and passes its checks is deployed; there is no separate approval step.
- Small footprint. One Go binary plus Postgres, built for amd64 and arm64.
- Multiple GitHub owners. One doorstep instance serves several users and organizations.
- Every check is a recorded gate, mirrored to GitHub commit statuses.
- Rollback is a forward commit plus a recorded block on the bad version.

#### Prior art

- [Shopify Shipit](https://github.com/Shopify/shipit-engine): the deploy queue, per-stack locks, merge-driven deploys and rollback.
- [Akuity Kargo](https://github.com/akuity/kargo): promoting immutable artifacts into GitOps repositories for Argo CD.
- [Argo GitOps Promoter](https://github.com/argoproj-labs/gitops-promoter): pull request based promotion with commit statuses as gates.
- [GitHub's deploy practice](https://github.blog/engineering/deploying-branches-to-github-com/): merge as approval, deploy locks and a queue, and deploying then verifying.
- [Slack's deploy process and ReleaseBot](https://slack.engineering/deploys-at-slack/): automated health and metric gating, with automatic stop and rollback.

## Decision

### 1. doorstep is written in Go

Go is the language of the Argo ecosystem. client-go and go-github are the reference-quality clients for the two APIs doorstep lives on, and Go produces a static single binary that cross-compiles to linux/amd64 and linux/arm64 with no extra toolchain. The standard library covers HTTP, TLS, JSON and structured logging, which keeps the dependency list short.

Trade-off: Go offers less compile-time safety than Rust and needs more boilerplate for error handling and state modelling.

### 2. doorstep sits upstream of Argo CD and never writes to the cluster

This is a hard invariant. doorstep's only Kubernetes access is read-only: get, list and watch on Argo CD Application resources. It never applies manifests, never triggers or patches Argo CD syncs, and never holds cluster write credentials. Its only write targets are:

- the GitOps repository (a commit or a pull request),
- GitHub commit statuses,
- its own Postgres database,
- notification endpoints.

The list is checkable in review and in RBAC manifests. It rules out the tempting shortcut of calling the Argo CD sync API.

Trade-off: doorstep cannot repair the cluster directly, so recovery speed is bounded by Argo CD's sync.

### 3. Git is the source of truth

The deployed version of an app is whatever image digest is pinned in the GitOps repository at HEAD. Images are pinned by digest, not by tag. Postgres holds operational state (the queue, locks, deploy history, gate results and blocks) that explains and controls changes, but the cluster is never reconciled from it. If Postgres is lost, the cluster and the desired state are unaffected; only history and in-flight deploys are lost.

Merge is the approval, so doorstep has no approval UI and no role model of its own for deciding what ships. GitHub branch protection and review are the access control.

Trade-off: two stores must be kept consistent, and git always wins when they disagree.

### 4. Gates are recorded data

Every check that can hold or fail a deploy is a gate. This includes the CI result, a lock, Argo CD synced and healthy, soak time, a metric query and a manual hold. A gate is a named record with a status, timestamps and evidence. It is persisted per deploy and mirrored to a GitHub commit status. No check exists only as control flow or a log line.

Trade-off: this needs more schema and more writes than inline checks.

### 5. Rollback is a forward commit

A rollback is a new commit (or pull request, per the app's configured mode) that re-pins the previously deployed digest. It travels the same queue, lock and gate path as any deploy. doorstep also records a block on the bad digest, which prevents it from being queued again until the block is cleared. A rollback is never a history rewrite, a force-push, or an Argo CD rollback or sync to a revision, because those make the cluster diverge from git and fight auto-sync.

Trade-off: a rollback is as slow as a deploy and depends on the previous digest still being pullable.

## Options considered

### Run Shopify Shipit

Shipit is the model doorstep borrows most from: a deploy queue, locks per stack, merge-driven deploys and rollback. It is a Rails engine that needs Ruby, Redis and a relational database, and it executes deploy scripts itself with deploy credentials. It has no native GitOps pin or Argo CD health model. It is heavier than doorstep's footprint allows and sits on the wrong side of the trust boundary in decision 2.

### Adopt Akuity Kargo

Kargo is a capable, actively developed Argo-native promotion tool built on Warehouses, Freight and Stages. It runs as CRDs and controllers in the cluster with its own API server and UI, so its state lives in the cluster it deploys to. That is more than a homelab needs, and its model is multi-stage promotion rather than a deploy queue with locks per app.

### Adopt Argo GitOps Promoter

GitOps Promoter is closest in philosophy: pull requests, commit statuses as gates and git as truth. It is an in-cluster, CRD-driven controller built around hydrated environment branches and promotion between environments. It does not watch app-repository CI, queue deploys per app or roll back, which are the parts doorstep exists to provide.

### Build in Rust

Rust offers memory safety, small binaries and a stronger type system. The Kubernetes and GitHub clients for it (kube-rs and octocrab) trail client-go and go-github in coverage and maturity, cross-compilation is harder, and contributor familiarity in the Argo community is lower. doorstep's workload is I/O-bound orchestration, which gains little from what Rust adds.

### Build in Go (chosen)

See decision 1.

### Comparison

| | Runtime footprint | Where state lives | Writes to cluster | Approval model | Rollback |
| --- | --- | --- | --- | --- | --- |
| Shipit | Rails app, Redis, relational database | Its own database | Yes, through deploy scripts | Merge-driven, with optional checks | Re-deploy an earlier revision |
| Kargo | Controllers and API server in the cluster | CRDs in the cluster | Yes, in-cluster controllers | Stage promotion rules | Promote earlier Freight |
| GitOps Promoter | Controller in the cluster | CRDs in the cluster, plus git | Controller runs in the cluster | Pull requests and commit statuses | Not a focus |
| doorstep | One binary plus Postgres, outside or inside the cluster | Git for desired state, Postgres for history | Never; read-only Argo CD access | Merge is the approval | Forward commit plus a block |

## Consequences

### Positive

- A small attack surface: doorstep holds no cluster write credentials.
- It works with any Argo CD setup without installing CRDs.
- The audit trail is complete: git holds what shipped, and gate records hold why.
- One binary plus Postgres runs on a Raspberry Pi class node or outside the cluster.
- Deploy and rollback share one code path.

### Negative

- doorstep cannot fix the cluster directly, so recovery speed is bounded by Argo CD's sync.
- A forward-commit rollback is only as good as the previous digest still being pullable.
- Postgres is a required dependency and a second state store beside git.
- Read-only Kubernetes access still needs a ServiceAccount and RBAC.
- doorstep is GitHub-only and Argo CD-only by design.
- Go's weaker type system compared with Rust is accepted.

### Accepted risks

- The GitHub App credential with write access to the GitOps repository is the high-value secret. It must be scoped per installation and owner.
- A rolled-back digest may have been garbage-collected from the registry.
- doorstep's view of health is only as good as Argo CD's health assessment.

Package layout, library choices and interfaces are in the architecture document (`docs/architecture.md`).

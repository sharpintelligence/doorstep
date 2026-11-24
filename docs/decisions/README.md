# Decisions

This directory holds architecture decision records (ADRs). An ADR records one decision that is expensive to reverse, why it was made and what was given up. ADRs explain why; the [architecture document](../architecture.md) describes what exists now.

## Index

| ADR | Title | Status | Date |
| --- | --- | --- | --- |
| [0001](0001-go-and-core-design.md) | Go, and doorstep's core design | Accepted | 2026-11-23 |
| [0002](0002-rollback-is-a-new-deployment.md) | A rollback is a new deployment | Accepted | 2026-11-23 |
| [0003](0003-queue-batching-and-urgent.md) | Queue batching and urgent deploys | Accepted | 2026-11-23 |
| [0004](0004-freeze-locks.md) | Freeze locks stop new deploys, not recovery | Proposed | 2026-11-23 |
| [0005](0005-pin-commits-and-trailers.md) | Pin commits, byte-preserving edits and trailers | Accepted | 2026-11-23 |
| [0006](0006-notifications-are-best-effort.md) | Notifications are best effort | Accepted | 2026-11-23 |
| [0007](0007-authentication-and-roles.md) | Authentication and roles | Accepted | 2026-11-24 |
| [0008](0008-kubernetes-rest-client.md) | A small Kubernetes REST client, one get and watch per wait | Accepted | 2026-11-24 |
| [0009](0009-blocks-in-postgres-mirrored-to-git.md) | Blocks live in Postgres and are mirrored to git | Accepted | 2026-11-24 |
| [0010](0010-webhooks-and-polling-safety-net.md) | Webhooks plus a polling safety net | Accepted | 2026-11-24 |
| [0011](0011-built-in-gates-and-deploy-schedules.md) | Built-in gates and deploy schedules | Proposed | 2026-11-24 |
| [0012](0012-deployment-engine.md) | The deployment engine | Proposed | 2026-11-24 |

## When to write one

Write an ADR for a decision that is expensive to reverse or that constrains other packages:

- Language or runtime.
- Datastore.
- Trust boundaries, meaning what doorstep is allowed to write to.
- The shape of the public API or the configuration format.
- The security model.
- Replacing a core dependency.

Do not write one for routine library picks, internal refactors or anything cheap to undo. Those belong in the architecture document or the pull request description.

## Process

1. Copy the template below to `NNNN-kebab-title.md`, using the next four-digit number. Numbers are never reused.
2. Open it as Proposed in the pull request that implements or precedes the change.
3. Set the status to Accepted when the pull request merges, and add a row to the index above.
4. Do not edit an Accepted ADR except to fix typos and to update its status line. To change a decision, write a new ADR, set the old one to `Superseded by NNNN`, and link the two to each other.

The founding ADR, 0001, is an exception: it was written as Accepted with its index row in place, because the project had no earlier decisions to review it against.

## Statuses

- Proposed: under discussion in an open pull request.
- Accepted: in force.
- Superseded by NNNN: replaced by a later ADR, which is linked from the status line.
- Deprecated: no longer applies and has no replacement.

## Template

```markdown
# NNNN: Title

- Status: Proposed
- Date: YYYY-MM-DD

## Context

What problem or force prompts this decision, and what constraints apply.

## Decision

What we will do, stated in the present tense.

## Options considered

Each option weighed, including the one chosen, with the trade-off that decided it.

## Consequences

What becomes easier, what becomes harder, and what risks are accepted.
```

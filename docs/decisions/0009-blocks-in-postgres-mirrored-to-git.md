# 0009: Blocks live in Postgres and are mirrored to git

- Status: Proposed
- Date: 2026-11-24

## Context

A rollback must stop the version it replaces from being deployed again automatically ([ADR 0001](0001-go-and-core-design.md), decision 5). ADR 0001 has the rollback's pin commit carry a `Doorstep-Block` trailer and an unblock carry `Doorstep-Unblock`, and [ADR 0005](0005-pin-commits-and-trailers.md) fixed that vocabulary. ADR 0005's last consequence left two gaps open:

- A rollback whose target digest is already pinned writes no commit, so its `Doorstep-Block` has nowhere to go.
- An unblock is not a pin, so `Doorstep-Unblock` needs a commit of its own; ADR 0005 reserved the Git Data API for an empty unblock commit.

The bead implementing one-click rollback (doorstep-ehl.6.1) also has to decide what a block is keyed on. ADR 0001 blocks digests, but intake only knows the commit SHA when it enqueues a deploy (the image tag is the commit SHA and the digest is resolved later), so a digest-only block cannot stop an automatic deploy at intake. And whatever records a block has to be consistent with the rollback request that creates it: a request must not queue the rollback and then lose the block.

## Decision

- **Postgres is the operational source of truth.** A `blocks` row records a block per app with a digest, a commit SHA (with its source repository), or both. A rollback blocks the replaced deployment's digest and, unless the rollback target was built from the same commit, its commit. Lookups match either key. The row is written in the same transaction as the rollback request (cancel, enqueue, block and audit), or as a manual block or unblock with its audit row, and takes effect immediately.
- **Intake refuses blocked commits** where it enqueues, inside the settle transaction, under the app row lock that rollback requests also take. **The engine refuses blocked digests** right after resolving a deploy. The queue and store do not refuse enqueues, and rollbacks are never refused.
- **Git is an eventually consistent mirror.** A digest block not yet written to git, and a cleared digest block whose block reached git but whose unblock did not, are the app's *pending trailers*. The engine adds them to the app's next pin commit as `Doorstep-Block` and `Doorstep-Unblock` and then records that commit's SHA on the rows. The rollback's own pin commit therefore carries its block whenever it writes one, as ADR 0001 describes. `gitops.Request` gains `Unblocks` beside `Blocks`.
- **No empty commits and no Git Data API.** A block or unblock that has no pin commit to ride on waits for the next one.

This amends the last consequence of ADR 0005: blocks whose rollback writes no commit, and unblocks, are recorded in Postgres and carried by the app's next pin commit, so the Git Data API is not needed for them.

## Options considered

### Postgres first, trailers on the next pin commit (chosen)

No new GitHub surface or write path: trailers reuse the pin commit that is written anyway, so it works the same in commit mode, in pull request mode and under branch protection. Postgres answers "is this blocked?" with one indexed query inside the transactions that need it, so a rollback request and intake cannot disagree. The cost is lag: a block or unblock reaches git only with the app's next pin. For unblocks the lag is fail-safe (git keeps saying "blocked"). A block whose rollback wrote no commit is in Postgres only until the next pin, and forever if the app is never pinned again, so restoring doorstep purely from git could miss it.

### An empty commit per block or unblock

Writes the trailer at once, so git is always complete. But an empty commit needs the Git Data API (blob, tree, commit and a ref update), a second write path with its own retry and conflict rules. A direct ref update is refused under branch protection, and in pull request mode it would need an empty pull request and merge for every unblock. It also puts doorstep's bookkeeping on the GitOps branch as commits that change nothing.

### Git as the source of truth

Reading blocks from the GitOps log on every intake decision and every resolve costs GitHub API calls on the hot path, cannot be made atomic with a rollback request in Postgres, and still needs somewhere to keep a block until a commit carries it.

### Blocking digests only

Matches ADR 0001's wording, but intake would enqueue a blocked commit anyway and the deploy would fail only after resolving, producing a failed deployment and a notification for every push of a known-bad commit. Keying on the commit as well lets intake stop it quietly, and the digest key still catches the same image rebuilt under another commit.

## Consequences

- The `blocks` table (migration 00008) is the record of what is blocked; `LastSuccessfulDeployment` skips blocked digests, so the engine's automatic rollback and operator rollbacks never pick a blocked target.
- The engine must call `BlockList.Blocked` after resolving a deploy, `PendingTrailers` before every pin and `MarkMirrored` after a pin that wrote a commit ([architecture.md](../architecture.md#engine-ports)).
- Commit-only blocks are Postgres only: the trailer vocabulary has no commit form.
- A command that flushes pending trailers with an explicit commit, and one that rebuilds the `blocks` table from the GitOps history, are possible follow-ups if git-only recovery becomes a requirement.

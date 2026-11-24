# 0005: Pin commits, byte-preserving edits and trailers

- Status: Accepted
- Date: 2026-11-23

## Context

[ADR 0001](0001-go-and-core-design.md) makes git the record of what is deployed: doorstep writes image digests into the GitOps repository, as a direct commit or as a pull request it merges itself, and reads the previous digest and the active blocks back from that repository's log rather than from Postgres. The pin writer (doorstep-ehl.4.1) has to fix four things that are expensive to change later, because they end up in other people's repositories and git histories:

- how the file is written through the GitHub API;
- how a YAML file someone maintains by hand is edited;
- the commit message format that doorstep later parses back;
- how a pull request is merged without a human step and without turning the App's write access into a way around review.

## Decision

### The Contents API writes the pin

A pin is one file. doorstep reads it with `GET /repos/{owner}/{repo}/contents/{path}?ref={branch}` and writes it with one `PUT` carrying the blob `sha` and the `branch`. That is one commit per pin with a compare-and-set on the blob: commits to other files never conflict, and a concurrent edit of the same file returns `409`, after which doorstep re-reads, re-edits and retries a bounded number of times. No author is given, so GitHub attributes and signs the commit as the App. The Git Data API (blob, tree, commit, ref) is kept for the one case that needs it, an empty unblock commit.

### Edits splice bytes; the parser only locates

The yaml v3 `Node` API finds the target scalar and its position. doorstep replaces exactly that scalar's characters in the original bytes (or inserts one `digest:` line in a kustomize `images` entry), keeping its quote style, then parses the result again and requires the desired value at the same place and an unchanged node count. The document is never re-encoded. Shapes that cannot be spliced with certainty (block and multi-line scalars, anchors, aliases, merge keys, explicit tags, null values, a flow mapping that needs an insert) are refused with `ErrUnsupportedYAML`. The pin location must already exist: doorstep does not append an `images` entry or create a YAML path.

### The trailer vocabulary is fixed

The subject is `deploy(<app>): pin <image>@sha256:<12 hex>` or `rollback(<app>): ...`. The last paragraph holds trailers in a fixed order: `Doorstep-App`, `Doorstep-Deployment`, `Doorstep-Kind`, `Doorstep-App-Commit` (`owner/repo@<sha>`), `Doorstep-Image`, `Doorstep-Digest`, `Doorstep-Previous-Digest`, `Doorstep-Rollback-Of`, `Doorstep-Block` (repeated), `Doorstep-Unblock` (repeated) and `Doorstep-URL`. Values with control characters are rejected. `gitops.FormatMessage` and `gitops.ParseMessage` are the only code that writes and reads them.

### Pull requests are squash-merged by doorstep, and only when they hold nothing but its edit

The head branch is `doorstep/<app>-<deployment id>`, so the flow is rediscovered from GitHub after a restart without storing a pull request number. doorstep tries the merge with `merge_method=squash`, its own title and message, and the head `sha` it validated, and treats `405` and `409` as "not yet": GitHub enforces branch protection, and doorstep does not reimplement it. Before each attempt the pull request must come from the GitOps repository itself and change exactly one file, and that file must equal doorstep's edit of the current base; otherwise doorstep rewrites the file (when only its content differs) or refuses with `ErrPullRequestTampered`.

## Options considered

### Contents API (chosen) or Git Data API

The Git Data API takes five calls per pin (read ref, create blob, create tree, create commit, update ref) and adds a compare-and-set on the branch head. A pin does not need that: it touches one file, and a branch-head CAS would make every unrelated commit on a busy GitOps branch a conflict. The Contents API does the same work in one call with the finer-grained blob CAS.

### Byte splicing (chosen) or re-encoding the node tree

yaml v3 keeps comments when it re-encodes, but drops blank lines, re-indents sequences, normalises quoting and rewraps long lines. In someone's GitOps repository that is a noisy diff on every deploy and can fight their formatter. Splicing leaves every other byte identical, including CRLF line endings and trailing comments, and the re-parse catches any splice that would not mean what was intended. The cost is a smaller set of supported YAML shapes, which fail loudly.

### Appending a missing kustomize entry or YAML path

Creating the entry would make the first deploy of a new app work without preparing the repository, but an `images` entry whose name matches no container is silently ignored by kustomize, so a typo in the image name would look like a successful deploy that changes nothing. Requiring the location to exist surfaces configuration mistakes as `ErrPinNotFound`.

### Squash (chosen), merge commit or rebase

The commit that lands on the GitOps branch must carry doorstep's trailers, because previous digests and blocks are rebuilt from that branch's log. Squash produces exactly one commit with the message doorstep supplies, and its SHA is what Argo CD syncs. A merge commit would also take a message but adds a second parent and the branch commit to the history; rebase would keep the branch commit's message but not let doorstep set it at merge time. Repositories must allow squash merging; a configurable method can follow if needed.

### Merging any open doorstep pull request

Trusting the branch name would let anyone with push access add commits to a doorstep branch and have the App merge them, which on repositories where the App bypasses review is a way around review. The checks use data doorstep already fetches and pin the merge to the head commit that was checked.

## Consequences

- The trailer names and the `doorstep/<app>-<id>` branch name are now part of users' repositories; renaming them needs a migration path that still parses old history.
- Pin files must be regular files of at most 1 MiB with the pinned value written as a single-line string. Unusual YAML around the pin is an error, not a best effort.
- Pull request mode needs *Allow squash merging*, and needs the App on a bypass list where approving reviews are required.
- Nothing about a pin is stored in Postgres beyond the resulting commit SHA and previous digest; a crashed pin resumes from what GitHub shows.
- A rollback whose target digest is already pinned writes no commit, so its `Doorstep-Block` trailer has nowhere to go; the rollback and blocks work must record such a block another way, and needs the Git Data API for an empty `Doorstep-Unblock` commit.

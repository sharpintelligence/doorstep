# GitOps pins

doorstep deploys an image by writing its digest into the GitOps repository that Argo CD syncs from. This page describes exactly what doorstep changes in that repository, what the commits look like, how the two deploy modes behave and what each error means. The app settings that select the file and the mode are in [configuration.md](configuration.md#pin-strategies); the reasoning is in [ADR 0005](decisions/0005-pin-commits-and-trailers.md).

doorstep never clones the repository. It reads and writes one file through the GitHub Contents API with the GitHub App's installation token, so commits are attributed to and signed by the App.

## What gets edited

doorstep changes one value in one file and leaves every other byte alone: comments, blank lines, key order, indentation, quote style and Windows line endings stay exactly as they were. It finds the value with a YAML parser, replaces only its characters in the original text, then parses the result again and checks that the value is the one it wanted and that nothing else in the document structure changed. It never re-serialises your YAML.

### `kustomize` strategy

doorstep reads `kustomization.yaml`, `kustomization.yml` or `Kustomization` (the first that exists) in `gitops.path`, finds the single `images` entry whose `name` is `pin.kustomize.image`, and sets its `digest`.

When the entry already has a `digest`, only that value changes:

```diff
 images:
   - name: ghcr.io/example-user/blog   # the blog
-    digest: sha256:1111111111111111111111111111111111111111111111111111111111111111
+    digest: sha256:2222222222222222222222222222222222222222222222222222222222222222
```

When it has none, doorstep inserts a `digest` line directly below `name`, at the same indentation:

```diff
 images:
   - name: ghcr.io/example-user/blog
+    digest: sha256:2222222222222222222222222222222222222222222222222222222222222222
     newTag: v1
```

`newName` and `newTag` are left alone. When both a tag and a digest are set, kustomize renders `name:tag@digest` and the digest decides what is pulled, so a leftover `newTag` is harmless; you can delete it.

The entry must already exist. doorstep does not append one, because an entry whose name matches no image is silently ignored by kustomize and would hide a typo. The `images` list may contain other entries, including ones that other apps pin.

### `yaml` strategy

doorstep reads `pin.yaml.file` in `gitops.path` and follows `pin.yaml.path`: on a mapping a segment is a key, on a sequence a segment made of digits is an index. The value at the end must already exist and be a string. A placeholder such as `digest: ""` is fine.

A Helm values file with `format: digest` and `path: image.digest`:

```diff
 image:
   repository: ghcr.io/example-user/blog
-  digest: ""  # set by doorstep
+  digest: "sha256:2222222222222222222222222222222222222222222222222222222222222222"  # set by doorstep
```

A plain manifest with `format: reference` and `path: spec.template.spec.containers.0.image`:

```diff
       containers:
         - name: blog
-          image: ghcr.io/example-user/blog:1.0
+          image: ghcr.io/example-user/blog@sha256:2222222222222222222222222222222222222222222222222222222222222222
```

Files with several YAML documents (`---`) are supported as long as exactly one document has a value at the path.

### What cannot be edited

doorstep refuses rather than guesses. These fail with `ErrUnsupportedYAML` and leave the file untouched:

- a value written as a block scalar (`|` or `>`), or a plain or quoted value that spans several lines;
- a value with an anchor (`&name`) or an explicit tag (`!!str`), an alias (`*name`) anywhere on the path, or a merge key (`<<`) in the mapping that holds it;
- a null value (`digest:` with nothing after it, `~`, `null`) or a value that is not a string (a number, a list, a mapping); use `""` as a placeholder;
- a kustomize `images` entry written as a flow mapping (`{name: ..., newTag: ...}`) that has no `digest` key yet; add `digest: ""` to it or write it as a block mapping;
- a file that is not valid YAML.

A missing file, entry or path is `ErrPinNotFound`. Two entries with the same name, a key that appears twice, or a path present in several documents is `ErrAmbiguousPin`. Only regular files of at most 1 MiB are read: directories, symlinks and submodules are `github.ErrNotAFile`, larger files `github.ErrFileTooLarge`.

## Commit messages

Every pin commit has the same shape:

```text
deploy(blog): pin ghcr.io/example-user/blog@sha256:222222222222

Deploy ghcr.io/example-user/blog@sha256:2222...2222 to blog, built from example-user/blog@0123456789abcdef0123456789abcdef01234567.

Doorstep-App: blog
Doorstep-Deployment: 42
Doorstep-Kind: deploy
Doorstep-App-Commit: example-user/blog@0123456789abcdef0123456789abcdef01234567
Doorstep-Image: ghcr.io/example-user/blog
Doorstep-Digest: sha256:2222222222222222222222222222222222222222222222222222222222222222
Doorstep-Previous-Digest: sha256:1111111111111111111111111111111111111111111111111111111111111111
```

A rollback uses `rollback(<app>)` in the subject and explains which deployment it reverts. The trailers form the last paragraph, in this order:

| Trailer | When | Value |
| --- | --- | --- |
| `Doorstep-App` | always | app name |
| `Doorstep-Deployment` | always | doorstep deployment id |
| `Doorstep-Kind` | always | `deploy` or `rollback` |
| `Doorstep-App-Commit` | always | `owner/repo@<40 hex>` of the commit the image was built from; GitHub links it |
| `Doorstep-Image` | always | image repository |
| `Doorstep-Digest` | always | the digest now pinned |
| `Doorstep-Previous-Digest` | when the file held a digest before | the digest this commit replaced |
| `Doorstep-Rollback-Of` | rollbacks | id of the deployment being reverted |
| `Doorstep-Block` | rollbacks that block digests, one per digest | a digest that must not be deployed again until unblocked |
| `Doorstep-Unblock` | unblock commits, one per digest | a digest whose block is cleared |
| `Doorstep-URL` | when doorstep knows its public URL | link to the deployment in doorstep |

These trailers are a durable contract: doorstep reads previous digests and blocks back from the GitOps branch's history, so the history survives losing doorstep's database. Values containing a newline or other control character are rejected, so a trailer cannot be forged through an app name or URL.

## Deploy modes

### `commit`

doorstep pushes one commit straight to `gitops.branch`. The write is a compare-and-set on the file's blob: commits to other files never conflict, and only a concurrent change to the same file does. doorstep then re-reads the file, re-applies its edit and tries again, up to five times with a short randomised backoff (doubling from 200 ms, capped at 10 seconds), so two apps that share a kustomization can deploy at the same moment. Only GitHub's stale-sha answers count as a conflict (`409` or `422` saying the file "does not match" or "is at ... but expected ..."). If the write keeps conflicting the deployment fails with `ErrConflict`. If branch protection or a ruleset rejects it (`GH006`, `GH013`, "Changes must be made through a pull request", "Repository rule violations found") it fails at once with `ErrBranchProtected`, because retrying cannot help. Any other refusal is returned as GitHub's error without a retry.

If the branch is protected, the App must be allowed to push to it (for example through a bypass list), or use `pull_request` mode.

### `pull_request`

doorstep works on a branch named `doorstep/<app>-<deployment id>`:

1. It creates the branch from `gitops.branch` and commits the edited file to it. When the branch already exists but no pull request is open from it (a crash, or a branch doorstep failed to delete), doorstep force-resets it to the current `gitops.branch` first, so old history cannot make the new pull request conflict.
2. It opens a pull request against `gitops.branch`, titled with the commit subject and with the trailers in its description, or finds the one it opened before. If GitHub answers that a pull request already exists (another run for the same deployment opened it a moment earlier), doorstep adopts that one.
3. It waits for the pull request to become mergeable, checking every 10 seconds, and merges it itself with **squash**, using the commit message above. There is no human step: merging the app change was the approval ([ADR 0001](decisions/0001-go-and-core-design.md)). Required status checks on the GitOps repository are honoured, because GitHub refuses the merge until they pass.
4. When GitHub reports the branch as behind and the base requires up-to-date branches, doorstep updates the branch. An update that GitHub refuses because the branch moved or is already up to date is ignored and the next poll looks again; one refused for a merge conflict is treated like a conflicting pull request; any other refusal fails the deployment. When the pull request conflicts with the base, doorstep closes it (noting why in its description), deletes the branch and starts again from the current base, at most three times.
5. After the merge it deletes the branch. The squash commit on `gitops.branch` is the commit Argo CD syncs.

If the pull request is not merged within 30 minutes the deployment step stops with `ErrMergeTimeout` and the pull request stays open; the next attempt picks it up again. Closing the pull request without merging it fails the deployment with `ErrPullRequestClosed`. If someone else merges the pull request, doorstep accepts it only when `gitops.branch` then holds exactly the desired value; when it holds anything else (someone pushed other content to the branch first, or a newer deployment pinned over it) the deployment fails with `ErrPinOverwritten` rather than reporting a commit that does not pin its digest.

doorstep recognises the pull requests it closed itself by a fixed sentence it appends to their description, and opens a fresh pull request instead of failing with `ErrPullRequestClosed`. Anyone who can edit pull request descriptions in the GitOps repository can add that sentence to a pull request they closed, which makes doorstep start over rather than stop. This is deliberate: people with write access to the GitOps repository can already change what Argo CD deploys, so doorstep trusts them; it never trusts content from forks (see below).

doorstep only merges what it wrote. Before each merge it checks that the pull request's branch lives in the GitOps repository itself, that it changes exactly the pin file, and that this file is byte-for-byte doorstep's edit of the current base; the merge is pinned to the commit it checked. A pull request from a fork or one that touches other files is refused with `ErrPullRequestTampered` and left open for a person to look at. If only the pin file differs (for example because someone pushed to the branch), doorstep commits its own edit again before merging.

#### Repository settings

- **Allow squash merging** must be enabled in the GitOps repository.
- Required status checks work as expected: doorstep waits for them.
- Required approving reviews cannot be satisfied by doorstep, so pull requests stall until the timeout unless the App is on the branch protection or ruleset bypass list. Doorstep's self-merge is safe to allow because of the checks above.
- *Automatically delete head branches* is fine; doorstep tolerates branches that are already gone.

## Restarts

Every step is find-or-create and doorstep keeps no extra state about it. Before writing anything doorstep checks whether `gitops.branch` already holds the desired value. If it does, nothing is written, and among the last 20 commits touching the file doorstep reports:

- the commit whose trailers name this app, digest and deployment, together with its `Doorstep-Previous-Digest`; or else
- the newest commit whose trailers name this app and digest (an earlier deployment of the same image), without a previous digest; or else
- no commit at all, when the value was set by hand or by commits doorstep cannot attribute. The deployment then has no pin commit of its own; doorstep never reports a commit that belongs to another app sharing the file.

A crash after a commit or merge, or a digest that was already current, therefore never produces a second commit.

A pin that already landed and was later replaced is never written again. If `gitops.branch` no longer holds the desired value but a commit among the last 20 (commit mode) or a merged pull request (pull request mode) shows that this deployment pinned it before, `Pin` fails with `ErrPinOverwritten` in both modes instead of writing the old digest back. Pins for one app are serialised by doorstep's engine (one active deployment per target), so this only happens when a stale run is replayed.

## Troubleshooting

| Error | Meaning | What to do |
| --- | --- | --- |
| `ErrPinNotFound` | No kustomization or YAML file in `gitops.path`, no `images` entry named `pin.kustomize.image`, or nothing at `pin.yaml.path` | Add the entry or value (a placeholder is enough), or fix the configuration |
| `ErrAmbiguousPin` | Two entries with the same name, a duplicated key, or the path in several documents | Remove the duplicate |
| `ErrUnsupportedYAML` | The value is written in a form doorstep will not edit (see [What cannot be edited](#what-cannot-be-edited)) | Rewrite that value as a single-line string |
| `ErrConflict` | The file kept changing under doorstep's write, or the pull request kept conflicting with the base | Retry later; look for something rewriting the file in a loop |
| `ErrBranchProtected` | Branch protection or a ruleset refused doorstep's direct commit | Add the App to the bypass list or switch to `pull_request` mode |
| `ErrPinOverwritten` | This deployment's pin landed, but `gitops.branch` now holds another value (a newer pin, or a merged pull request with other content) | Nothing to repair: deploy again if this digest should be live |
| `github.ErrRefNotFound` | `gitops.branch` does not exist, or the App cannot see the repository | Fix `gitops.branch` or the installation's repository selection |
| `ErrPullRequestClosed` | Someone closed doorstep's pull request | Queue the deployment again if it should still ship |
| `ErrPullRequestTampered` | The pull request comes from a fork or changes other files | Inspect and close the pull request |
| `ErrMergeTimeout` | The pull request did not become mergeable in 30 minutes | Check required checks and reviews on the GitOps repository |
| `github.ErrFileTooLarge` / `github.ErrNotAFile` | The pin file is over 1 MiB, or a directory, symlink or submodule | Pin a smaller regular file |
| `403 Resource not accessible by integration` | The App lacks *Contents* or *Pull requests* write access, or the repository is not selected in the installation | See [github-app.md](github-app.md#permissions) |

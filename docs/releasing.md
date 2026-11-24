# Releasing

Releases are cut by pushing a version tag. `.github/workflows/release.yml` does the rest.

## Cutting a release

1. Pick a commit on `main` whose CI run is green. For a final release, [dogfood](#dogfooding) it as a release candidate first once a self-hosted install exists.
2. Preview the notes with `just changelog --unreleased`.
3. Tag and push:

   ```sh
   git tag -s vX.Y.Z
   git push origin vX.Y.Z
   ```

   The workflow does not check that the tag is signed or annotated. The enforced control is the `v*` tag ruleset (see One-time setup); signing is a maintainer convention.

4. When the workflow has finished, advance the [release branch](#the-release-branch) to the tag.

Tags with a hyphen (`v0.2.0-rc.1`) become GitHub prereleases. Do not re-use or move a tag.

If the release contains a contract migration (`internal/store/migrations/NNNNN_contract_*.sql`), check that it raises the compatibility floor to the newest migration of the previous release tag, `git ls-tree --name-only <previous tag> internal/store/migrations/ | tail -1` ([database.md](database.md#compatibility-across-one-release)).

The first release that contains migration `00012_schema_compatibility.sql` cannot be reverted onto the binary before it: compatibility mode ships in that release, so the previous binary refuses the migrated schema, and an old pod that restarts during the rollout crashloops until the new one is ready. Say so in its release notes, and tell self-hosted installs to take a database backup first, because restoring it is the only way back ([self-hosting.md](self-hosting.md#safeguards)). One-release compatibility applies from the release after it.

## The release branch

The `release` branch is what a self-hosted doorstep follows ([self-hosting.md](self-hosting.md)). It only ever points at commits that have a release image, because doorstep deploys `:<commit SHA>` for the head of the branch it follows and only tagged commits have images. After the release workflow has pushed the images, fast-forward it to the tagged commit:

```sh
git fetch origin
git push origin 'vX.Y.Z^{commit}:refs/heads/release'
```

The push is a fast-forward because every tag is on `main` and `release` only ever holds earlier tagged commits of `main`; if git refuses it as non-fast-forward, stop and find out why instead of forcing it. Advancing `release` is what upgrades a self-hosted install, so it is a deliberate step rather than part of the workflow ([ADR 0022](decisions/0022-self-hosting-through-a-release-branch.md)). Pushing a release candidate tag there is how a candidate is dogfooded.

## Dogfooding

Not applicable until a self-hosted install of doorstep exists. Once one does, every final release is deployed to it through doorstep before it is tagged. This is a manual checklist; nothing in CI or the release workflow enforces it yet:

1. Tag the candidate commit `vX.Y.Z-rc.N` and push the tag.
2. Wait for the release workflow to finish, so `ghcr.io/sharpintelligence/doorstep:<commit SHA>` exists and is signed.
3. Fast-forward `release` to the candidate tag, as in [The release branch](#the-release-branch).
4. Confirm the self-deployment reached `succeeded`, including its soak, in the web UI or with `GET /api/v1/apps/doorstep/deployments`. If it failed or was rolled back, fix the problem and start again with the next `-rc`.
5. Tag `vX.Y.Z` on the same commit and push it, then fast-forward `release` to the final tag, which is a no-op for the branch because the commit is the same.

The final tag rebuilds the same commit, so `:<commit SHA>` then points at the final build, which differs from the candidate only in its version string. Digest pins stay valid; `doorstep deploy doorstep <commit SHA>` moves the install to the final build's digest if wanted.

## What the workflow does

The workflow token has no permissions by default; each job requests only what it needs. Jobs run in sequence, so the GitHub Release appears only after the image is signed and attested.

| Job | Permissions | Work |
| --- | --- | --- |
| `build` | `contents: read`, `packages: write` | rejects tags that do not match `^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$` or whose commit is not an ancestor of `origin/main`; builds `linux/amd64` and `linux/arm64`; pushes to GHCR; outputs the index digest |
| `sbom` | `packages: read` | for each platform manifest, generates an SPDX SBOM with syft (version pinned in the workflow) and hands it on as a short-lived artifact |
| `attest` | `packages: write`, `id-token: write`, `attestations: write` | attests build provenance for the index and each platform manifest, attests each platform SBOM, and signs the index and its platform manifests with cosign |
| `release` | `contents: write` | renders notes with git-cliff, appends the image digest and verification commands, and runs `gh release create` |

The job that compiles third-party module code cannot mint OIDC tokens or write to the repository, the signing job runs no build and no scanner (syft runs in a job that can only read packages), and only the last job can write contents. The tag name reaches shell steps only through `env`, never by interpolation. The release workflow does not re-run the tests: `main` is protected and CI already ran on the commit.

## Image tags

Each release pushes two tags: `ghcr.io/sharpintelligence/doorstep:<git tag>` (for example `v0.1.0`) and `ghcr.io/sharpintelligence/doorstep:<full 40-character commit SHA>`. There is no `latest` and there are no floating major or minor tags, in line with doorstep's own rule that deployments pin digests, never mutable tags. The release notes always show the `@sha256:` digest of the multi-arch index.

To resolve a tag to its digest:

```sh
docker buildx imagetools inspect ghcr.io/sharpintelligence/doorstep:v0.1.0
crane digest ghcr.io/sharpintelligence/doorstep:v0.1.0
```

Image labels set `org.opencontainers.image.source` (which links the GHCR package to the repository), `revision`, `version`, `licenses` (Apache-2.0), `created`, `title` and `description`. `doorstep version` in the image reports the version, commit and commit time, set through link-time variables because the build has no `.git` directory.

## What is attested

- SLSA build provenance, from `actions/attest-build-provenance`, naming the workflow, tag and commit that produced the image. It is attested for the index digest and for each platform manifest digest.
- An SPDX JSON SBOM per platform, from syft via `anchore/sbom-action` and `actions/attest-sbom`, attested against that platform's manifest digest.
- A keyless cosign signature on the index digest and, through `cosign sign --recursive`, on each platform manifest digest.

Attestations are stored in the GitHub attestation store and pushed to the registry as referrers. The buildx built-in provenance and SBOM are turned off, so the index contains exactly two platform manifests and there is one provenance format.

SBOM attestations attach to platform manifests, not the index: resolve the platform digest (`docker buildx imagetools inspect`) before verifying an SBOM.

## Verifying an image

Signature, with the identity anchored to this workflow on a version tag:

```sh
cosign verify ghcr.io/sharpintelligence/doorstep@sha256:<digest> \
  --certificate-identity-regexp '^https://github.com/sharpintelligence/doorstep/\.github/workflows/release\.yml@refs/tags/v' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

Provenance:

```sh
gh attestation verify oci://ghcr.io/sharpintelligence/doorstep@sha256:<digest> \
  --repo sharpintelligence/doorstep \
  --signer-workflow sharpintelligence/doorstep/.github/workflows/release.yml
```

SBOM, against a platform manifest digest:

```sh
gh attestation verify oci://ghcr.io/sharpintelligence/doorstep@sha256:<platform-digest> \
  --repo sharpintelligence/doorstep \
  --signer-workflow sharpintelligence/doorstep/.github/workflows/release.yml \
  --predicate-type https://spdx.dev/Document/v2.3
```

## After a release

Update the image digest in `deploy/kustomize/examples/homelab/kustomization.yaml` to the new release, so the example installs what was just published. The base keeps its placeholder digest; users override it in their own overlay (see [install.md](install.md#pinning-the-image)).

## One-time setup

- After the first release, make the GHCR package public (package settings, change visibility). New GHCR packages start private.
- Add a repository ruleset restricting creation, update and deletion of `v*` tags to maintainers.
- Create the `release` branch at the first release tag (`git push origin 'v0.1.0^{commit}:refs/heads/release'`) and add a branch ruleset for it that restricts updates to maintainers and blocks force pushes and deletions.

## Changelog

Notes come from `git-cliff` and the committed `cliff.toml`; there is no `CHANGELOG.md`. History consists of squash-merged pull request titles such as `Title (doorstep-ehl.1.1) (#2)`, not conventional commits, so the configuration strips bead IDs, links pull request numbers, and groups Dependabot `Bump` commits under Dependencies. Everything else is listed under Changes. For a final release (no hyphen in the tag) the workflow passes `--ignore-tags '-'`, so the notes cover everything since the previous final release rather than since the last release candidate. Use `just changelog --latest --ignore-tags '-'` to preview that locally.

## Why these tools

- git-cliff is one static binary, read-only on the repository and previewable locally, and is pinned in `mise.toml`. release-please needs conventional commits and a bot that opens release pull requests; goreleaser would take over the build and duplicate the `Dockerfile` and `justfile`.
- GitHub attestations are what `gh attestation verify` checks, are Sigstore-backed and keyless, and live in both the attestation store and the registry.
- Cross-compilation instead of QEMU: the binary is pure Go (`CGO_ENABLED=0`) and the final stage has no `RUN`, so emulation is never needed.
- Cosign (`cosign-release`) and syft (`syft-version`) are pinned so the tools change only through a reviewed pull request.
- `release.yml` only runs on tags, so `just lint-actions` (part of `just check`) and the `image` job in CI exercise the workflow syntax and the image build on every pull request.

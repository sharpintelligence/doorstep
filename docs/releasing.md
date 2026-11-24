# Releasing

Releases are cut by pushing a version tag. `.github/workflows/release.yml` does the rest.

## Cutting a release

1. Pick a commit on `main` whose CI run is green.
2. Preview the notes with `just changelog --unreleased`.
3. Tag and push:

   ```sh
   git tag -s vX.Y.Z
   git push origin vX.Y.Z
   ```

Tags with a hyphen (`v0.2.0-rc.1`) become GitHub prereleases. Do not re-use or move a tag.

## What the workflow does

The workflow token has no permissions by default; each job requests only what it needs. Jobs run in sequence, so the GitHub Release appears only after the image is signed and attested.

| Job | Permissions | Work |
| --- | --- | --- |
| `build` | `contents: read`, `packages: write` | rejects tags that do not match `^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$` or whose commit is not an ancestor of `origin/main`; builds `linux/amd64` and `linux/arm64`; pushes to GHCR; outputs the index digest |
| `attest` | `packages: write`, `id-token: write`, `attestations: write` | generates an SPDX SBOM with syft, attests build provenance and the SBOM, and signs the index with cosign |
| `release` | `contents: write` | renders notes with git-cliff, appends the image digest and verification commands, and runs `gh release create` |

The job that compiles third-party module code cannot mint OIDC tokens or write to the repository, the signing job runs no build, and only the last job can write contents. The tag name reaches shell steps only through `env`, never by interpolation. The release workflow does not re-run the tests: `main` is protected and CI already ran on the commit.

## Image tags

Each release pushes two tags: `ghcr.io/sharpintelligence/doorstep:<git tag>` (for example `v0.1.0`) and `ghcr.io/sharpintelligence/doorstep:<full 40-character commit SHA>`. There is no `latest` and there are no floating major or minor tags, in line with doorstep's own rule that deployments pin digests, never mutable tags. The release notes always show the `@sha256:` digest of the multi-arch index.

To resolve a tag to its digest:

```sh
docker buildx imagetools inspect ghcr.io/sharpintelligence/doorstep:v0.1.0
crane digest ghcr.io/sharpintelligence/doorstep:v0.1.0
```

Image labels set `org.opencontainers.image.source` (which links the GHCR package to the repository), `revision`, `version`, `licenses` (Apache-2.0), `created`, `title` and `description`. `doorstep version` in the image reports the version, commit and commit time, set through link-time variables because the build has no `.git` directory.

## What is attested

- SLSA build provenance, from `actions/attest-build-provenance`, naming the workflow, tag and commit that produced the image.
- An SPDX JSON SBOM, from syft via `anchore/sbom-action` and `actions/attest-sbom`.
- A keyless cosign signature on the index digest. Only the index is signed; the per-platform manifests are covered through it.

Attestations are stored in the GitHub attestation store and pushed to the registry as referrers. The buildx built-in provenance and SBOM are turned off, so the index contains exactly two platform manifests and there is one provenance format.

Known limitation: syft scans the runner platform (`linux/amd64`) of the index. For a static Go binary the package list is the same on both architectures.

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

SBOM:

```sh
gh attestation verify oci://ghcr.io/sharpintelligence/doorstep@sha256:<digest> \
  --repo sharpintelligence/doorstep \
  --signer-workflow sharpintelligence/doorstep/.github/workflows/release.yml \
  --predicate-type https://spdx.dev/Document/v2.3
```

## One-time setup

- After the first release, make the GHCR package public (package settings, change visibility). New GHCR packages start private.
- Add a repository ruleset restricting creation, update and deletion of `v*` tags to maintainers.

## Changelog

Notes come from `git-cliff` and the committed `cliff.toml`; there is no `CHANGELOG.md`. History consists of squash-merged pull request titles such as `Title (doorstep-ehl.1.1) (#2)`, not conventional commits, so the configuration strips bead IDs, links pull request numbers, and groups Dependabot `Bump` commits under Dependencies. Everything else is listed under Changes.

## Why these tools

- git-cliff is one static binary, read-only on the repository and previewable locally, and is pinned in `mise.toml`. release-please needs conventional commits and a bot that opens release pull requests; goreleaser would take over the build and duplicate the `Dockerfile` and `justfile`.
- GitHub attestations are what `gh attestation verify` checks, are Sigstore-backed and keyless, and live in both the attestation store and the registry.
- Cross-compilation instead of QEMU: the binary is pure Go (`CGO_ENABLED=0`) and the final stage has no `RUN`, so emulation is never needed.
- Cosign is pinned (`cosign-release`) so the signing tool changes only through a reviewed pull request.
- `release.yml` only runs on tags, so `just lint-actions` (part of `just check`) and the `image` job in CI exercise the workflow syntax and the image build on every pull request.

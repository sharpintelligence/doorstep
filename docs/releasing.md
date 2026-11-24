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

   The workflow does not check that the tag is signed or annotated. The enforced control is the `v*` tag ruleset (see One-time setup); signing is a maintainer convention.

Tags with a hyphen (`v0.2.0-rc.1`) become GitHub prereleases. Do not re-use or move a tag.

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

## Changelog

Notes come from `git-cliff` and the committed `cliff.toml`; there is no `CHANGELOG.md`. History consists of squash-merged pull request titles such as `Title (doorstep-ehl.1.1) (#2)`, not conventional commits, so the configuration strips bead IDs, links pull request numbers, and groups Dependabot `Bump` commits under Dependencies. Everything else is listed under Changes. For a final release (no hyphen in the tag) the workflow passes `--ignore-tags '-'`, so the notes cover everything since the previous final release rather than since the last release candidate. Use `just changelog --latest --ignore-tags '-'` to preview that locally.

## Why these tools

- git-cliff is one static binary, read-only on the repository and previewable locally, and is pinned in `mise.toml`. release-please needs conventional commits and a bot that opens release pull requests; goreleaser would take over the build and duplicate the `Dockerfile` and `justfile`.
- GitHub attestations are what `gh attestation verify` checks, are Sigstore-backed and keyless, and live in both the attestation store and the registry.
- Cross-compilation instead of QEMU: the binary is pure Go (`CGO_ENABLED=0`) and the final stage has no `RUN`, so emulation is never needed.
- Cosign (`cosign-release`) and syft (`syft-version`) are pinned so the tools change only through a reviewed pull request.
- `release.yml` only runs on tags, so `just lint-actions` (part of `just check`) and the `image` job in CI exercise the workflow syntax and the image build on every pull request.

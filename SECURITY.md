# Security policy

## Supported versions

| Version | Receives security fixes |
| --- | --- |
| Before 1.0 | `main`, and the latest tagged release once one exists. Fixes are not backported. |
| From 1.0 | The latest minor release. |

## Reporting a vulnerability

Report vulnerabilities privately through GitHub: open the repository's **Security** tab, choose **Report a vulnerability**, or go straight to <https://github.com/sharpintelligence/doorstep/security/advisories/new>.

Do not open a public issue, pull request or discussion for a suspected vulnerability.

Please include:

- the doorstep version, commit or image digest;
- the configuration involved, with secrets removed;
- steps to reproduce, or a proof of concept;
- the impact you expect: what an attacker gains and what they need first.

## What to expect

doorstep is maintained by one person, so these are best-effort targets rather than guarantees:

- an acknowledgement within 7 days;
- coordinated disclosure through a GitHub Security Advisory, with the fix released before details are published;
- credit in the advisory if you want it.

There is no bug bounty.

## Scope

In scope: the `doorstep` binary, the container image, the kustomize manifests under `deploy/kustomize` and the release workflow. Examples:

- authentication or role bypass, including API token and session handling;
- CSRF protection bypass in the web UI;
- GitHub webhook signature bypass;
- secrets disclosed in logs, errors, API responses or the UI;
- writes outside the configured GitOps repository and paths, or any write to Argo CD or the cluster (doorstep is designed to only read them);
- a signed image or attestation that does not verify.

Out of scope: vulnerabilities in Argo CD, GitHub, Postgres or other software doorstep talks to (report those upstream), and misconfiguration that the documentation warns against.

## Background

- [Security posture](docs/architecture.md#security-posture) describes the trust boundaries and what doorstep is allowed to touch.
- [Authentication and roles](docs/authentication.md) describes sign-in, tokens and roles.
- [Verifying an image](docs/releasing.md#verifying-an-image) shows how to check signatures and attestations.

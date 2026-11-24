# GitHub App

doorstep talks to GitHub as a [GitHub App](https://docs.github.com/en/apps/creating-github-apps/about-creating-github-apps/about-creating-github-apps). The App lets doorstep:

- commit image digest pins to the GitOps repository and, where a repository requires it, open pull requests for them;
- read check runs and commit statuses on application repositories to decide whether a commit is green;
- write commit statuses that mirror doorstep's own gates;
- receive webhooks (pushes, check and status updates) that start deployments.

One App serves every repository doorstep works with, across any number of organizations and personal accounts. There are no personal access tokens, deploy keys or SSH keys.

## Create the App

Create the App once, under whichever account should own it (**Settings → Developer settings → GitHub Apps → New GitHub App** for a personal account, or the organization's settings for an organization).

| Setting | Value |
| --- | --- |
| Homepage URL | your doorstep URL or this repository |
| Webhook | active; URL `https://<doorstep host>/webhooks/github` |
| Webhook secret | a long random value, for example `openssl rand -hex 32` (always set one) |
| Where can this GitHub App be installed? | **Any account**, so it can be installed on several organizations and personal accounts. Choose *Only on this account* if every repository lives under the owning account. |

The webhook handler is not built yet; until it is, you can leave the webhook inactive. Installing the App does not depend on it.

### Permissions

Repository permissions:

| Permission | Access | Needed on | Why |
| --- | --- | --- | --- |
| Metadata | Read | all | mandatory for every App |
| Contents | Read and write | GitOps repository; read on application repositories | read manifests and commit digest pins through the Contents and Git Data APIs; read the head of an app's branch and receive push events |
| Pull requests | Read and write | GitOps repository | open, update, squash-merge and close doorstep's own pin pull requests for apps in `pull_request` mode |
| Checks | Read | application repositories | read check runs for the CI gate |
| Commit statuses | Read and write | application repositories | read statuses for the CI gate; write statuses that mirror doorstep's gates |

No organization or account permissions are needed.

Before recording a push, doorstep reads `GET /repos/{owner}/{repo}/git/ref/heads/{branch}` to check that the pushed commit is still the branch head (Contents read).

To decide whether a commit is green, doorstep reads, per tracked commit and with the installation token, `GET /repos/{owner}/{repo}/commits/{sha}/status` and the commit's check runs. When every app following the commit lists `required_checks`, it reads only those check runs, one `GET /repos/{owner}/{repo}/commits/{sha}/check-runs?check_name={name}&filter=latest` per name. Otherwise it reads every check run (`check-runs?filter=latest`) and `GET /repos/{owner}/{repo}/commits/{sha}/check-suites`. These need only the Checks and Commit statuses read permissions above. Results are read 100 per page; more than 1,000 results for one request (all check runs, the check runs of one name, statuses or suites) is treated as an error rather than evaluated on a partial list, so very large monorepos should list `required_checks`.

For apps in `pull_request` mode, enable **Allow squash merging** on the GitOps repository. doorstep merges its own pull requests once required status checks pass; if branch protection or a ruleset requires approving reviews, or forbids direct pushes for apps in `commit` mode, add the App to the bypass list. doorstep only ever merges a pull request that changes nothing but its own pin edit ([gitops.md](gitops.md#pull_request)).

**GitHub applies one permission set to every repository an installation can reach.** The table says which repositories *need* each permission, but GitHub cannot express that: if the App can write contents in the GitOps repository, it can write contents in every repository the installation includes. Least privilege therefore comes from the repository selection:

- install with **Only select repositories**, never *All repositories*;
- select only the GitOps repository and the application repositories doorstep deploys;
- review the selection when repositories are added or retired.

Splitting doorstep into a writer App (GitOps repository) and a reader App (application repositories) would narrow this further. It is a possible future hardening, not something doorstep supports today.

### Events

Subscribe to:

- **Push**
- **Check suite**
- **Check run**
- **Status**
- **Pull request**

`installation` and `installation_repositories` events are delivered to every App without a subscription.

## Install the App

From the App's public page (`https://github.com/apps/<slug>`), choose **Install** and pick an account. Repeat for every organization and personal account that owns repositories doorstep should manage, each time with **Only select repositories**.

doorstep does not need to be told about installations. For every repository it works with, it asks GitHub which installation covers that repository (`GET /repos/{owner}/{repo}/installation`) and uses that installation's token. Repositories under different owners are therefore served by different installations and different tokens automatically.

## Private key

On the App's settings page, under **Private keys**, choose **Generate a private key**. GitHub downloads a PEM file (PKCS #1, `-----BEGIN RSA PRIVATE KEY-----`). doorstep also accepts PKCS #8 (`-----BEGIN PRIVATE KEY-----`) as long as it holds an RSA key of at least 2048 bits.

Give doorstep the App ID and the key, from exactly one of two sources. See [configuration.md](configuration.md#github-app) for every setting.

The App ID can be the numeric **App ID** or the **Client ID** shown on the App's settings page. GitHub recommends the Client ID; older GitHub Enterprise Server versions accept only the numeric ID.

### Kubernetes

Store the key in a Secret and mount it as a file:

```sh
kubectl -n doorstep create secret generic doorstep-github-app \
  --from-file=private-key.pem=./doorstep.2026-01-01.private-key.pem
```

```yaml
spec:
  containers:
    - name: doorstep
      env:
        - name: DOORSTEP_GITHUB_APP_ID
          value: "Iv23liAbCdEf"
        - name: DOORSTEP_GITHUB_APP_PRIVATE_KEY_FILE
          value: /var/run/secrets/github/private-key.pem
      volumeMounts:
        - name: github-app
          mountPath: /var/run/secrets/github
          readOnly: true
  volumes:
    - name: github-app
      secret:
        secretName: doorstep-github-app
```

### Docker Compose

Pass the key inline. Env files cannot hold multi-line values, so write the newlines as literal `\n`; doorstep converts them back before decoding:

```sh
printf 'DOORSTEP_GITHUB_APP_PRIVATE_KEY=%s\n' "$(awk '{printf "%s\\n", $0}' doorstep.2026-01-01.private-key.pem)" >> github.env
```

```yaml
services:
  doorstep:
    env_file: github.env
    environment:
      DOORSTEP_GITHUB_APP_ID: "Iv23liAbCdEf"
```

Keep `github.env` out of version control.

### Startup

When `DOORSTEP_GITHUB_APP_ID` or a key is set, `doorstep serve` reads and parses the key at startup and refuses to start if it is missing, malformed or too small. It then logs `github app configured` with the App ID and API URL. It does not call GitHub at startup, and readiness does not depend on GitHub being reachable. Without any GitHub settings it logs `github app not configured` and runs without GitHub access.

## GitHub Enterprise Server

Set `DOORSTEP_GITHUB_API_URL` to the REST API root, for example `https://ghe.example.com/api/v3`. The URL must use `https`; plain `http` is accepted only for loopback hosts (local fakes and tests).

## How tokens are handled

- For each installation lookup and token mint, doorstep signs a fresh JWT with the private key (RS256, issued 60 seconds in the past to absorb clock drift, valid for 9 minutes). JWTs are used for nothing else.
- The repository-to-installation lookup is cached for one hour per repository. A repository that is not covered, or whose installation is suspended, is never cached. When minting a token finds the installation uninstalled (`404`) or suspended (`403`), every cached lookup pointing at it is dropped and the request fails with `github app is not installed for the repository` or `github app installation is suspended`.
- Installation tokens are valid for one hour. doorstep keeps one per installation, in memory only, and mints a new one five minutes before expiry. Concurrent requests share a single lookup per repository and a single mint per installation, including its failure, so an outage costs one call rather than one per waiting request. A shared call is bounded to 30 seconds. A `401` from GitHub drops the cached token so the next request mints a fresh one.
- Tokens are minted with the installation's full permission set and repository selection.
- Tokens, JWTs and keys are never logged or written to disk, and are only sent to the configured API host: a request to any other scheme or host, including a redirect, is refused before it is sent.

## Rotating the private key

1. Generate a new private key on the App's settings page. GitHub keeps the old one valid.
2. Replace the Secret (or env file) with the new key and restart or roll doorstep.
3. Once every replica runs with the new key, delete the old key on the App's settings page.

Installation tokens minted with the old key stay valid until they expire (at most one hour), so a rotation needs no downtime.

## Troubleshooting

| Symptom | Cause and fix |
| --- | --- |
| `github app is not installed for the repository` | The App is not installed on the repository's owner, or the repository is not in the installation's selected repositories. Install the App on that account or add the repository to the selection. doorstep picks up the change on the next request. |
| `github app installation is suspended` | An owner of that account suspended the installation. Unsuspend it under the account's **Settings → GitHub Apps**. |
| `github api: 401 Unauthorized` with `'Expiration time' claim` or `'Issued at' claim` in the message | The host clock is off by more than about a minute. Run NTP on the node. |
| `github api: 401 Unauthorized` with `A JSON web token could not be decoded` or `Integration not found` | The App ID does not match the private key, or the key was deleted from the App. Check both settings. |
| `invalid github app private key` at startup | The key is not a PEM RSA key of at least 2048 bits, or an inline value lost its line breaks. Use the file form, or escape newlines as `\n`. |
| `request host does not match the github api url` | A request or redirect pointed somewhere other than `DOORSTEP_GITHUB_API_URL`. Check the API URL, in particular for GitHub Enterprise Server. |

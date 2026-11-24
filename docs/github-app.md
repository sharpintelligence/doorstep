# GitHub App

doorstep talks to GitHub as a [GitHub App](https://docs.github.com/en/apps/creating-github-apps/about-creating-github-apps/about-creating-github-apps). The App lets doorstep:

- commit image digest pins to the GitOps repository and, where a repository requires it, open pull requests for them;
- read check runs and commit statuses on application repositories to decide whether a commit is green;
- write commit statuses that mirror doorstep's own gates;
- create deployments and deployment statuses on application repositories, so GitHub shows what runs in each environment ([notifications.md](notifications.md#github-deployments));
- receive webhooks (pushes, check and status updates) that start deployments.

One App serves every repository doorstep works with, across any number of organizations and personal accounts. There are no personal access tokens, deploy keys or SSH keys.

## Create the App

Create the App once, under whichever account should own it (**Settings → Developer settings → GitHub Apps → New GitHub App** for a personal account, or the organization's settings for an organization).

| Setting | Value |
| --- | --- |
| Homepage URL | your doorstep URL or this repository |
| Webhook | active when GitHub can reach doorstep; URL `https://<doorstep host>/webhooks/github`. Leave it inactive for a polling-only install |
| Webhook secret | a long random value, for example `openssl rand -hex 32` (always set one when the webhook is active) |
| Where can this GitHub App be installed? | **Any account**, so it can be installed on several organizations and personal accounts. Choose *Only on this account* if every repository lives under the owning account. |

Webhooks are optional: doorstep also polls GitHub on an interval, so an install that GitHub cannot reach still deploys, just later. See [Webhooks](#webhooks) and [Polling](#polling).

### Permissions

Repository permissions:

| Permission | Access | Needed on | Why |
| --- | --- | --- | --- |
| Metadata | Read | all | mandatory for every App |
| Contents | Read and write | GitOps repository; read on application repositories | read manifests and commit digest pins through the Contents and Git Data APIs; read the head of an app's branch and receive push events |
| Pull requests | Read and write | GitOps repository | open, update, squash-merge and close doorstep's own pin pull requests for apps in `pull_request` mode |
| Checks | Read | application repositories | read check runs for the CI gate |
| Commit statuses | Read and write | application repositories | read statuses for the CI gate; write statuses that mirror doorstep's gates |
| Deployments | Read and write | application repositories | create a GitHub deployment per doorstep deployment and post its statuses (`queued`, `in_progress`, `success`, `failure`, `inactive`) |

No organization or account permissions are needed.

Before recording a push, doorstep reads `GET /repos/{owner}/{repo}/git/ref/heads/{branch}` to check that the pushed commit is still the branch head (Contents read).

To decide whether a commit is green, doorstep reads, per tracked commit and with the installation token, `GET /repos/{owner}/{repo}/commits/{sha}/status` and the commit's check runs. When every app following the commit lists `required_checks`, it reads only those check runs, one `GET /repos/{owner}/{repo}/commits/{sha}/check-runs?check_name={name}&filter=latest` per name. Otherwise it reads every check run (`check-runs?filter=latest`) and `GET /repos/{owner}/{repo}/commits/{sha}/check-suites`. These need only the Checks and Commit statuses read permissions above. Results are read 100 per page; more than 1,000 results for one request (all check runs, the check runs of one name, statuses or suites) is treated as an error rather than evaluated on a partial list, so very large monorepos should list `required_checks`.

For apps in `pull_request` mode, enable **Allow squash merging** on the GitOps repository. doorstep merges its own pull requests once required status checks pass; if branch protection or a ruleset requires approving reviews, or forbids direct pushes for apps in `commit` mode, add the App to the bypass list. doorstep only ever merges a pull request that changes nothing but its own pin edit ([gitops.md](gitops.md#pull_request)).
**Adding a permission to an existing App.** When you add *Deployments: Read and write* to an App that is already installed, GitHub asks the owner of each installation to accept the new permission (on the account's **Settings → GitHub Apps** page, or from the email GitHub sends). Until an installation accepts it, doorstep's deployment reports for that installation's repositories fail with `403`; doorstep logs `cannot report deployments to github` once per repository per hour and keeps deploying. To skip GitHub deployments for an app instead, set `apps[].source.deployments: false`.

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

`installation` and `installation_repositories` events are delivered to every App without a subscription. Other events, including **Pull request**, are answered with `200` and ignored, so subscribing to more does no harm.

## Install the App

From the App's public page (`https://github.com/apps/<slug>`), choose **Install** and pick an account. Repeat for every organization and personal account that owns repositories doorstep should manage, each time with **Only select repositories**.

doorstep does not need to be told about installations. For every repository it works with, it asks GitHub which installation covers that repository (`GET /repos/{owner}/{repo}/installation`) and uses that installation's token. Repositories under different owners are therefore served by different installations and different tokens automatically.

## Private key

On the App's settings page, under **Private keys**, choose **Generate a private key**. GitHub downloads a PEM file (PKCS #1, `-----BEGIN RSA PRIVATE KEY-----`). doorstep also accepts PKCS #8 (`-----BEGIN PRIVATE KEY-----`) as long as it holds an RSA key of at least 2048 bits.

Give doorstep the App ID and the key, from exactly one of two sources. See [configuration.md](configuration.md#github-app) for every setting.

The App ID can be the numeric **App ID** or the **Client ID** shown on the App's settings page. GitHub recommends the Client ID; older GitHub Enterprise Server versions accept only the numeric ID.

### Kubernetes

Store the key under the key `github-app-private-key.pem` in the Secret `doorstep` that the [kustomize install base](install.md#secret-keys) mounts at `/etc/doorstep/secrets`:

```sh
kubectl -n doorstep create secret generic doorstep \
  --from-file=github-app-private-key.pem=./doorstep.2026-01-01.private-key.pem \
  --from-literal=database-url="$DATABASE_URL"
```

Then set the App ID and the key path in the configuration file:

```yaml kubernetes
github:
  app_id: "Iv23liAbCdEf"
  private_key_file: /etc/doorstep/secrets/github-app-private-key.pem
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

## Webhooks

doorstep accepts App webhooks at `POST /webhooks/github` on the main listener. It is the only path GitHub needs to reach; it is not behind doorstep's sign-in, because every delivery is authenticated by its signature instead.

1. Generate a secret: `openssl rand -hex 32`.
2. On the App's settings page, set the **Webhook URL** to `https://<doorstep host>/webhooks/github`, paste the secret into **Webhook secret**, keep SSL verification on, and mark the webhook **Active**. GitHub Apps always send `application/json`.
3. Give doorstep the same secret, from exactly one of two sources ([configuration.md](configuration.md#github-app)):
   - a file: `github.webhook_secret_file` in `config.yaml` or `DOORSTEP_GITHUB_WEBHOOK_SECRET_FILE`. On Kubernetes, store it under the key `github-webhook-secret` in the Secret `doorstep` ([install.md](install.md#secret-keys)) and set `webhook_secret_file: /etc/doorstep/secrets/github-webhook-secret`;
   - inline: `DOORSTEP_GITHUB_WEBHOOK_SECRET` (environment only).

```sh
kubectl -n doorstep create secret generic doorstep \
  --from-file=github-app-private-key.pem=./doorstep.2026-01-01.private-key.pem \
  --from-literal=github-webhook-secret="$(openssl rand -hex 32)" \
  --from-literal=database-url="$DATABASE_URL"
```

```yaml kubernetes
github:
  app_id: "Iv23liAbCdEf"
  private_key_file: /etc/doorstep/secrets/github-app-private-key.pem
  webhook_secret_file: /etc/doorstep/secrets/github-webhook-secret
```

The secret is read once at startup (one trailing newline is trimmed); an empty or unreadable secret stops `serve`. A webhook secret without the App ID and key is a configuration error. Without a secret the route is not mounted at all (`404`) and `serve` logs `github webhooks disabled, polling only`: doorstep never accepts unsigned payloads.

### What a delivery does

| Event | Effect |
| --- | --- |
| `push` to `refs/heads/<branch>` | handed to intake with the pusher (`sender.login`, else `pusher.name`). Apps following that repository and branch track the commit if it is still the branch head; other branches match no app and cost nothing. Branch deletions, tag pushes and the all-zero SHA are ignored. |
| `check_run`, `check_suite`, `status` (any action) | re-evaluates the open tracked commits for that repository and SHA (`check_run.head_sha`, `check_suite.head_sha`, `sha`); a green commit is enqueued once. Commits doorstep does not track cost one database lookup and no GitHub call. |
| `installation`, `installation_repositories` | drops this replica's cached installation lookup and token for that installation and for every removed repository, so the change applies at once. Other replicas recover on their own through the one-hour cache and the `404`/`403` handling described under [How tokens are handled](#how-tokens-are-handled). |
| `ping` | answered `200`. |
| anything else | answered `200` and ignored. |

Deliveries are processed while GitHub waits, with up to 25 seconds of work that is not cut short if GitHub hangs up. GitHub stops waiting after 10 seconds, so a slow delivery appears as timed out in **Recent Deliveries** even though doorstep finished processing it; redelivering a timed-out `push` returns `duplicate` once that processing has completed. Otherwise the response GitHub records is the outcome:

| Response | Meaning |
| --- | --- |
| `200 {"status":"processed"}` | handled |
| `200 {"status":"ignored"}` | not an event or ref doorstep acts on |
| `200 {"status":"duplicate"}` | this `push` delivery was already handled; nothing was done again |
| `400` | missing `X-GitHub-Event` or `X-GitHub-Delivery` header, or a payload that is not valid JSON or lacks the repository or SHA |
| `401` | missing or wrong `X-Hub-Signature-256`; checked over the raw body before anything is parsed or stored |
| `405` | not a `POST` |
| `409` | this `push` delivery is being handled by another request right now; redeliver it later if that attempt failed |
| `413` | body larger than 5 MiB |
| `500` | handling failed, for example GitHub could not be read; the delivery is not marked as handled |

**Redeliveries.** Each handled `push` delivery ID is recorded for seven days, so a redelivery that already succeeded is answered `duplicate`. A failed delivery is not recorded, so redelivering it from **Advanced → Recent Deliveries** on the App's settings page processes it again. If doorstep stops while handling a delivery, the delivery is processed again when it is redelivered more than 35 seconds later (the 25 second limit plus a margin). `check_run`, `check_suite` and `status` deliveries are not recorded: handling one twice is harmless, so a redelivery is always processed. GitHub does not redeliver failures by itself; the poller picks up anything a failed or lost delivery missed within one poll interval.

## Polling

Every replica also runs a poller (`intake.poll_interval`, default `1m`; `0` turns it off), so installs that GitHub cannot reach still work. On each cycle one replica, chosen through the database, reads the head of every followed branch (one call per repository and branch, however many apps share it), hands a head to intake exactly like a push unless the database shows intake already tracking it as the head for every app on that branch, and then re-checks open commits that no webhook evaluated in the last half interval. A green head is therefore deployed at most about one interval after CI finishes, even with webhooks off. Because the poller compares against what intake has recorded rather than what it saw last, a force-push back to an earlier commit is picked up even when a webhook moved intake on in between and the webhook for the force-push was lost. The next cycle is due one interval after the previous one finished, and a cycle is cut off after `max(interval, 1m)`, so cycles never overlap even when GitHub is slow.

There is no baseline: when an app is added, or after downtime, the first cycle tracks the current branch head and deploys it once it is green. This is what catches pushes missed while doorstep was down; it only ever deploys through the normal queue.

At the default interval polling costs about 60 branch reads per hour per followed branch plus a few calls per open commit, far below GitHub's 5,000 requests per hour per installation. With webhooks working, the poller mostly reads branch heads and finds nothing new. If both webhooks and polling are off, `serve` logs a warning: nothing detects new commits.

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

## Rotating the webhook secret

GitHub keeps one webhook secret per App, so a rotation has a short window where deliveries fail with `401`:

1. Update the Secret (or environment) with the new value.
2. Set the new value under **Webhook secret** on the App's settings page and restart or roll doorstep straight away; doorstep reads the secret only at startup.
3. Redeliver any deliveries that failed in between, or let the poller catch up.

## Troubleshooting

| Symptom | Cause and fix |
| --- | --- |
| Deliveries fail with `401` | The webhook secret in doorstep and on the App differ, or doorstep was not restarted after a rotation. Set the same value in both and restart. |
| Deliveries fail with `413` | The payload is larger than 5 MiB, typically a push of very many commits. Nothing is lost: the poller reads the branch head within one interval. |
| Deliveries get `404` | No webhook secret is configured, so the route is not mounted. Set `github.webhook_secret_file` or turn the webhook off on the App. |
| `github app is not installed for the repository` | The App is not installed on the repository's owner, or the repository is not in the installation's selected repositories. Install the App on that account or add the repository to the selection. doorstep picks up the change on the next request. |
| `github app installation is suspended` | An owner of that account suspended the installation. Unsuspend it under the account's **Settings → GitHub Apps**. |
| `github api: 401 Unauthorized` with `'Expiration time' claim` or `'Issued at' claim` in the message | The host clock is off by more than about a minute. Run NTP on the node. |
| `github api: 401 Unauthorized` with `A JSON web token could not be decoded` or `Integration not found` | The App ID does not match the private key, or the key was deleted from the App. Check both settings. |
| `invalid github app private key` at startup | The key is not a PEM RSA key of at least 2048 bits, or an inline value lost its line breaks. Use the file form, or escape newlines as `\n`. |
| `cannot report deployments to github` (warning) | The App lacks *Deployments: Read and write*, an installation has not accepted it yet, or the repository is not in the installation's selection. Grant or accept the permission, or set `source.deployments: false` on the app. Deploys are not affected. |
| `request host does not match the github api url` | A request or redirect pointed somewhere other than `DOORSTEP_GITHUB_API_URL`. Check the API URL, in particular for GitHub Enterprise Server. |

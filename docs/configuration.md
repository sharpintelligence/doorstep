# Configuration

## Precedence

Each setting is resolved from the first source that provides it:

1. command-line flags
2. environment variables (prefix `DOORSTEP_`)
3. YAML configuration file (`--config` / `DOORSTEP_CONFIG`), see [Configuration file](#configuration-file)
4. built-in defaults

An environment variable that is set but empty counts as set, and is then validated like any other value. All settings are validated together on startup, and every invalid setting is reported in one error.

A secret with several possible sources is resolved as a whole: when any flag or environment variable for the database URL is set, it replaces `database.url_file` from the file, and when either `DOORSTEP_GITHUB_APP_PRIVATE_KEY` or `DOORSTEP_GITHUB_APP_PRIVATE_KEY_FILE` is set, it replaces `github.private_key_file`. Sources at the same level still conflict, so setting both database environment variables is an error.

Apps, notification targets and the `auth` section are only read from the configuration file; they have no flags or environment variables.

## `doorstep serve`

| Flag | Environment variable | Default | Description |
| --- | --- | --- | --- |
| `--config` | `DOORSTEP_CONFIG` | none | Path to the YAML configuration file. Optional: without it doorstep runs with built-in defaults and no apps, and logs a `no apps configured` warning. A set but empty value is an error. |
| `--listen-addr` | `DOORSTEP_LISTEN_ADDR` | `:8080` | Address for the HTTP listener as `host:port`. The port must be numeric; `0` picks a free port. |
| `--metrics-listen-addr` | `DOORSTEP_METRICS_LISTEN_ADDR` | `:9090` | Address for the `/metrics` listener as `host:port`; `/metrics` is served only there. Must differ from `--listen-addr`. Set it to an empty value to serve `/metrics` on the main listener instead, which is only safe when the main listener is not reachable from the internet. See [metrics.md](metrics.md). |
| `--shutdown-delay` | `DOORSTEP_SHUTDOWN_DELAY` | `5s` | How long the server keeps serving after `SIGINT`/`SIGTERM` while `/readyz` reports `shutting_down`, before it stops accepting connections. Gives load balancers time to stop routing to the instance. Go duration syntax. `0s` disables it; must not be negative. |
| `--shutdown-timeout` | `DOORSTEP_SHUTDOWN_TIMEOUT` | `15s` | How long in-flight requests may run once the shutdown delay has passed, before the server is closed. Go duration syntax (`30s`, `1m`). Must be positive. |
| `--public-url` | `DOORSTEP_PUBLIC_URL` | empty | The external base URL people use to reach doorstep, such as `https://doorstep.example.com`. Notifications link to `<public url>/deployments/<id>` and GitHub deployment statuses use that as their log URL; without it those links are left out. Must be an absolute `http` or `https` URL with a host and without user information, query or fragment; a trailing slash is ignored. See [notifications.md](notifications.md#links). |
| `--log-level` | `DOORSTEP_LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error` (case-insensitive). Health probe and `/metrics` requests are logged at `debug`. |
| `--log-format` | `DOORSTEP_LOG_FORMAT` | `json` | `json` or `text`. Logs are written to stderr. |
| | `DOORSTEP_DATABASE_URL` | none | Postgres connection URL. Required unless a URL file is given; see [Database](#database). |
| `--database-url-file` | `DOORSTEP_DATABASE_URL_FILE` | none | Path to a file holding the Postgres connection URL. Required unless `DOORSTEP_DATABASE_URL` is set; see [Database](#database). |
| `--registry-credentials-file` | `DOORSTEP_REGISTRY_CREDENTIALS_FILE` | empty (anonymous) | Path to a Docker `config.json` with registry credentials; see [Registry credentials](#registry-credentials). When set, `serve` reads it at startup and refuses to start if it is unreadable or malformed. |
| | `DOORSTEP_LOCK_REMINDER_INTERVAL` | `24h` | How long a freeze lock may be held before doorstep sends a reminder, and how often it repeats while the lock stays held. Go duration syntax. `0s` disables reminders (expired locks are still cleaned up); otherwise at least `1m`; must not be negative. See [locks.md](locks.md#reminders). |

The GitHub App settings are described in [GitHub App](#github-app). Every setting in this table except `--config` and `DOORSTEP_DATABASE_URL` can also be set in the configuration file; see [Runtime settings](#runtime-settings). The lock reminder interval has no flag.

Example:

```sh
DOORSTEP_LOG_FORMAT=text doorstep serve --listen-addr 127.0.0.1:9000 --log-level debug
```

A second `SIGINT`/`SIGTERM` during shutdown exits immediately. Keep the Kubernetes `terminationGracePeriodSeconds` above the shutdown delay plus the shutdown timeout.

On start, `serve` connects to Postgres (giving up after 10 seconds), applies pending migrations and only then opens its listener. Any failure exits non-zero without retrying, so the process supervisor's restart backoff handles a database that is not up yet. `/readyz` includes a `postgres` check that pings the connection pool.

## Database

`serve` and `migrate` need a Postgres connection URL from exactly one of two sources; setting both, or neither, is a configuration error.

| Flag | Environment variable | Description |
| --- | --- | --- |
| | `DOORSTEP_DATABASE_URL` | The URL itself, for example `postgres://doorstep:secret@db:5432/doorstep?sslmode=verify-full`. There is no flag for it, because a flag would show the password in process listings. |
| `--database-url-file` | `DOORSTEP_DATABASE_URL_FILE` | A file whose content is the URL, such as a mounted Kubernetes Secret. Leading and trailing whitespace is trimmed; an empty file is an error. The flag replaces either environment variable. |

The configuration file can name the URL file as `database.url_file`; either environment variable or the flag replaces it. The file has no key for the URL itself, because the URL carries the password.

The URL is never logged and never included in error messages.

Connection settings are the standard [pgx connection string parameters](https://pkg.go.dev/github.com/jackc/pgx/v5/pgxpool#ParseConfig), passed in the URL query; doorstep adds no settings of its own. Useful ones:

| Parameter | Default | Description |
| --- | --- | --- |
| `sslmode` | `prefer` | `disable`, `require`, `verify-ca` or `verify-full`. Use `verify-full` (with `sslrootcert`) across untrusted networks. |
| `pool_max_conns` | larger of 4 and the number of CPUs | Maximum open connections per doorstep process. |
| `connect_timeout` | none | Seconds to wait for each connection attempt. |
| `application_name` | `doorstep` | Name shown in `pg_stat_activity`. |
| `search_path` | server default | Schema for doorstep's tables, if not `public`. |

Postgres 15 or newer is supported. Required privileges and the schema are described in [database.md](database.md).

## `doorstep migrate`

Applies pending database migrations, printing one line per applied migration (or `no pending migrations`), then exits. Use it to migrate from a separate job before rolling out a new version; `serve` applies the same migrations on start, so running it is optional. Migrations are forward-only: there are no down, redo or create commands.

`doorstep migrate status` prints every migration the binary knows, whether it is applied or pending, and when it was applied:

```
VERSION  STATE    APPLIED AT            NAME
1        applied  2026-11-23T10:00:00Z  00001_initial_schema.sql
2        applied  2026-11-23T10:00:00Z  00002_queue_notify.sql
3        pending  -                     00003_lock_scope_and_events.sql
```

Both accept `--config`, `--database-url-file`, `--log-level` and `--log-format` and the matching environment variables. From the configuration file they use `log` and `database`; other settings and apps are ignored. Migration progress is logged to stderr. Concurrent runs (several replicas starting at once) are serialised by a Postgres advisory lock keyed by the schema, so instances that share a schema wait for each other while instances in other schemas of the same database do not. If the database has a newer schema than the binary, `migrate` and `serve` fail rather than run against it.

## GitHub App

GitHub access is optional as a group: with none of these set, `doorstep serve` runs without GitHub. Once the App ID or a private key is set, the App ID and exactly one private key source are required. These settings come from environment variables or the configuration file; there are no flags, so the key never shows up in process listings, and the file can only name a key file, never hold the key itself. See [github-app.md](github-app.md) for creating, installing and rotating the App.

| Environment variable | File key | Default | Description |
| --- | --- | --- | --- |
| `DOORSTEP_GITHUB_APP_ID` | `github.app_id` | unset | The App's numeric App ID or its Client ID, used as the JWT issuer. Must not contain whitespace. |
| `DOORSTEP_GITHUB_APP_PRIVATE_KEY_FILE` | `github.private_key_file` | unset | Path to the App's private key: PEM, PKCS #1 (`RSA PRIVATE KEY`, as GitHub downloads it) or PKCS #8 (`PRIVATE KEY`), RSA, at least 2048 bits. Usually a mounted Kubernetes Secret. |
| `DOORSTEP_GITHUB_APP_PRIVATE_KEY` | none | unset | The private key inline, same formats. Literal `\n` sequences are turned into newlines, for env files that cannot hold multi-line values. Setting both this and `DOORSTEP_GITHUB_APP_PRIVATE_KEY_FILE` is an error. |
| `DOORSTEP_GITHUB_API_URL` | `github.api_url` | `https://api.github.com` | REST API root. For GitHub Enterprise Server use `https://<host>/api/v3`. Must be an absolute URL without user information, query or fragment, and must use `https` unless the host is loopback (`localhost`, `127.0.0.1`, `::1`). A trailing slash is ignored. Validated even when no App is configured. |

The key is read and parsed when `serve` starts; a missing, unreadable or malformed key stops startup. Key values are redacted wherever configuration is printed or logged.

## `doorstep image resolve`

```sh
doorstep image resolve <repository> <tag> [--registry-credentials-file PATH] [--config PATH]
```

Resolves a tag to the digest it currently points at and prints it on stdout, using the same registry client and credentials as the server. Use it to check that doorstep can reach a registry and that its credentials work before wiring up an application:

```sh
$ doorstep image resolve ghcr.io/owner/app 0123456789abcdef0123456789abcdef01234567
sha256:9b2c...
```

- `<repository>` is a repository without a tag or digest: `nginx`, `owner/app` (both Docker Hub), `ghcr.io/owner/app`, `registry.example.com:5000/team/app`. Docker Hub names are normalised the usual way (`nginx` is `index.docker.io/library/nginx`).
- `<tag>` is required and must be a valid OCI tag; nothing ever defaults to `latest`. doorstep images are tagged with the full commit SHA, but any tag works here.
- A multi-architecture image resolves to the digest of its index, not of one platform's manifest.
- `--registry-credentials-file` (env `DOORSTEP_REGISTRY_CREDENTIALS_FILE`, or `registry.credentials_file` in the file given by `--config` / `DOORSTEP_CONFIG`) selects the credentials file; without any of them the request is anonymous.

On failure it exits with status 1 and prints the reason: `image not found` (the tag or repository does not exist yet), `registry denied access` (missing or wrong credentials; GHCR and Docker Hub also answer this way for a private repository that does not exist), `invalid image reference` (rejected locally or by the registry), or another error such as a transport failure or a host that does not answer the registry API.

## Registry credentials

doorstep resolves image tags to digests through the OCI distribution API. Registries that allow anonymous pulls need no configuration. For private images, point `DOORSTEP_REGISTRY_CREDENTIALS_FILE` (or `--registry-credentials-file`, or `registry.credentials_file` in the configuration file) at a Docker `config.json` file:

```json
{
  "auths": {
    "ghcr.io": { "auth": "cm9ib3Q6Z2hwX2V4YW1wbGU=" },
    "ghcr.io/other-owner": { "username": "other-bot", "password": "ghp_other" },
    "https://index.docker.io/v1/": { "username": "me", "password": "dckr_pat_example" },
    "registry.example.com:5000": { "registrytoken": "a-bearer-token" }
  }
}
```

Each entry under `auths` provides one of:

- `auth`: base64 of `username:password` (the password may contain colons);
- `username` and `password`;
- `identitytoken`: an OAuth2 refresh token, as written by some cloud registry logins;
- `registrytoken`: a bearer token sent to the registry as is.

`auth` wins over `username`/`password` when both are present. Other fields (`email`, `credsStore`, `credHelpers`, `HttpHeaders`, ...) are ignored, so a file written by `docker login` works as long as the credentials are inline. Credential helpers and credential stores are never executed. An entry with no usable credential, invalid base64, an `auth` value with an empty username or password, or two keys that name the same registry make the whole file invalid.

### Kubernetes

The file has the same shape as the `.dockerconfigjson` key of a `kubernetes.io/dockerconfigjson` Secret, so the Secret you already use as an `imagePullSecret` works. The kustomize [registry-credentials component](install.md#registry-credentials) wires this up for you; the equivalent manual configuration is:

```sh
kubectl create secret docker-registry doorstep-registry \
  --docker-server=ghcr.io --docker-username=robot --docker-password="$GHCR_TOKEN"
```

```yaml kubernetes
containers:
  - name: doorstep
    env:
      - name: DOORSTEP_REGISTRY_CREDENTIALS_FILE
        value: /etc/doorstep/registry/.dockerconfigjson
    volumeMounts:
      - name: registry-credentials
        mountPath: /etc/doorstep/registry
        readOnly: true
volumes:
  - name: registry-credentials
    secret:
      secretName: doorstep-registry
```

The file is re-read on every resolve, so rotating the Secret takes effect without restarting doorstep. `serve` also reads it once at startup and logs the number of entries (never their contents).

### Matching

Keys are normalised before matching: the scheme and trailing slashes are removed, a legacy `/v1/` or `/v2/` path is removed when it is the whole path after the host (`https://index.docker.io/v1/` is `index.docker.io`, but `ghcr.io/acme/v2` stays a prefix for `ghcr.io/acme/v2/...` only), and the host is lowercased. `docker.io`, `index.docker.io` and `registry-1.docker.io` are the same registry.

A key is a registry host with an optional repository path prefix. For an image `host/repository`, doorstep uses the entry with the longest key that matches whole path segments: with the example above, `ghcr.io/other-owner/app` uses the `ghcr.io/other-owner` entry and `ghcr.io/owner/app` uses `ghcr.io`, while `ghcr.io/other-owner-2/app` does not match `ghcr.io/other-owner`. This is how to use a different token per GitHub owner on GHCR. Wildcards are not supported. When no key matches, the request is anonymous, and credentials are never sent to a host they were not configured for.

For GHCR, use a token with the `read:packages` scope (a classic personal access token or a GitHub App installation token with package read access).

### Transport

Registries are reached over HTTPS. Plain HTTP is only attempted, as a fallback alongside HTTPS, for `127.0.0.1`, `::1`, `localhost:<port>`, `*.localhost` and IP addresses in RFC 1918 ranges (`10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16`), which covers an in-cluster registry addressed by IP. Every other host, including mDNS `.local` names and cluster DNS names, is HTTPS only, and there is no switch to allow plain HTTP elsewhere. A registry with a certificate from a private CA works once the CA is in the system trust store, for example by mounting the bundle and setting `SSL_CERT_FILE` (or `SSL_CERT_DIR`). Proxies are honoured through `HTTPS_PROXY`, `HTTP_PROXY` and `NO_PROXY`.

The plain HTTP fallback also carries credentials. When a credential matches one of those local or private hosts, the `Authorization` header (basic credentials, or the bearer token after a token exchange) is sent over HTTP whenever HTTPS fails, and anyone who can block port 443 on the path can force that and read the secret. Only configure credentials for such a registry when the network path to it is trusted, or serve it over HTTPS and address it by a host name outside the list above (such as a cluster DNS name), which is never contacted over plain HTTP.

## `doorstep lock`, `unlock` and `locks`

```sh
doorstep lock blog --reason "database migration" --for 2h
doorstep lock --global --reason "holiday freeze" --until 2027-01-02T09:00:00Z
doorstep locks
doorstep unlock blog
doorstep unlock --global
```

Thin clients of the [lock API](locks.md#api) on a running server. `lock` freezes deploys of one app, or of every app with `--global`; `--reason` is required, and `--for` (Go duration) or `--until` (RFC 3339 time) sets an optional expiry. `unlock` releases a freeze. `locks` lists the live freezes as a table, or as JSON with `--output json` (`-o json`). The semantics are described in [locks.md](locks.md).

| Flag | Environment variable | Default | Description |
| --- | --- | --- | --- |
| `--server` | `DOORSTEP_SERVER_URL` | `http://127.0.0.1:8080` | Base URL of the doorstep server (the main listener, see [api.md](api.md)), `http` or `https`, optionally with a path prefix. Must not contain user information, a query or a fragment. The token is sent with every request, so with a token set the client refuses plain `http` to anything but a loopback address (`localhost`, `127.0.0.0/8`, `::1`); use `https` whenever the connection leaves the host. |
| | `DOORSTEP_TOKEN` | none | API token, sent as `Authorization: Bearer <token>`. There is no flag, so the token never shows up in process listings or shell history. |
| | `DOORSTEP_TOKEN_FILE` | none | File holding the API token, such as a mounted Secret; surrounding whitespace is trimmed, and the file may be at most 64 KiB. Setting both token variables is an error. |

The lock owner is the identity the server derives from the token, never a command-line value. Requests time out after 30 seconds, redirects are not followed (so the token is never forwarded to another host), and responses larger than 1 MiB are rejected. When the server refuses a request, the command prints the server's error message, for example `doorstep: blog is already locked by alice: database migration`, and exits 1.

## `doorstep version`

Prints the release version, VCS commit, commit time and Go version. `--json` prints the same as a JSON object. The release version is set at build time (`DOORSTEP_VERSION=v1.2.3 just build`) and is `dev` otherwise.

## Configuration file

`doorstep serve --config /etc/doorstep/config.yaml` (or `DOORSTEP_CONFIG=/etc/doorstep/config.yaml`) reads one YAML file that holds runtime settings, sign-in settings, notification targets and apps. `doorstep migrate`, `doorstep token` and `doorstep image resolve` accept the same flag and variable and read the runtime settings they need from it. The file is read once at startup; to change it, update the file (or ConfigMap) and restart doorstep. On Kubernetes, the [kustomize base](install.md#configuration) generates the ConfigMap with a content hash in its name, so changing the file rolls the Deployment automatically.

Parsing is strict:

- unknown keys are errors, reported with their line number, so a typo never silently falls back to a default;
- the file holds exactly one YAML document; a second document after `---` is an error;
- the file may be at most 1 MiB, the Kubernetes ConfigMap limit;
- an empty file is a valid configuration with no apps;
- durations use Go syntax with a unit (`30s`, `10m`, `1h30m`); bare numbers are rejected;
- keys are `snake_case`.

After parsing, defaults are applied and the whole file is validated. Every problem is reported at once, one per line, each prefixed with its location:

```text
doorstep: invalid configuration: apps[1] (blog): gitops.path: must be relative to the repository root
apps[1] (blog): notify[0]: unknown notification "pager"
```

The smallest useful file has one app with three settings:

```yaml
apps:
  - name: blog
    source:
      repo: example-user/blog
    image:
      repository: ghcr.io/example-user/blog
    gitops:
      repo: example-user/homelab
      path: apps/blog
```

This deploys every commit merged to `main` of `example-user/blog` once its checks pass: doorstep resolves `ghcr.io/example-user/blog:<full commit SHA>` to a digest, sets it on the `images` entry named `ghcr.io/example-user/blog` in the kustomization in `apps/blog` on `main` of `example-user/homelab`, and waits until the Argo CD Application `argocd/blog` is synced to that commit and healthy.

### Runtime settings

Each key overrides the built-in default and is itself overridden by the environment variable and flag of the same setting (see [Precedence](#precedence)). Values and validation are the same as for the flag or environment variable.

| Key | Flag | Environment variable |
| --- | --- | --- |
| `server.listen_addr` | `--listen-addr` | `DOORSTEP_LISTEN_ADDR` |
| `server.metrics_listen_addr` | `--metrics-listen-addr` | `DOORSTEP_METRICS_LISTEN_ADDR` |
| `server.shutdown_delay` | `--shutdown-delay` | `DOORSTEP_SHUTDOWN_DELAY` |
| `server.shutdown_timeout` | `--shutdown-timeout` | `DOORSTEP_SHUTDOWN_TIMEOUT` |
| `server.public_url` | `--public-url` | `DOORSTEP_PUBLIC_URL` |
| `log.level` | `--log-level` | `DOORSTEP_LOG_LEVEL` |
| `log.format` | `--log-format` | `DOORSTEP_LOG_FORMAT` |
| `database.url_file` | `--database-url-file` | `DOORSTEP_DATABASE_URL_FILE` or `DOORSTEP_DATABASE_URL` |
| `github.app_id` | none | `DOORSTEP_GITHUB_APP_ID` |
| `github.private_key_file` | none | `DOORSTEP_GITHUB_APP_PRIVATE_KEY_FILE` or `DOORSTEP_GITHUB_APP_PRIVATE_KEY` |
| `github.api_url` | none | `DOORSTEP_GITHUB_API_URL` |
| `registry.credentials_file` | `--registry-credentials-file` | `DOORSTEP_REGISTRY_CREDENTIALS_FILE` |
| `locks.reminder_interval` | none | `DOORSTEP_LOCK_REMINDER_INTERVAL` |

A key that is present is applied even when its value is zero, so `shutdown_delay: 0s` disables the delay and `metrics_listen_addr: ""` serves `/metrics` on the main listener. The database URL and the GitHub App private key can only be referenced by file path; there are no keys for the values themselves (see [Secrets](#secrets)).

### `auth`

Sign-in with one identity provider and how its users map to roles. Without `oidc` or `github`, only API tokens authenticate, the `/auth/*` routes are not mounted, and `session_ttl`, `default_role` and `roles` are rejected. Configuring a provider also requires [`server.public_url`](#doorstep-serve). [authentication.md](authentication.md) explains the roles, the sign-in flow and how to register doorstep with each provider.

| Key | Default | Description |
| --- | --- | --- |
| `session_ttl` | `12h` | Absolute lifetime of a session from sign-in, between `5m` and `720h`. Sessions are not extended by activity. Roles are decided at sign-in, so this also bounds how long a role change takes to apply. |
| `default_role` | none | Role granted to a user who matches no entry in `roles`: `viewer`, `deployer` or `admin`. Empty refuses sign-in to such users. |
| `roles.<role>.groups` | none | Groups that grant `<role>` (`viewer`, `deployer` or `admin`). OIDC: values of the groups claim, compared exactly. GitHub: an organization login (`example-org`) or `org/team-slug` (`example-org/platform`), compared case-insensitively. |
| `roles.<role>.users` | none | Users that grant `<role>`. OIDC: the `sub` claim, or an email address that the provider marks verified. GitHub: `id:` followed by the numeric user ID (`id:583231`); logins are rejected because a renamed login can be registered by someone else. |
| `oidc.issuer_url` | required | Issuer URL; its `/.well-known/openid-configuration` must be reachable. `https`, or `http` only on a loopback host, without user information, query or fragment. |
| `oidc.client_id` | required | Client ID registered at the provider; no whitespace. |
| `oidc.client_secret_env` / `oidc.client_secret_file` | exactly one | Environment variable or absolute file path holding the client secret; see [Secrets](#secrets). |
| `oidc.scopes` | `[openid, profile, email, groups]` | Scopes requested; must include `openid`, without duplicates. |
| `oidc.groups_claim` | `groups` | Claim holding the user's groups, read from the ID token or else from UserInfo. |
| `oidc.username_claim` | `preferred_username` | Claim shown as the user's name in logs, `doorstep session list` and the audit trail, falling back to `sub`. It is a label only: the actor ID is always `oidc:<sub>`, and role mapping never uses it. |
| `github.client_id` | required | Client ID of a GitHub OAuth App, or of the GitHub App's own client credentials. |
| `github.client_secret_env` / `github.client_secret_file` | exactly one | Environment variable or absolute file path holding the client secret. |

Each role under `roles` needs at least one group or user, and entries must be unique and non-empty. On sign-in the highest role with a matching entry wins (`admin`, then `deployer`, then `viewer`). GitHub sign-in uses `github.api_url` for the API and derives the OAuth URLs from it, so GitHub Enterprise Server needs no extra setting.

```yaml
server:
  public_url: https://deploy.example.com
auth:
  session_ttl: 8h
  oidc:
    issuer_url: https://auth.example.com/application/o/doorstep/
    client_id: doorstep
    client_secret_file: /etc/doorstep/secrets/oidc-client-secret
    scopes: [openid, profile, email]
  roles:
    admin:
      groups: [homelab-admins]
    deployer:
      groups: [developers]
      users: [alice@example.com]
    viewer:
      groups: [family]

apps:
  - name: blog
    source:
      repo: example-user/blog
    image:
      repository: ghcr.io/example-user/blog
    gitops:
      repo: example-user/homelab
      path: apps/blog
```

### `notifications[]`

Named notification targets. Apps refer to them by name in `notify`.

| Key | Required | Description |
| --- | --- | --- |
| `name` | yes | Unique name; a DNS label (lowercase letters, digits and `-`, starting and ending with a letter or digit, at most 63 characters). |
| `type` | yes | `slack` or `webhook`. |
| `webhook_url_env` / `webhook_url_file` | `slack`: exactly one | Environment variable or absolute file path holding the Slack incoming webhook URL. Slack webhook URLs are credentials, so there is no inline form. |
| `url` / `url_env` / `url_file` | `webhook`: exactly one | The endpoint doorstep posts to: inline (`http` or `https`, with a host, and without `user:password@`, a query string or a fragment, since those often carry tokens), or from an environment variable or absolute file path when the URL itself carries a token. |
| `secret_env` / `secret_file` | `webhook`: optional, at most one | Secret used to sign webhook payloads (`X-Doorstep-Signature-256`, HMAC-SHA256 of the body). |
| `events` | no | Events this target receives, by name: `deploy.queued`, `deploy.started`, `deploy.succeeded`, `deploy.failed`, `deploy.rolled_back`, `deploy.cancelled`, `deploy.superseded`, `lock.changed`, `lock.reminder`. Leave it out for the defaults (`deploy.started`, `deploy.succeeded`, `deploy.failed`, `deploy.rolled_back`, `lock.changed`, `lock.reminder`). An empty list, an unknown event or a repeated event is an error. |

Keys that belong to the other type are rejected. Environment variable names must be identifiers (`[A-Za-z_][A-Za-z0-9_]*`) and file paths must be absolute. See [Secrets](#secrets) for how they are read, [notifications.md](notifications.md) for message contents, the webhook payload and delivery, and [`doorstep notify test`](#doorstep-notify-test) to try a target.

### `apps[]`

One app is one deploy target: one image, one place in a GitOps repository and one Argo CD Application. Model environments as separate apps (`blog-staging`, `blog-prod`).

| Key | Default | Description |
| --- | --- | --- |
| `name` | required | Unique name; a DNS label. It appears in URLs, commit status contexts and metric labels. |
| `source.repo` | required | GitHub repository that builds the image, as `owner/name`. Owners can be organizations or personal accounts, and apps from several owners can share one file. |
| `source.branch` | `main` | Merges to this branch are deployed. |
| `source.deployments` | `true` | Report each deployment as a GitHub deployment with statuses on `source.repo` (environment = the app name) when the GitHub App is configured. Set `false` to skip, for example when the repository has `on: deployment` workflows that should not run. See [notifications.md](notifications.md#github-deployments). |
| `source.required_checks` | all checks | Names of the check runs and commit statuses that must pass on the commit before it deploys. Empty means every check reported on the commit must pass. Entries must be unique. See [Required checks](#required-checks). |
| `image.repository` | required | Fully qualified image repository without tag or digest, such as `ghcr.io/example-user/blog`. The registry host must contain a `.` or a `:port`, or be `localhost`; there is no implicit Docker Hub. The path must be lowercase. Images are expected to be tagged with the full commit SHA. |
| `gitops.repo` | required | GitHub repository Argo CD syncs from, as `owner/name`. |
| `gitops.branch` | `main` | Branch doorstep commits pins to, or opens pull requests against. |
| `gitops.path` | repository root | Directory of the app inside the GitOps repository; relative, without `..`. |
| `pin` | `kustomize` | How the digest is written; see [Pin strategies](#pin-strategies). |
| `deploy.mode` | `commit` | `commit` pushes the pin straight to `gitops.branch`; `pull_request` opens a pull request with the pin instead, which doorstep squash-merges itself once the GitOps repository's required checks pass. See [gitops.md](gitops.md#deploy-modes). |
| `deploy.max_batch` | `10` | Most queued commits collapsed into one deploy, 1 to 100. When several merges are waiting, the newest of the first `max_batch` is deployed and the older ones are superseded; `1` deploys every commit. An urgent deploy still supersedes older queued deploys of older commits regardless of this limit. See [queue.md](queue.md#batching). |
| `argocd.application` | the app name | Name of the Argo CD `Application` to watch; a DNS subdomain. Several apps may share one Application, for example when it runs several images. |
| `argocd.namespace` | `argocd` | Namespace of that Application; a DNS label. |
| `gates.before` | `checks` | Gates evaluated before the pin is written; see [Gates](#gates). |
| `gates.after` | `health` | Gates evaluated after Argo CD has synced the pin. |
| `notify` | none | Names of `notifications[]` targets to notify about this app. Each must exist and appear once. |

#### Required checks

When a commit is pushed to `source.branch`, doorstep tracks it for every app that follows that repository and branch and decides from its CI results whether it may deploy. Each app is evaluated against its own `required_checks`, so `blog-staging` and `blog-prod` can share a repository with different requirements.

- Names match check run names and commit status contexts exactly and case-sensitively.
- A check run that has not completed is pending. Conclusions `success`, `neutral` and `skipped` pass, as in GitHub branch protection; every other conclusion (`failure`, `cancelled`, `timed_out`, `action_required`, `stale`, or anything unknown) fails.
- A commit status `success` passes and `pending` is pending; `failure`, `error` and anything unknown fail.
- When a check is re-run, the newest run of that name counts. When a check run and a commit status share a name, the worse result counts.
- With `required_checks` listed, only those checks count. Each must be reported and pass; one that has not been reported yet is pending, and any failure fails the commit even while others are pending.
- With `required_checks` empty, every reported check run and commit status must pass, at least one must be reported, and every check suite that has created check runs must have completed. The last rule covers GitHub Actions jobs that wait on `needs:` and are not created until their dependencies finish. A repository with no CI at all therefore never deploys automatically in this mode. Listing the checks explicitly is recommended: it is faster to evaluate and does not depend on other Apps' check suites.
- Names are the only thing matched; the reporter is not. Any GitHub App or user that can write check runs or commit statuses on the repository can therefore satisfy a required check by reporting a passing result under its name, for example a `build` status posted before the real `build` job has started. Once the real check reports, the worse result counts, so a failing real check still blocks. Give `statuses: write` and `checks: write` on application repositories only to integrations you trust to gate production, and prefer check names that are unlikely to collide with other integrations.
- A failed commit is re-evaluated when its checks change and on every poll, so re-running a failed job can still deploy it.
- Only the commit at the head of `source.branch` deploys. Before recording a push, doorstep asks GitHub where the branch points now and ignores the push if it has moved on, so a late or redelivered webhook for an older push never replaces a newer one. Recording the head supersedes every older commit that has not been queued yet, even if their checks later pass; if the head is red, nothing deploys until a green commit follows (or someone deploys manually). A force-push back to an earlier commit makes it the head again, and it deploys once its checks pass.
- Commits tracked for an app stop being evaluated when the app is removed or its `source.repo` or `source.branch` changes.

There is no timeout while a commit waits for its checks; the `checks` gate's `timeout` bounds the wait once a deployment is underway.

#### Pin strategies

`pin` holds exactly one of `kustomize` or `yaml`. Without a `pin` block the app uses `kustomize` with `image` set to `image.repository`.

| Key | Default | Description |
| --- | --- | --- |
| `pin.kustomize.image` | `image.repository` | The `name` of the entry in the `images` list of the kustomization in `gitops.path` (`kustomization.yaml`, `kustomization.yml` or `Kustomization`). doorstep sets its `digest`. No tag or digest. |
| `pin.yaml.file` | required | YAML file relative to `gitops.path`, ending in `.yaml` or `.yml`, without `..`. |
| `pin.yaml.path` | required | Location of the value to set; see below. |
| `pin.yaml.format` | `digest` | `digest` writes `sha256:...`, for Helm values with a separate digest field; `reference` writes `repository@sha256:...`, for an `image:` field in a plain manifest. |

`pin.yaml.path` is a list of keys separated by `.`; segments must not be empty or start or end with whitespace, and a segment made only of digits is an index into a sequence. `image.digest` addresses `digest` under `image`, and `spec.template.spec.containers.0.image` addresses the image of the first container. Keys that contain a `.` cannot be addressed.

The place being pinned must already exist in the GitOps repository: the `images` entry with that `name` (doorstep adds its `digest` key if missing, and leaves `newName` and `newTag` alone), or a string value at the YAML path (a placeholder such as `digest: ""` is enough). doorstep edits only that value and keeps the rest of the file byte for byte. [gitops.md](gitops.md) shows the edits, the commit trailers and the YAML forms doorstep refuses to edit.

#### Gates

`gates.before` and `gates.after` are lists of gates. Leaving a list out uses its default; an explicit empty list (`before: []`) runs no gates in that phase. Every gate has a `name`, which defaults to its type and must be unique within the app, so two gates of the same type need explicit names.

| `type` | Phase | Settings | Passes when |
| --- | --- | --- | --- |
| `checks` | `before` | `timeout`, default `30m` | The checks in `source.required_checks` (or all checks) succeeded on the commit. |
| `health` | `after` | `timeout`, default `10m` | The Argo CD Application is synced to the pin commit and healthy. |
| `soak` | `after` | `duration`, required | The Application stays healthy for `duration`. |

A gate's `timeout` is measured from its first evaluation for the deployment; a gate still pending when it runs out fails, which fails the phase (see [gates.md](gates.md#timeouts)).

A gate type is rejected in the other phase, and settings that do not belong to a type (`timeout` on `soak`, `duration` on `checks` or `health`) are errors, even when zero. Timeouts and durations must be positive: leave `timeout` out to use the default, because an explicit `timeout: 0s` is an error rather than a way to ask for the default. Deploys of one app are always serialised by a lock; that is not configurable. A `metrics` gate that holds a deploy on a Prometheus query is planned (see [ADR 0001](decisions/0001-go-and-core-design.md)) and is not accepted yet.

#### Rules across apps

- App names and notification names are unique.
- Two apps must not write the same pin: the same GitOps repository (compared case-insensitively), branch and file, and the same kustomize image name or YAML path. They would overwrite each other's digests.
- A `yaml` pin must not target a `kustomization.yaml` or `kustomization.yml` that a `kustomize` pin of another app writes (same GitOps repository, branch and directory); pin that file with `kustomize` instead.
- Apps may share a source repository, a GitOps repository and an Argo CD Application.

### Full example

Two owners, both pin strategies, both deploy modes, a soak gate and both notification types:

```yaml
server:
  shutdown_delay: 10s
  metrics_listen_addr: :9090
  public_url: https://doorstep.example.com
log:
  format: json
database:
  url_file: /etc/doorstep/secrets/database-url
github:
  app_id: "123456"
  private_key_file: /etc/doorstep/secrets/github-app-private-key.pem
registry:
  credentials_file: /etc/doorstep/registry/.dockerconfigjson
locks:
  reminder_interval: 12h

notifications:
  - name: ops
    type: slack
    webhook_url_file: /etc/doorstep/secrets/slack-webhook-url
  - name: hooks
    type: webhook
    url: https://hooks.example.com/doorstep
    secret_env: DOORSTEP_HOOKS_SECRET
    events: [deploy.succeeded, deploy.failed, deploy.rolled_back]

apps:
  - name: blog
    source:
      repo: example-user/blog
    image:
      repository: ghcr.io/example-user/blog
    gitops:
      repo: example-user/homelab
      path: apps/blog
    notify: [ops]

  - name: api-staging
    source:
      repo: example-org/api
      required_checks: [test, lint]
    image:
      repository: ghcr.io/example-org/api
    gitops:
      repo: example-org/platform
      path: envs/staging/api
    pin:
      kustomize:
        image: api
    gates:
      after:
        - type: health
          timeout: 5m
    notify: [ops, hooks]

  - name: api-prod
    source:
      repo: example-org/api
      branch: release
      required_checks: [test, lint, e2e]
      deployments: false
    image:
      repository: ghcr.io/example-org/api
    gitops:
      repo: example-org/platform
      path: charts/api
    pin:
      yaml:
        file: values-prod.yaml
        path: image.digest
        format: digest
    deploy:
      mode: pull_request
      max_batch: 5
    argocd:
      application: api
      namespace: argo-cd
    gates:
      before:
        - type: checks
          timeout: 1h
      after:
        - type: health
        - type: soak
          duration: 30m
    notify: [ops, hooks]
```

### Kubernetes

Keep the file as a ConfigMap in the GitOps repository, mount it, point `DOORSTEP_CONFIG` at it and mount secrets from a Secret:

```yaml kubernetes
apiVersion: v1
kind: ConfigMap
metadata:
  name: doorstep
data:
  config.yaml: |
    apps:
      - name: blog
        source:
          repo: example-user/blog
        image:
          repository: ghcr.io/example-user/blog
        gitops:
          repo: example-user/homelab
          path: apps/blog
```

```yaml kubernetes
# excerpt of the doorstep Deployment's pod spec
containers:
  - name: doorstep
    env:
      - name: DOORSTEP_CONFIG
        value: /etc/doorstep/config.yaml
    volumeMounts:
      - name: config
        mountPath: /etc/doorstep/config.yaml
        subPath: config.yaml
        readOnly: true
      - name: secrets
        mountPath: /etc/doorstep/secrets
        readOnly: true
volumes:
  - name: config
    configMap:
      name: doorstep
  - name: secrets
    secret:
      secretName: doorstep
```

Restart the Deployment after changing the ConfigMap.

## `doorstep token`

```sh
doorstep token create --name NAME --role viewer|deployer|admin [--expires-in DURATION]
doorstep token list
doorstep token revoke NAME
```

Creates, lists and revokes API tokens by talking to the database directly. `create` prints the token once on stdout, and its details on stderr. The commands take the same flags, environment variables and configuration file settings as [`doorstep migrate`](#doorstep-migrate), and fail without changing anything when the database schema is behind; run `doorstep migrate` first. `--actor NAME` (or `DOORSTEP_ACTOR`) names the person running the command; it is recorded as `cli:NAME` in the audit log, with the operating system user kept in the row's details. Without it the actor is `cli:<operating system user>`. See [authentication.md](authentication.md#api-tokens).

## `doorstep session`

```sh
doorstep session list
doorstep session revoke --user USER
doorstep session revoke --all
```

Lists active sign-in sessions and ends them immediately by talking to the database directly. `--user` matches a session's username case-insensitively, its subject, or its actor ID such as `github:583231`. Each `revoke` writes a `session.revoke` audit row. The commands take the same flags as `doorstep token`, including `--actor`. See [authentication.md](authentication.md#ending-sessions).

## `doorstep config validate`

```sh
doorstep config validate --config config.yaml
```

Parses the configuration file and validates the values in it with the same rules as `serve`, without starting a server, so it can run in the GitOps repository's CI before a configuration change merges. It does not read secrets, and it does not apply or check `DOORSTEP_*` environment overrides or flags (so a file with a sign-in provider must set `server.public_url` itself to pass), so `serve` can still reject an invalid environment variable that `config validate` never saw. It does not require a database URL source either, since that usually comes from the environment. The path comes from `--config` or `DOORSTEP_CONFIG`; one of them is required. On success it prints `ok: N apps, M notification targets` and exits 0; otherwise it prints every error and exits 1.

## `doorstep notify test`

```sh
doorstep notify test <notification-name> --config config.yaml
```

Sends one synthetic `deploy.succeeded` notification to the named `notifications[]` target, with a single attempt, and prints `ok: sent deploy.succeeded to <name> (<type>)`. It is sent even when the target's `events` leave `deploy.succeeded` out. It uses the same secrets (`*_env` variables and `*_file` paths), `DOORSTEP_PUBLIC_URL` and HTTP client as `serve`, so it proves the target works before a real deploy depends on it. On failure it prints the reason without the URL, such as `unexpected response 404 Not Found: no_service`, and exits 1. The path comes from `--config` or `DOORSTEP_CONFIG`. See [notifications.md](notifications.md#testing-a-target).

## Secrets

Secrets (GitHub App private key, webhook secret, database password, OIDC client secret, registry credentials, notification tokens) are never written inline in the configuration file.

Notification secrets and the sign-in client secret (`auth.oidc.client_secret_*` or `auth.github.client_secret_*`) are referenced by a pair of settings: `*_env` names an environment variable that holds the value, and `*_file` names an absolute path to a file that holds it, which maps directly to a mounted Kubernetes Secret. At most one of the pair is set, and exactly one where the setting is required. One trailing newline is removed from the value, and an empty value is an error. A secret file may be at most 64 KiB; a larger file (or a wrong path such as `/dev/zero`) is an error rather than being read into memory.

`doorstep serve` checks every referenced notification and client secret at startup by reading it, and refuses to start if any is unset, unreadable, too large or empty, reporting all of them together. The values read by that check are discarded. Errors name the setting and the variable or path, never the value. Secrets are read again each time they are used, so a rotated mounted Secret takes effect without a restart. `doorstep config validate` only checks that references are well formed, because secrets are usually not available where the file is validated.

The database URL, the GitHub App private key and registry credentials are referenced only by path in the file (`database.url_file`, `github.private_key_file`, `registry.credentials_file`); to pass the value itself, use the environment variable described in [Database](#database) or [GitHub App](#github-app). Those files are read as described in their sections.

Settings for the remaining secrets arrive with the features that need them.

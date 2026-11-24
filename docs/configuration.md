# Configuration

## Precedence

Each setting is resolved from the first source that provides it:

1. command-line flags
2. environment variables (prefix `DOORSTEP_`)
3. YAML configuration file (`--config` / `DOORSTEP_CONFIG`) (planned, not yet implemented)
4. built-in defaults

An environment variable that is set but empty counts as set, and is then validated like any other value. All settings are validated together on startup, and every invalid setting is reported in one error.

## `doorstep serve`

| Flag | Environment variable | Default | Description |
| --- | --- | --- | --- |
| `--listen-addr` | `DOORSTEP_LISTEN_ADDR` | `:8080` | Address for the HTTP listener as `host:port`. The port must be numeric; `0` picks a free port. |
| `--metrics-listen-addr` | `DOORSTEP_METRICS_LISTEN_ADDR` | `:9090` | Address for the `/metrics` listener as `host:port`; `/metrics` is served only there. Must differ from `--listen-addr`. Set it to an empty value to serve `/metrics` on the main listener instead, which is only safe when the main listener is not reachable from the internet. See [metrics.md](metrics.md). |
| `--shutdown-delay` | `DOORSTEP_SHUTDOWN_DELAY` | `5s` | How long the server keeps serving after `SIGINT`/`SIGTERM` while `/readyz` reports `shutting_down`, before it stops accepting connections. Gives load balancers time to stop routing to the instance. Go duration syntax. `0s` disables it; must not be negative. |
| `--shutdown-timeout` | `DOORSTEP_SHUTDOWN_TIMEOUT` | `15s` | How long in-flight requests may run once the shutdown delay has passed, before the server is closed. Go duration syntax (`30s`, `1m`). Must be positive. |
| `--log-level` | `DOORSTEP_LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error` (case-insensitive). Health probe and `/metrics` requests are logged at `debug`. |
| `--log-format` | `DOORSTEP_LOG_FORMAT` | `json` | `json` or `text`. Logs are written to stderr. |
| | `DOORSTEP_DATABASE_URL` | none | Postgres connection URL. Required unless a URL file is given; see [Database](#database). |
| `--database-url-file` | `DOORSTEP_DATABASE_URL_FILE` | none | Path to a file holding the Postgres connection URL. Required unless `DOORSTEP_DATABASE_URL` is set; see [Database](#database). |
| `--registry-credentials-file` | `DOORSTEP_REGISTRY_CREDENTIALS_FILE` | empty (anonymous) | Path to a Docker `config.json` with registry credentials; see [Registry credentials](#registry-credentials). When set, `serve` reads it at startup and refuses to start if it is unreadable or malformed. |

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
```

Both accept `--database-url-file`, `--log-level` and `--log-format` and the matching environment variables. Migration progress is logged to stderr. Concurrent runs (several replicas starting at once) are serialised by a Postgres advisory lock. If the database has a newer schema than the binary, `migrate` and `serve` fail rather than run against it.

## GitHub App

GitHub access is optional as a group: with none of these set, `doorstep serve` runs without GitHub. Once the App ID or a private key is set, the App ID and exactly one private key source are required. These settings are environment variables only; there are no flags, so the key never shows up in process listings. See [github-app.md](github-app.md) for creating, installing and rotating the App.

| Environment variable | Default | Description |
| --- | --- | --- |
| `DOORSTEP_GITHUB_APP_ID` | unset | The App's numeric App ID or its Client ID, used as the JWT issuer. Must not contain whitespace. |
| `DOORSTEP_GITHUB_APP_PRIVATE_KEY_FILE` | unset | Path to the App's private key: PEM, PKCS #1 (`RSA PRIVATE KEY`, as GitHub downloads it) or PKCS #8 (`PRIVATE KEY`), RSA, at least 2048 bits. Usually a mounted Kubernetes Secret. |
| `DOORSTEP_GITHUB_APP_PRIVATE_KEY` | unset | The private key inline, same formats. Literal `\n` sequences are turned into newlines, for env files that cannot hold multi-line values. Setting both this and `DOORSTEP_GITHUB_APP_PRIVATE_KEY_FILE` is an error. |
| `DOORSTEP_GITHUB_API_URL` | `https://api.github.com` | REST API root. For GitHub Enterprise Server use `https://<host>/api/v3`. Must be an absolute URL without user information, query or fragment, and must use `https` unless the host is loopback (`localhost`, `127.0.0.1`, `::1`). A trailing slash is ignored. Validated even when no App is configured. |

The key is read and parsed when `serve` starts; a missing, unreadable or malformed key stops startup. Key values are redacted wherever configuration is printed or logged.

## `doorstep image resolve`

```sh
doorstep image resolve <repository> <tag> [--registry-credentials-file PATH]
```

Resolves a tag to the digest it currently points at and prints it on stdout, using the same registry client and credentials as the server. Use it to check that doorstep can reach a registry and that its credentials work before wiring up an application:

```sh
$ doorstep image resolve ghcr.io/owner/app 0123456789abcdef0123456789abcdef01234567
sha256:9b2c...
```

- `<repository>` is a repository without a tag or digest: `nginx`, `owner/app` (both Docker Hub), `ghcr.io/owner/app`, `registry.example.com:5000/team/app`. Docker Hub names are normalised the usual way (`nginx` is `index.docker.io/library/nginx`).
- `<tag>` is required and must be a valid OCI tag; nothing ever defaults to `latest`. doorstep images are tagged with the full commit SHA, but any tag works here.
- A multi-architecture image resolves to the digest of its index, not of one platform's manifest.
- `--registry-credentials-file` (env `DOORSTEP_REGISTRY_CREDENTIALS_FILE`) selects the credentials file; without it the request is anonymous.

On failure it exits with status 1 and prints the reason: `image not found` (the tag or repository does not exist yet), `registry denied access` (missing or wrong credentials; GHCR and Docker Hub also answer this way for a private repository that does not exist), `invalid image reference` (rejected locally or by the registry), or another error such as a transport failure or a host that does not answer the registry API.

## Registry credentials

doorstep resolves image tags to digests through the OCI distribution API. Registries that allow anonymous pulls need no configuration. For private images, point `DOORSTEP_REGISTRY_CREDENTIALS_FILE` (or `--registry-credentials-file`) at a Docker `config.json` file:

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

The file has the same shape as the `.dockerconfigjson` key of a `kubernetes.io/dockerconfigjson` Secret, so the Secret you already use as an `imagePullSecret` works:

```sh
kubectl create secret docker-registry doorstep-registry \
  --docker-server=ghcr.io --docker-username=robot --docker-password="$GHCR_TOKEN"
```

```yaml
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

## `doorstep version`

Prints the release version, VCS commit, commit time and Go version. `--json` prints the same as a JSON object. The release version is set at build time (`DOORSTEP_VERSION=v1.2.3 just build`) and is `dev` otherwise.

## Secrets

Secrets (GitHub App private key, webhook secret, database password, OIDC client secret, registry credentials, notification tokens) are never written inline in the configuration file. Each one is read from an environment variable or from a `*_file` setting that points at a file, which maps directly to a mounted Kubernetes Secret. Settings for those secrets arrive with the features that need them.

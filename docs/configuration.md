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
| `--shutdown-delay` | `DOORSTEP_SHUTDOWN_DELAY` | `5s` | How long the server keeps serving after `SIGINT`/`SIGTERM` while `/readyz` reports `shutting_down`, before it stops accepting connections. Gives load balancers time to stop routing to the instance. Go duration syntax. `0s` disables it; must not be negative. |
| `--shutdown-timeout` | `DOORSTEP_SHUTDOWN_TIMEOUT` | `15s` | How long in-flight requests may run once the shutdown delay has passed, before the server is closed. Go duration syntax (`30s`, `1m`). Must be positive. |
| `--log-level` | `DOORSTEP_LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error` (case-insensitive). Health probe requests are logged at `debug`. |
| `--log-format` | `DOORSTEP_LOG_FORMAT` | `json` | `json` or `text`. Logs are written to stderr. |
| | `DOORSTEP_DATABASE_URL` | none | Postgres connection URL. Required unless a URL file is given; see [Database](#database). |
| `--database-url-file` | `DOORSTEP_DATABASE_URL_FILE` | none | Path to a file holding the Postgres connection URL. Required unless `DOORSTEP_DATABASE_URL` is set; see [Database](#database). |

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

## `doorstep version`

Prints the release version, VCS commit, commit time and Go version. `--json` prints the same as a JSON object. The release version is set at build time (`DOORSTEP_VERSION=v1.2.3 just build`) and is `dev` otherwise.

## Secrets

Secrets (GitHub App private key, webhook secret, database password, OIDC client secret, notification tokens) are never written inline in the configuration file. Each one is read from an environment variable or from a `*_file` setting that points at a file, which maps directly to a mounted Kubernetes Secret. Settings for those secrets arrive with the features that need them.

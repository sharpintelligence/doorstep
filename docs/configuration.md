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

Example:

```sh
DOORSTEP_LOG_FORMAT=text doorstep serve --listen-addr 127.0.0.1:9000 --log-level debug
```

A second `SIGINT`/`SIGTERM` during shutdown exits immediately. Keep the Kubernetes `terminationGracePeriodSeconds` above the shutdown delay plus the shutdown timeout.

## `doorstep version`

Prints the release version, VCS commit, commit time and Go version. `--json` prints the same as a JSON object. The release version is set at build time (`DOORSTEP_VERSION=v1.2.3 just build`) and is `dev` otherwise.

## Secrets

Secrets (GitHub App private key, webhook secret, database password, OIDC client secret, notification tokens) are never written inline in the configuration file. Each one is read from an environment variable or from a `*_file` setting that points at a file, which maps directly to a mounted Kubernetes Secret. Settings for those secrets arrive with the features that need them.

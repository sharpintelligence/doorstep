# Development

## Toolchain

Tool versions are pinned in `mise.toml` (Go, golangci-lint, just) and locked with checksums in `mise.lock`. CI uses the same files, so local and CI runs match.

```sh
mise install
```

`mise.toml` sets `GOTOOLCHAIN=local` so `go` never downloads a different toolchain behind your back. Run commands inside the mise environment (an activated shell, or `mise exec -- <cmd>`).

## Tasks

Recipes live in the `justfile`; `just` with no arguments lists them.

| Recipe | What it does |
| --- | --- |
| `just fmt` | format code with gofumpt and goimports (via `golangci-lint fmt`) |
| `just fmt-check` | show formatting differences without changing files |
| `just vet` | `go vet ./...` |
| `just lint` | `golangci-lint run` with `.golangci.yml` |
| `just tidy-check` | fail if `go.mod`/`go.sum` are not tidy (`go mod tidy -diff`) |
| `just test [args]` | `go test -race -shuffle=on -count=1 [args] ./...` |
| `just build` | static, trimmed binary at `bin/doorstep`; set `DOORSTEP_VERSION` to stamp a release version |
| `just run [args]` | `go run ./cmd/doorstep serve [args]` |
| `just clean` | remove `bin/` and `coverage.out` |
| `just check` | `fmt-check`, `vet`, `lint`, `tidy-check` and `test`; this is what CI runs |

Run `just check` before pushing.

## Integration tests

Tests that need Postgres read `DOORSTEP_TEST_DATABASE_URL` and skip when it is unset. To run them locally, point it at a disposable database:

```sh
docker run --rm -d --name doorstep-pg -e POSTGRES_PASSWORD=doorstep -e POSTGRES_DB=doorstep_test -p 5432:5432 postgres:18-alpine
export DOORSTEP_TEST_DATABASE_URL='postgres://postgres:doorstep@localhost:5432/doorstep_test?sslmode=disable'
just test
```

Each test gets its own schema, so tests can run in parallel against one database.

## Continuous integration

`.github/workflows/ci.yml` runs on every pull request and on pushes to `main`. A single job, `check`, on `ubuntu-24.04`:

1. checks out the code without persisting credentials;
2. installs the tools from `mise.toml`/`mise.lock` with `jdx/mise-action`;
3. runs `just check` and `just build`.

A `postgres:18-alpine` service (pinned by digest) runs alongside the job with `DOORSTEP_TEST_DATABASE_URL` exported. Actions are pinned by full commit SHA, the workflow token is read-only, and superseded pull request runs are cancelled. Dependabot (`.github/dependabot.yml`) proposes weekly grouped updates for actions and Go modules.

## Conventions

- No code comments or doc comments; names carry the meaning. Compiler directives such as `//go:embed` are fine.
- Standard library `testing` only: no testify. Tests are table-driven, use `t.Run` and `t.Parallel`, and `httptest` for HTTP.
- No package-level mutable state, no `init()`, no global logger; inject dependencies through constructors.
- `log/slog` for logging. depguard rejects `io/ioutil`, `github.com/pkg/errors`, testify, logrus, zap and zerolog.
- Follow the package layout and dependency rules in [architecture.md](architecture.md).

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

Tests that need Postgres read `DOORSTEP_TEST_DATABASE_URL` (a `postgres://` URL) and skip when it is unset. Set `DOORSTEP_TEST_REQUIRE_DATABASE=true` to make them fail instead of skipping; CI does, so a lost variable cannot silently turn the integration tests off. To run them locally, start a disposable database on a free port:

```sh
container=$(docker run -d --rm -e POSTGRES_PASSWORD=doorstep -p 127.0.0.1:0:5432 postgres:18-alpine)
port=$(docker port "$container" 5432 | cut -d: -f2)
export DOORSTEP_TEST_DATABASE_URL="postgres://postgres:doorstep@127.0.0.1:$port/postgres?sslmode=disable"
export DOORSTEP_TEST_REQUIRE_DATABASE=true
just test
docker stop "$container"
```

Tests that only unit-test logic need none of this; `serve` and `migrate` command tests and everything in `internal/store` do.

`internal/store/storetest` isolates tests so they can run in parallel against one database:

- `storetest.Open(t)` returns a `*store.Store` connected to a fresh schema with all migrations applied, closed when the test ends.
- `storetest.DatabaseURL(t)` creates a fresh, empty schema and returns a URL whose `search_path` points at it (with `pool_max_conns=5`), for tests that open the store or run commands themselves.

Each schema is named `t_<random hex>` and dropped with everything in it when the test finishes, so no `CREATEDB` privilege is needed. `storetest.Open` migrates with `store.WithoutSessionLock()`, because the migration advisory lock is database-wide and would serialise parallel tests; production code always takes the lock.

To run the binary locally, point it at the same container:

```sh
export DOORSTEP_DATABASE_URL="$DOORSTEP_TEST_DATABASE_URL"
bin/doorstep migrate status
bin/doorstep serve
```

## Continuous integration

`.github/workflows/ci.yml` runs on every pull request and on pushes to `main`. A single job, `check`, on `ubuntu-24.04`:

1. checks out the code without persisting credentials;
2. installs the tools from `mise.toml`/`mise.lock` with `jdx/mise-action`;
3. runs `just check` and `just build`.

A `postgres:18-alpine` service (pinned by digest) runs alongside the job with `DOORSTEP_TEST_DATABASE_URL` and `DOORSTEP_TEST_REQUIRE_DATABASE=true` exported, so every Postgres integration test runs and a missing database fails the job. Actions are pinned by full commit SHA, the workflow token is read-only, and superseded pull request runs are cancelled. Dependabot (`.github/dependabot.yml`) proposes weekly grouped updates for actions and Go modules.

## Conventions

- No code comments or doc comments; names carry the meaning. Compiler directives such as `//go:embed` are fine.
- Standard library `testing` only: no testify. Tests are table-driven, use `t.Run` and `t.Parallel`, and `httptest` for HTTP.
- No package-level mutable state, no `init()`, no global logger; inject dependencies through constructors.
- `log/slog` for logging. depguard rejects `io/ioutil`, `github.com/pkg/errors`, testify, logrus, zap and zerolog.
- Follow the package layout and dependency rules in [architecture.md](architecture.md).

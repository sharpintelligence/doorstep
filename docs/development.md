# Development

## Toolchain

Tool versions are pinned in `mise.toml` (Go, golangci-lint, just, actionlint, git-cliff, kustomize, kubeconform) and locked with checksums in `mise.lock`. CI uses the same files, so local and CI runs match.

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
| `just image` | build the container image as `doorstep:<version>` (override with `DOORSTEP_IMAGE`), stamping version, commit and commit time; needs a container engine |
| `just vuln` | `govulncheck` (pinned in the `justfile`) over all packages; needs network access and is not part of `check` |
| `just lint-actions` | `actionlint` over `.github/workflows` |
| `just lint-manifests` | `kustomize build` of `deploy/kustomize/base` and every example overlay, validated with `kubeconform -strict` against the Kubernetes version pinned in the `justfile`; kubeconform downloads schemas, so it needs network access and is not part of `check`; CI runs it as a separate step |
| `just changelog [args]` | `git-cliff` with `cliff.toml`; `just changelog --unreleased` previews the next release notes |
| `just run [args]` | `go run ./cmd/doorstep serve [args]` |
| `just clean` | remove `bin/` and `coverage.out` |
| `just check` | `fmt-check`, `vet`, `lint`, `lint-actions`, `tidy-check` and `test`; this is what CI runs |

Run `just check` before pushing.

The manifests under `deploy/kustomize` are also covered by a hermetic Go test (`deploy/kustomize/manifests_test.go`, part of `just test`). It needs no kustomize binary or network and checks that environment variables are settings known to `internal/config`, that ports match the defaults, that the security context, RBAC and digest pinning have not been weakened, and that every `config.yaml` there validates. When a change renames a setting or alters a default, it fails until the manifests follow.

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

Each schema is named `t_<random hex>` and dropped with everything in it when the test finishes, so no `CREATEDB` privilege is needed. Migrations take the same session advisory lock as production; its ID is derived from the schema, so parallel tests (and the `serve` and `migrate` command tests) never wait for each other. `store.WithoutSessionLock()` remains for tests that migrate one schema repeatedly.

To run the binary locally, point it at the same container:

```sh
export DOORSTEP_DATABASE_URL="$DOORSTEP_TEST_DATABASE_URL"
bin/doorstep migrate status
bin/doorstep serve
```

## GitHub in tests

Tests that need GitHub never call the real API and need no credentials. `githubtest.NewServer(t)` (in `internal/github/githubtest`) starts a fake API with a freshly generated App key: it verifies App JWTs, serves installation lookup and token minting (`Install`, `Suspend`, `Uninstall`, call counters, an injectable clock) and authenticates extra handlers registered with `Handle` by installation token. Point `github.Options.BaseURL` at its `URL`, using its `AppID` and `PrivateKey`.

## Continuous integration

`.github/workflows/ci.yml` runs on every pull request, on pushes to `main` and weekly (Monday 06:23 UTC, so new advisories surface without waiting for a pull request); the weekly run executes only `govulncheck`. All jobs run on `ubuntu-24.04`. The `check` job:

1. checks out the code without persisting credentials;
2. installs the tools from `mise.toml`/`mise.lock` with `jdx/mise-action`;
3. runs `just check` and `just build`.

A `postgres:18-alpine` service (pinned by digest) runs alongside the job with `DOORSTEP_TEST_DATABASE_URL` and `DOORSTEP_TEST_REQUIRE_DATABASE=true` exported, so every Postgres integration test runs and a missing database fails the job. Two more jobs run beside it:

- `govulncheck` runs `just vuln`, using the mise-pinned Go.
- `image` builds the `Dockerfile` for `linux/amd64` and `linux/arm64` without pushing, with the same `build-push-action` inputs as the release workflow (including the commit and commit time build arguments). It then loads a `linux/amd64` build and asserts that `doorstep version` reports the version, commit SHA and commit time, so a renamed link-time variable fails CI. Cross-compilation keeps it cheap, and it catches Dockerfile and workflow breakage before the first tag.

Actions are pinned by full commit SHA, the workflow token is read-only, and superseded pull request runs are cancelled. Dependabot (`.github/dependabot.yml`) proposes weekly grouped updates for actions, Go modules and Docker base images. Releases are described in [releasing.md](releasing.md).

## Container image

The `Dockerfile` cross-compiles on the build platform (`GOOS`/`GOARCH` from `TARGETOS`/`TARGETARCH`) and copies the static binary into a distroless `static` nonroot image; no emulation is needed. Both base images are pinned by index digest and kept fresh by Dependabot. The builder image's Go version must match `mise.toml`: `GOTOOLCHAIN=local` makes an older image fail loudly instead of downloading a toolchain. `.dockerignore` is an allowlist (`go.mod`, `go.sum`, `cmd`, `internal`), so edits elsewhere do not invalidate the build cache.

## Conventions

- No code comments or doc comments; names carry the meaning. Compiler directives such as `//go:embed` are fine.
- Standard library `testing` only: no testify. Tests are table-driven, use `t.Run` and `t.Parallel`, and `httptest` for HTTP.
- No package-level mutable state, no `init()`, no global logger; inject dependencies through constructors.
- `log/slog` for logging. depguard rejects `io/ioutil`, `github.com/pkg/errors`, testify, logrus, zap and zerolog.
- Follow the package layout and dependency rules in [architecture.md](architecture.md).

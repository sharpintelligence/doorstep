# Contributing

Thanks for helping with doorstep. This page is the short version; [docs/development.md](docs/development.md) has the detail.

## Getting set up

Tool versions are pinned in `mise.toml` and `mise.lock`:

```sh
mise trust ./mise.toml
mise install
```

Run commands inside the mise environment, either an activated shell or `mise exec -- <cmd>`.

## The quality gate

```sh
just check
```

This runs the formatting check, `go vet`, `golangci-lint`, `actionlint`, the `go mod tidy` check and the tests, and is what CI runs. CI also runs `just lint-manifests` (kustomize and kubeconform), `just vuln` (govulncheck) and an image build; all three need network access.

## Tests

`just test` runs `go test -race -shuffle=on -count=1 ./...`. Tests that need Postgres read `DOORSTEP_TEST_DATABASE_URL` and skip when it is unset; set `DOORSTEP_TEST_REQUIRE_DATABASE=true` to fail instead of skipping, as CI does. A disposable database:

```sh
container=$(docker run -d --rm -e POSTGRES_PASSWORD=doorstep -p 127.0.0.1:0:5432 postgres:18-alpine)
port=$(docker port "$container" 5432 | cut -d: -f2)
export DOORSTEP_TEST_DATABASE_URL="postgres://postgres:doorstep@127.0.0.1:$port/postgres?sslmode=disable"
export DOORSTEP_TEST_REQUIRE_DATABASE=true
just test
docker stop "$container"
```

## Conventions

- No code comments and no doc comments. Name things so the code explains itself; compiler directives are fine.
- Tests use the standard `testing` package (no testify), are table-driven and call `t.Parallel()`.
- No package-level mutable state and no `init()`. Pass dependencies, including the logger and clock, through constructors.
- Log with `log/slog`.
- Follow the package map and dependency rules in [docs/architecture.md](docs/architecture.md#dependency-rules).

## Database migrations

doorstep deploys its own upgrades, and the previous release must keep running against a migrated database ([docs/self-hosting.md](docs/self-hosting.md)). A pull request that adds a file under `internal/store/migrations`:

- Follows expand/contract: a release adds and dual-writes, and only the next release may remove what the previous one no longer uses.
- Puts anything that removes or narrows schema in a file named `NNNNN_contract_<description>.sql` that raises the compatibility floor. A test in `just check` rejects destructive statements anywhere else.
- Goes through the review checklist in [docs/database.md](docs/database.md#compatibility-across-one-release).
- Carries a `Doorstep-Migration: true` line in its squash-merge commit message, so the self-hosted install flags the release as containing migrations.

## Decisions

Write an architecture decision record for a decision that is expensive to reverse, such as the datastore, trust boundaries, the shape of the API or configuration, or the security model. See [when to write one and the process](docs/decisions/README.md#process).

## Documentation

Update the docs in the same pull request as the behaviour. `docs/configuration.md`, the relative links in every Markdown file and the kustomize manifests are checked by tests, so a renamed setting or a broken link fails `just check`.

## Issue tracking

Maintainers track work in [beads](https://github.com/gastownhall/beads) (`bd`), stored under `.beads/`. Commit and pull request titles carry the bead ID in parentheses, which the changelog tooling strips. You do not need `bd`: use GitHub Issues, and do not include changes under `.beads/` in a pull request. Agent workflows are described in [AGENTS.md](AGENTS.md).

## Pull requests

- One focused change per pull request, with an imperative subject line.
- `just check` passes locally.
- New behaviour has tests, and docs are updated.
- CI passes: check, lint-manifests, govulncheck and image.
- New dependencies come with a justification.
- Pull requests are squash merged.

## Security

Do not report vulnerabilities in public issues. Follow [SECURITY.md](SECURITY.md).

## Conduct

This project follows the [code of conduct](CODE_OF_CONDUCT.md).

## License

doorstep is licensed under Apache-2.0, and contributions are accepted under the same license.

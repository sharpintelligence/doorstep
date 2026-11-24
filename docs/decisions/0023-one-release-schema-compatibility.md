# 0023: One-release schema compatibility

- Status: Accepted
- Date: 2026-11-24

## Context

Until now a doorstep binary refused to start when the database schema was newer than its newest embedded migration (`store.ErrDatabaseNewer`). That is safe against running on a schema the binary does not understand, but it breaks two things self-hosting depends on ([ADR 0022](0022-self-hosting-through-a-release-branch.md)):

- During a rolling update the new pod migrates the database before it becomes ready, while the old pod keeps running. If the old pod restarts in that window, it cannot start again.
- The escape hatch, reverting the pin commit so Argo CD restores the previous image, fails for any release that added a migration.

A policy that migrations stay backward compatible for one release is meaningless if the previous binary cannot start against the migrated schema. Removing the check altogether would let an arbitrarily old binary run against a schema it does not understand.

## Decision

1. Migration `00012_schema_compatibility.sql` creates a single-row table `schema_compatibility (min_binary_schema bigint)` seeded with `12`. It is the compatibility floor: the oldest newest-migration a binary may know and still run against this database.
2. On startup, when the database version is ahead of the binary's newest embedded migration, `serve` and `migrate` start in compatibility mode if that migration is at least `min_binary_schema`: nothing is applied and a warning is logged with `database_version`, `binary_version` and `min_binary_schema`. `doorstep migrate status` prints a one-line note above the table. Otherwise, or when the table or row is missing, they fail with `ErrDatabaseNewer`, and the message names the floor.
3. Migrations are expand/contract: release N adds and dual-writes, release N+1 may remove what N no longer uses. A migration that removes or narrows something is a contract step. It is named `NNNNN_contract_<description>.sql` and raises the floor with `UPDATE schema_compatibility SET min_binary_schema = <n>`, where `n` is the newest migration of the previous release.
4. A Go test in `internal/store`, which needs no database and runs in `just check`, enforces this for migrations newer than `00011`: a file that drops anything (a table, column, type, view, function, schema, index, trigger, default, identity, constraint and so on), renames, changes a column type, sets `NOT NULL`, truncates, adds a `UNIQUE`, `PRIMARY KEY`, `FOREIGN KEY` or `EXCLUDE` constraint to an existing table, or adds a `NOT NULL` column without `DEFAULT` must be a contract file; a contract file must update the floor; the floor must be at least 12, at least the floor set by earlier files, and below the file's own version. The one `DROP` that is not flagged is a `DROP CONSTRAINT` re-added under the same name as a `CHECK` in the same file, which is how enumeration values are added. `DROP NOT NULL` and `DROP EXPRESSION` widen and are not flagged either. [database.md](../database.md#compatibility-across-one-release) lists exactly what the test covers.
5. What a regular expression cannot see is a review checklist in [database.md](../database.md#compatibility-across-one-release): whether a replaced enumeration `CHECK` narrows, values old readers cannot parse, new unique indexes, backfills, and the `Doorstep-Migration: true` trailer. CI fails a pull request that changes `internal/store/migrations` when none of its commits carries the trailer.
6. Migrations `00001` to `00011` are grandfathered: they predate the policy and no release has been tagged.

## Options considered

- **Keep the hard failure.** Rejected: it makes the rolling update and the escape hatch fail for every release with a migration.
- **Remove the check.** Rejected: any old binary would run against any newer schema.
- **A compatibility floor in the database (chosen).** One table, one query on the rare path, and it fails closed: a binary that predates the table, or a floor above the binary, still refuses to start.
- **Run the previous release's queries against the new schema in CI.** Rejected as too heavy for the value; the lint and the checklist catch the common cases.
- **Lint every `DROP CONSTRAINT` and `ADD CONSTRAINT`.** Rejected: this codebase adds enumeration values by replacing a `CHECK` constraint, which is an expand step; flagging it would train people to ignore the lint. The lint flags a dropped constraint unless the same file adds a `CHECK` of the same name, and flags added uniqueness and foreign keys. Whether a replaced `CHECK` narrows is left to review.

## Consequences

- The previous release can run against the migrated schema, so a rolling update survives an old pod restart and a git revert restores a working version.
- The guarantee is exactly one release back. The floor is a best-effort guard set by hand, not an exact release counter: it is only a lower bound, so any binary at or above it starts in compatibility mode however many expand-only releases behind it is. Such a binary is not refused, but running more than one release behind is unsupported and untested; the expand/contract rules only promise that the previous release works.
- Binaries older than this release have no compatibility mode and keep failing against a newer schema, which is correct. The first release that benefits is the one after the release that ships `00012`: reverting that release, or an old pod restarting during its rollout, fails, and its upgrade needs the backup-and-restore path.
- Contract steps are visible in file names and reviews, and each one deliberately drops support for older binaries.

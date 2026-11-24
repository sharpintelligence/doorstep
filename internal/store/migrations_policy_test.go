package store

import (
	"fmt"
	"io/fs"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

const (
	newestGrandfatheredMigration = 11
	initialSchemaFloor           = 12
)

var (
	migrationFileName   = regexp.MustCompile(`^(\d{5})_[a-z0-9_]+\.sql$`)
	contractFileName    = regexp.MustCompile(`^\d{5}_contract_[a-z0-9_]+\.sql$`)
	gooseDownMarker     = regexp.MustCompile(`(?im)^\s*--\s*\+goose\s+down\b`)
	lineComment         = regexp.MustCompile(`--[^\n]*`)
	blockComment        = regexp.MustCompile(`(?s)/\*.*?\*/`)
	stringLiteral       = regexp.MustCompile(`'(?:[^']|'')*'`)
	floorUpdate         = regexp.MustCompile(`\bupdate\s+schema_compatibility\s+set\s+min_binary_schema\s*=\s*(\d+)`)
	addClause           = regexp.MustCompile(`\badd\s+`)
	addNonColumnKeyword = regexp.MustCompile(`^(constraint|primary|unique|check|foreign|exclude|value)\b`)
	notNull             = regexp.MustCompile(`\bnot\s+null\b`)
	columnDefault       = regexp.MustCompile(`\b(default|generated)\b`)
	dropClause          = regexp.MustCompile(`\bdrop\s+(materialized\s+view|[a-z_]+)(?:\s+(?:if\s+exists\s+)?("[^"]+"|[a-z_][a-z0-9_]*))?`)
	droppedObjectKinds  = []string{"table", "column", "type", "view", "materialized view", "function", "procedure", "schema", "sequence", "index", "trigger", "domain", "extension", "policy", "rule", "aggregate", "operator", "cast", "owned", "default", "identity"}
	widenedByDrop       = []string{"not", "expression"}
	addedRestriction    = regexp.MustCompile(`\badd\s+(?:constraint\s+(?:"[^"]+"|[a-z_][a-z0-9_]*)\s+)?(unique|primary\s+key|foreign\s+key|exclude)\b`)
	destructivePatterns = []struct {
		name    string
		pattern *regexp.Regexp
	}{
		{name: "RENAME", pattern: regexp.MustCompile(`\brename\b`)},
		{name: "ALTER COLUMN ... TYPE", pattern: regexp.MustCompile(`\balter\s+(?:column\s+)?(?:"[^"]+"|\w+)\s+(?:set\s+data\s+)?type\b`)},
		{name: "SET NOT NULL", pattern: regexp.MustCompile(`\bset\s+not\s+null\b`)},
		{name: "TRUNCATE", pattern: regexp.MustCompile(`\btruncate\b`)},
	}
)

type migrationPolicyResult struct {
	violations []string
	floor      int64
}

func checkMigrationPolicy(name, sql string, highestFloor int64) migrationPolicyResult {
	result := migrationPolicyResult{floor: highestFloor}
	match := migrationFileName.FindStringSubmatch(name)
	if match == nil {
		result.violations = append(result.violations, "file name must match NNNNN_description.sql in lowercase")
		return result
	}
	version, _ := strconv.ParseInt(match[1], 10, 64)
	if version <= newestGrandfatheredMigration {
		return result
	}

	body := normalizedUpSection(sql)
	destructive := destructiveStatements(body)
	contract := contractFileName.MatchString(name)
	floors := floorUpdate.FindAllStringSubmatch(body, -1)

	if len(destructive) > 0 && !contract {
		result.violations = append(result.violations, fmt.Sprintf("contains %s: destructive changes must be in an NNNNN_contract_<description>.sql file that raises the schema compatibility floor", strings.Join(destructive, ", ")))
	}
	if contract && len(floors) == 0 {
		result.violations = append(result.violations, "contract migration must UPDATE schema_compatibility SET min_binary_schema = <newest migration of the previous release>")
	}
	for _, f := range floors {
		floor, err := strconv.ParseInt(f[1], 10, 64)
		switch {
		case err != nil:
			result.violations = append(result.violations, fmt.Sprintf("schema compatibility floor %s is not a number", f[1]))
		case floor < max(initialSchemaFloor, highestFloor):
			result.violations = append(result.violations, fmt.Sprintf("schema compatibility floor %d lowers the floor %d set by an earlier migration", floor, max(initialSchemaFloor, highestFloor)))
		case floor >= version:
			result.violations = append(result.violations, fmt.Sprintf("schema compatibility floor %d must be below the migration's own version %d", floor, version))
		default:
			result.floor = max(result.floor, floor)
		}
	}
	return result
}

func normalizedUpSection(sql string) string {
	if loc := gooseDownMarker.FindStringIndex(sql); loc != nil {
		sql = sql[:loc[0]]
	}
	sql = blockComment.ReplaceAllString(sql, " ")
	sql = lineComment.ReplaceAllString(sql, " ")
	sql = stringLiteral.ReplaceAllString(sql, "''")
	return strings.ToLower(sql)
}

func destructiveStatements(body string) []string {
	found := droppedObjects(body)
	for _, d := range destructivePatterns {
		if d.pattern.MatchString(body) {
			found = append(found, d.name)
		}
	}
	for _, m := range addedRestriction.FindAllStringSubmatch(body, -1) {
		found = appendOnce(found, "ADD "+strings.ToUpper(strings.Join(strings.Fields(m[1]), " ")))
	}
	if addsRequiredColumnWithoutDefault(body) {
		found = append(found, "ADD COLUMN ... NOT NULL without DEFAULT")
	}
	return found
}

func droppedObjects(body string) []string {
	var found []string
	for _, m := range dropClause.FindAllStringSubmatch(body, -1) {
		kind := strings.Join(strings.Fields(m[1]), " ")
		switch {
		case slices.Contains(widenedByDrop, kind):
		case kind == "constraint":
			if !replacesCheckConstraint(body, m[2]) {
				found = appendOnce(found, "DROP CONSTRAINT")
			}
		case slices.Contains(droppedObjectKinds, kind):
			found = appendOnce(found, "DROP "+strings.ToUpper(kind))
		default:
			found = appendOnce(found, "DROP COLUMN")
		}
	}
	return found
}

func replacesCheckConstraint(body, name string) bool {
	if name == "" {
		return false
	}
	readd := regexp.MustCompile(`\badd\s+constraint\s+` + regexp.QuoteMeta(name) + `\s+check\b`)
	return readd.MatchString(body)
}

func appendOnce(found []string, name string) []string {
	if slices.Contains(found, name) {
		return found
	}
	return append(found, name)
}

func addsRequiredColumnWithoutDefault(body string) bool {
	for _, loc := range addClause.FindAllStringIndex(body, -1) {
		rest := body[loc[1]:]
		if addNonColumnKeyword.MatchString(rest) {
			continue
		}
		clause := topLevelClause(rest)
		if notNull.MatchString(clause) && !columnDefault.MatchString(clause) {
			return true
		}
	}
	return false
}

func topLevelClause(s string) string {
	depth := 0
	for i, r := range s {
		switch r {
		case '(':
			depth++
		case ')':
			if depth == 0 {
				return s[:i]
			}
			depth--
		case ',', ';':
			if depth == 0 {
				return s[:i]
			}
		}
	}
	return s
}

func TestCheckMigrationPolicy(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		file         string
		sql          string
		highestFloor int64
		wantFloor    int64
		violations   []string
	}{
		{name: "grandfathered drop", file: "00011_deployment_migration.sql", sql: "-- +goose Up\nALTER TABLE t DROP COLUMN c;"},
		{name: "create table", file: "00013_widgets.sql", sql: "-- +goose Up\nCREATE TABLE widgets (id bigint PRIMARY KEY, name text NOT NULL);"},
		{name: "add nullable column", file: "00013_widget_color.sql", sql: "-- +goose Up\nALTER TABLE widgets ADD COLUMN color text;"},
		{name: "add required column with default", file: "00013_widget_color.sql", sql: "-- +goose Up\nALTER TABLE widgets ADD COLUMN color text NOT NULL DEFAULT 'red';"},
		{name: "add identity column", file: "00013_widget_seq.sql", sql: "-- +goose Up\nALTER TABLE widgets ADD COLUMN seq bigint NOT NULL GENERATED ALWAYS AS IDENTITY;"},
		{name: "replace enum check", file: "00013_widget_state.sql", sql: "-- +goose Up\nALTER TABLE widgets\n    DROP CONSTRAINT widgets_state_check,\n    ADD CONSTRAINT widgets_state_check CHECK (state IN ('a', 'b'));"},
		{name: "add check with not null", file: "00013_widget_state.sql", sql: "-- +goose Up\nALTER TABLE widgets ADD CONSTRAINT widgets_name_check CHECK (name IS NOT NULL);"},
		{name: "add enum value", file: "00013_widget_kind.sql", sql: "-- +goose Up\nALTER TYPE widget_kind ADD VALUE 'big';"},
		{name: "destructive only in comments", file: "00013_widgets.sql", sql: "-- +goose Up\n-- DROP TABLE widgets later\n/* ALTER TABLE widgets RENAME TO gadgets */\nCREATE INDEX widgets_name_idx ON widgets (name);"},
		{name: "destructive only in a string", file: "00013_widgets.sql", sql: "-- +goose Up\nINSERT INTO notes (body) VALUES ('drop table widgets');"},
		{name: "destructive only in down section", file: "00013_widgets.sql", sql: "-- +goose Up\nCREATE TABLE widgets (id bigint);\n\n-- +goose Down\nDROP TABLE widgets;"},
		{name: "drop table", file: "00013_drop_widgets.sql", sql: "-- +goose Up\nDROP TABLE widgets;", violations: []string{"DROP TABLE"}},
		{name: "drop column", file: "00013_widgets.sql", sql: "-- +goose Up\nALTER TABLE widgets DROP COLUMN color;", violations: []string{"DROP COLUMN"}},
		{name: "rename column", file: "00013_widgets.sql", sql: "-- +goose Up\nALTER TABLE widgets RENAME COLUMN color TO colour;", violations: []string{"RENAME"}},
		{name: "rename table", file: "00013_widgets.sql", sql: "-- +goose Up\nALTER TABLE widgets RENAME TO gadgets;", violations: []string{"RENAME"}},
		{name: "change column type", file: "00013_widgets.sql", sql: "-- +goose Up\nALTER TABLE widgets ALTER COLUMN size TYPE bigint;", violations: []string{"ALTER COLUMN ... TYPE"}},
		{name: "change column type without column keyword", file: "00013_widgets.sql", sql: "-- +goose Up\nalter table widgets alter size set data type bigint;", violations: []string{"ALTER COLUMN ... TYPE"}},
		{name: "set not null", file: "00013_widgets.sql", sql: "-- +goose Up\nALTER TABLE widgets ALTER COLUMN color SET NOT NULL;", violations: []string{"SET NOT NULL"}},
		{name: "add required column without default", file: "00013_widgets.sql", sql: "-- +goose Up\nALTER TABLE widgets ADD COLUMN color text NOT NULL;", violations: []string{"ADD COLUMN ... NOT NULL without DEFAULT"}},
		{name: "second added column is required", file: "00013_widgets.sql", sql: "-- +goose Up\nALTER TABLE widgets ADD COLUMN a text DEFAULT 'x', ADD b numeric(10, 2) NOT NULL;", violations: []string{"ADD COLUMN ... NOT NULL without DEFAULT"}},
		{name: "contract with floor", file: "00014_contract_drop_color.sql", sql: "-- +goose Up\nALTER TABLE widgets DROP COLUMN color;\nUPDATE schema_compatibility SET min_binary_schema = 13;", wantFloor: 13},
		{name: "contract with floor at the initial floor", file: "00013_contract_drop_color.sql", sql: "-- +goose Up\nALTER TABLE widgets DROP COLUMN color;\nupdate schema_compatibility set min_binary_schema=12;", wantFloor: 12},
		{name: "contract without floor", file: "00014_contract_drop_color.sql", sql: "-- +goose Up\nALTER TABLE widgets DROP COLUMN color;", violations: []string{"must UPDATE schema_compatibility"}},
		{name: "contract with floor only in a comment", file: "00014_contract_drop_color.sql", sql: "-- +goose Up\nALTER TABLE widgets DROP COLUMN color;\n-- UPDATE schema_compatibility SET min_binary_schema = 13;", violations: []string{"must UPDATE schema_compatibility"}},
		{name: "contract floor below initial", file: "00014_contract_drop_color.sql", sql: "-- +goose Up\nDROP TABLE widgets;\nUPDATE schema_compatibility SET min_binary_schema = 11;", violations: []string{"lowers the floor 12"}},
		{name: "contract floor at own version", file: "00014_contract_drop_color.sql", sql: "-- +goose Up\nDROP TABLE widgets;\nUPDATE schema_compatibility SET min_binary_schema = 14;", violations: []string{"below the migration's own version 14"}},
		{name: "floor lowered after an earlier contract", file: "00020_contract_drop_size.sql", sql: "-- +goose Up\nALTER TABLE widgets DROP COLUMN size;\nUPDATE schema_compatibility SET min_binary_schema = 13;", highestFloor: 15, wantFloor: 15, violations: []string{"lowers the floor 15"}},
		{name: "non-contract floor raise", file: "00016_bump_floor.sql", sql: "-- +goose Up\nUPDATE schema_compatibility SET min_binary_schema = 15;", highestFloor: 13, wantFloor: 15},
		{name: "contract name required for destructive", file: "00014_drop_color.sql", sql: "-- +goose Up\nALTER TABLE widgets DROP COLUMN color;\nUPDATE schema_compatibility SET min_binary_schema = 13;", wantFloor: 13, violations: []string{"must be in an NNNNN_contract_"}},
		{name: "drop not null", file: "00013_widgets.sql", sql: "-- +goose Up\nALTER TABLE widgets ALTER COLUMN color DROP NOT NULL;"},
		{name: "drop generated expression", file: "00013_widgets.sql", sql: "-- +goose Up\nALTER TABLE widgets ALTER COLUMN total DROP EXPRESSION;"},
		{name: "replace quoted enum check", file: "00013_widget_state.sql", sql: "-- +goose Up\nALTER TABLE widgets DROP CONSTRAINT IF EXISTS \"Widgets_State\", ADD CONSTRAINT \"Widgets_State\" CHECK (state IN ('a'));"},
		{name: "add foreign key in a new table", file: "00013_widgets.sql", sql: "-- +goose Up\nCREATE TABLE parts (id bigint PRIMARY KEY, widget_id bigint REFERENCES widgets (id), UNIQUE (widget_id));"},
		{name: "drop column without the column keyword", file: "00013_widgets.sql", sql: "-- +goose Up\nALTER TABLE widgets DROP IF EXISTS color;", violations: []string{"DROP COLUMN"}},
		{name: "drop type", file: "00013_widgets.sql", sql: "-- +goose Up\nDROP TYPE widget_kind;", violations: []string{"DROP TYPE"}},
		{name: "drop view", file: "00013_widgets.sql", sql: "-- +goose Up\nDROP VIEW IF EXISTS widget_summary;", violations: []string{"DROP VIEW"}},
		{name: "drop materialized view", file: "00013_widgets.sql", sql: "-- +goose Up\nDROP MATERIALIZED VIEW widget_totals;", violations: []string{"DROP MATERIALIZED VIEW"}},
		{name: "drop function", file: "00013_widgets.sql", sql: "-- +goose Up\nDROP FUNCTION notify_widgets();", violations: []string{"DROP FUNCTION"}},
		{name: "drop schema", file: "00013_widgets.sql", sql: "-- +goose Up\nDROP SCHEMA legacy CASCADE;", violations: []string{"DROP SCHEMA"}},
		{name: "drop index", file: "00013_widgets.sql", sql: "-- +goose Up\nDROP INDEX CONCURRENTLY widgets_name_key;", violations: []string{"DROP INDEX"}},
		{name: "drop trigger", file: "00013_widgets.sql", sql: "-- +goose Up\nDROP TRIGGER widgets_notify ON widgets;", violations: []string{"DROP TRIGGER"}},
		{name: "drop default", file: "00013_widgets.sql", sql: "-- +goose Up\nALTER TABLE widgets ALTER COLUMN color DROP DEFAULT;", violations: []string{"DROP DEFAULT"}},
		{name: "drop identity", file: "00013_widgets.sql", sql: "-- +goose Up\nALTER TABLE widgets ALTER COLUMN id DROP IDENTITY IF EXISTS;", violations: []string{"DROP IDENTITY"}},
		{name: "drop unique constraint", file: "00013_widgets.sql", sql: "-- +goose Up\nALTER TABLE widgets DROP CONSTRAINT widgets_name_key;", violations: []string{"DROP CONSTRAINT"}},
		{name: "drop check and add a different one", file: "00013_widgets.sql", sql: "-- +goose Up\nALTER TABLE widgets DROP CONSTRAINT widgets_state_check, ADD CONSTRAINT widgets_state_check2 CHECK (state IN ('a'));", violations: []string{"DROP CONSTRAINT"}},
		{name: "drop check and add a unique constraint of the same name", file: "00013_widgets.sql", sql: "-- +goose Up\nALTER TABLE widgets DROP CONSTRAINT widgets_c, ADD CONSTRAINT widgets_c UNIQUE (name);", violations: []string{"DROP CONSTRAINT, ADD UNIQUE"}},
		{name: "truncate", file: "00013_widgets.sql", sql: "-- +goose Up\nTRUNCATE widgets;", violations: []string{"TRUNCATE"}},
		{name: "add unique constraint", file: "00013_widgets.sql", sql: "-- +goose Up\nALTER TABLE widgets ADD CONSTRAINT widgets_name_key UNIQUE (name);", violations: []string{"ADD UNIQUE"}},
		{name: "add unnamed unique constraint", file: "00013_widgets.sql", sql: "-- +goose Up\nALTER TABLE widgets ADD UNIQUE (name);", violations: []string{"ADD UNIQUE"}},
		{name: "add foreign key", file: "00013_widgets.sql", sql: "-- +goose Up\nALTER TABLE widgets ADD CONSTRAINT widgets_owner_fkey FOREIGN KEY (owner_id) REFERENCES owners (id);", violations: []string{"ADD FOREIGN KEY"}},
		{name: "add primary key", file: "00013_widgets.sql", sql: "-- +goose Up\nALTER TABLE widgets ADD PRIMARY KEY (id);", violations: []string{"ADD PRIMARY KEY"}},
		{name: "several destructive statements", file: "00013_widgets.sql", sql: "-- +goose Up\nDROP TABLE a;\nDROP TABLE b;\nTRUNCATE c;", violations: []string{"DROP TABLE, TRUNCATE"}},
		{name: "uppercase file name", file: "00013_Widgets.sql", sql: "-- +goose Up\nSELECT 1;", violations: []string{"file name must match"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := checkMigrationPolicy(tt.file, tt.sql, tt.highestFloor)
			if len(got.violations) != len(tt.violations) {
				t.Fatalf("violations = %q, want %d matching %q", got.violations, len(tt.violations), tt.violations)
			}
			for i, want := range tt.violations {
				if !strings.Contains(got.violations[i], want) {
					t.Errorf("violation %d = %q, want it to contain %q", i, got.violations[i], want)
				}
			}
			if got.floor != tt.wantFloor {
				t.Errorf("floor = %d, want %d", got.floor, tt.wantFloor)
			}
		})
	}
}

func TestEmbeddedMigrationsFollowCompatibilityPolicy(t *testing.T) {
	t.Parallel()

	entries, err := fs.ReadDir(embeddedMigrations, "migrations")
	if err != nil {
		t.Fatalf("read embedded migrations: %v", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	slices.Sort(names)
	if !slices.Contains(names, "00012_schema_compatibility.sql") {
		t.Fatal("the schema compatibility migration is missing")
	}

	var floor int64
	for _, name := range names {
		sql, err := fs.ReadFile(embeddedMigrations, "migrations/"+name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		result := checkMigrationPolicy(name, string(sql), floor)
		for _, v := range result.violations {
			t.Errorf("%s: %s", name, v)
		}
		floor = result.floor
	}
}

package core

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestCleanMigrationReason(t *testing.T) {
	t.Parallel()

	long := strings.Repeat("ab ", 100)
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "empty", in: "", want: ""},
		{name: "plain", in: "label db-migration on #12 (abc1234)", want: "label db-migration on #12 (abc1234)"},
		{name: "collapses whitespace", in: "  a \t b\n\nc  ", want: "a b c"},
		{name: "drops control characters", in: "a\x00b\x7fc", want: "a b c"},
		{name: "drops invalid UTF-8", in: "a\xffb", want: "ab"},
		{name: "truncates on rune boundary", in: strings.Repeat("é", 250), want: strings.Repeat("é", MaxMigrationReasonLength)},
		{name: "truncation trims trailing space", in: long, want: strings.TrimSpace(strings.Repeat("ab ", 67)[:MaxMigrationReasonLength])},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := CleanMigrationReason(tt.in)
			if got != tt.want {
				t.Errorf("CleanMigrationReason(%q) = %q, want %q", tt.in, got, tt.want)
			}
			if n := utf8.RuneCountInString(got); n > MaxMigrationReasonLength {
				t.Errorf("length %d exceeds the limit", n)
			}
			if err := validateMigration(got != "", got); err != nil {
				t.Errorf("cleaned reason does not validate: %v", err)
			}
		})
	}
}

func TestMigrationRollbackError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		err        *MigrationRollbackError
		wantParts  []string
		wantIDs    []DeploymentID
		wantAbsent string
	}{
		{
			name: "one crossed deployment with reason",
			err: &MigrationRollbackError{
				To:         Deployment{ID: 41},
				Migrations: []Deployment{{ID: 44, Migration: true, MigrationReason: "label db-migration on #12 (abc1234)"}},
			},
			wantParts: []string{"deployment 41", "deployment 44 (label db-migration on #12 (abc1234))", "confirm_migration_rollback=true", "--confirm-migration-rollback"},
			wantIDs:   []DeploymentID{44},
		},
		{
			name: "several crossed deployments, one without reason",
			err: &MigrationRollbackError{
				To:         Deployment{ID: 3},
				Migrations: []Deployment{{ID: 5, Migration: true}, {ID: 9, Migration: true, MigrationReason: "trailer"}},
			},
			wantParts:  []string{"deployment 3", "deployment 5, deployment 9 (trailer)"},
			wantIDs:    []DeploymentID{5, 9},
			wantAbsent: "deployment 5 (",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var err error = tt.err
			for _, part := range tt.wantParts {
				if !strings.Contains(err.Error(), part) {
					t.Errorf("Error() = %q, want it to contain %q", err.Error(), part)
				}
			}
			if tt.wantAbsent != "" && strings.Contains(err.Error(), tt.wantAbsent) {
				t.Errorf("Error() = %q, must not contain %q", err.Error(), tt.wantAbsent)
			}
			if !errors.Is(fmt.Errorf("wrapped: %w", err), ErrMigrationConfirmationRequired) {
				t.Error("error does not match ErrMigrationConfirmationRequired")
			}
			if errors.Is(err, ErrRollbackInProgress) {
				t.Error("error matches an unrelated sentinel")
			}
			got, ok := errors.AsType[*MigrationRollbackError](fmt.Errorf("wrapped: %w", err))
			if !ok || fmt.Sprint(got.IDs()) != fmt.Sprint(tt.wantIDs) {
				t.Errorf("IDs() = %v, want %v", got.IDs(), tt.wantIDs)
			}
		})
	}
}

package core

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

const MaxMigrationReasonLength = 200

var ErrMigrationConfirmationRequired = errors.New("rollback crosses a database migration and needs confirmation")

func CleanMigrationReason(reason string) string {
	cleaned := strings.Join(strings.FieldsFunc(strings.ToValidUTF8(reason, ""), isSpaceOrControl), " ")
	if utf8.RuneCountInString(cleaned) <= MaxMigrationReasonLength {
		return cleaned
	}
	runes := []rune(cleaned)
	return strings.TrimRightFunc(string(runes[:MaxMigrationReasonLength]), unicode.IsSpace)
}

func validateMigration(migration bool, reason string) error {
	switch n := utf8.RuneCountInString(reason); {
	case !migration && reason != "":
		return errors.New("migration reason must be empty unless the deployment is flagged as a migration")
	case !utf8.ValidString(reason):
		return errors.New("migration reason must be valid UTF-8")
	case n > MaxMigrationReasonLength:
		return fmt.Errorf("migration reason must be at most %d characters, got %d", MaxMigrationReasonLength, n)
	case strings.ContainsFunc(reason, unicode.IsControl):
		return errors.New("migration reason must not contain control characters")
	}
	return nil
}

type MigrationRollbackError struct {
	To         Deployment
	Migrations []Deployment
}

func (e *MigrationRollbackError) IDs() []DeploymentID {
	ids := make([]DeploymentID, len(e.Migrations))
	for i, d := range e.Migrations {
		ids[i] = d.ID
	}
	return ids
}

func (e *MigrationRollbackError) Error() string {
	crossed := make([]string, len(e.Migrations))
	for i, d := range e.Migrations {
		crossed[i] = "deployment " + strconv.FormatInt(int64(d.ID), 10)
		if d.MigrationReason != "" {
			crossed[i] += " (" + d.MigrationReason + ")"
		}
	}
	return "rolling back to deployment " + strconv.FormatInt(int64(e.To.ID), 10) +
		" crosses database migrations in " + strings.Join(crossed, ", ") +
		"; the old version may not work with the migrated schema. Confirm with confirm_migration_rollback=true (API) or --confirm-migration-rollback (CLI)"
}

func (e *MigrationRollbackError) Is(target error) bool {
	return target == ErrMigrationConfirmationRequired
}

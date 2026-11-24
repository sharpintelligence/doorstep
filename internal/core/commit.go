package core

import (
	"errors"
	"fmt"
	"strings"
)

const commitSHALength = 40

var ErrInvalidCommitSHA = errors.New("invalid commit SHA")

type Commit struct {
	Repository string
	SHA        string
}

func (c Commit) Validate() error {
	var errs []error
	if strings.TrimSpace(c.Repository) == "" {
		errs = append(errs, errors.New("commit repository must not be empty"))
	}
	if err := ValidateCommitSHA(c.SHA); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func ValidateCommitSHA(sha string) error {
	if !isLowerHex(sha, commitSHALength) {
		return fmt.Errorf("%w: %q must be %d lowercase hex characters", ErrInvalidCommitSHA, sha, commitSHALength)
	}
	return nil
}

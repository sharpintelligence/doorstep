package core

import (
	"errors"
	"strings"
)

var ErrStateConflict = errors.New("deployment state conflict")

type Lineage struct {
	Digest           Digest
	PreviousDigest   Digest
	GitOpsRepository string
	PinCommitSHA     string
	ArgoCDRevision   string
}

func (l Lineage) IsZero() bool {
	return l == Lineage{}
}

func (l Lineage) Validate() error {
	var errs []error
	if l.Digest != "" {
		errs = append(errs, l.Digest.Validate())
	}
	if l.PreviousDigest != "" {
		errs = append(errs, prefixError("previous digest", l.PreviousDigest.Validate()))
	}
	if l.GitOpsRepository != "" && strings.TrimSpace(l.GitOpsRepository) == "" {
		errs = append(errs, errors.New("gitops repository must not be blank"))
	}
	if l.PinCommitSHA != "" {
		errs = append(errs, prefixError("pin commit", ValidateCommitSHA(l.PinCommitSHA)))
	}
	if l.ArgoCDRevision != "" && strings.TrimSpace(l.ArgoCDRevision) == "" {
		errs = append(errs, errors.New("argo cd revision must not be blank"))
	}
	return errors.Join(errs...)
}

func TerminalStates() []State {
	return []State{StateSucceeded, StateFailed, StateCancelled, StateSuperseded}
}

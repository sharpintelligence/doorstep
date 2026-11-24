package core

import (
	"errors"
	"strings"
)

type Target struct {
	App         string
	Environment string
}

func (t Target) String() string {
	return t.App + "/" + t.Environment
}

func (t Target) Validate() error {
	var errs []error
	if strings.TrimSpace(t.App) == "" {
		errs = append(errs, errors.New("target app must not be empty"))
	}
	if strings.TrimSpace(t.Environment) == "" {
		errs = append(errs, errors.New("target environment must not be empty"))
	}
	return errors.Join(errs...)
}

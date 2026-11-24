package core

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode"
)

type ActorKind string

const (
	ActorUser   ActorKind = "user"
	ActorToken  ActorKind = "token"
	ActorCLI    ActorKind = "cli"
	ActorSystem ActorKind = "system"
)

func ActorKinds() []ActorKind {
	return []ActorKind{ActorUser, ActorToken, ActorCLI, ActorSystem}
}

func (k ActorKind) Valid() bool {
	return slices.Contains(ActorKinds(), k)
}

type Actor struct {
	Kind ActorKind
	ID   string
	Name string
}

func (a Actor) String() string {
	if a.ID == "" {
		return string(a.Kind)
	}
	return string(a.Kind) + ":" + a.ID
}

func (a Actor) Validate() error {
	var errs []error
	if !a.Kind.Valid() {
		errs = append(errs, fmt.Errorf("unknown actor kind %q", a.Kind))
	}
	switch {
	case a.ID == "" && a.Kind != ActorSystem:
		errs = append(errs, errors.New("actor id must not be empty"))
	case strings.ContainsFunc(a.ID, unicode.IsControl), strings.TrimSpace(a.ID) != a.ID:
		errs = append(errs, fmt.Errorf("actor id %q must not contain control characters or surrounding whitespace", a.ID))
	}
	return errors.Join(errs...)
}

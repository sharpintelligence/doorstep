package core

import (
	"errors"
	"fmt"
	"slices"
)

type Role string

const (
	RoleViewer   Role = "viewer"
	RoleReporter Role = "reporter"
	RoleDeployer Role = "deployer"
	RoleAdmin    Role = "admin"
)

var ErrUnknownRole = errors.New("unknown role")

func Roles() []Role {
	return []Role{RoleViewer, RoleReporter, RoleDeployer, RoleAdmin}
}

func ParseRole(s string) (Role, error) {
	r := Role(s)
	if !r.Valid() {
		return "", fmt.Errorf("%w %q", ErrUnknownRole, s)
	}
	return r, nil
}

func (r Role) Valid() bool {
	return slices.Contains(Roles(), r)
}

func (r Role) Allows(required Role) bool {
	have := slices.Index(Roles(), r)
	need := slices.Index(Roles(), required)
	return have >= 0 && need >= 0 && have >= need
}

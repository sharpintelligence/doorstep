package auth

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/sharpintelligence/doorstep/internal/core"
)

const (
	ProviderOIDC   = "oidc"
	ProviderGitHub = "github"
)

type Binding struct {
	Groups []string
	Users  []string
}

type RoleMapper struct {
	Provider string
	Default  core.Role
	Bindings map[core.Role]Binding
}

func (m RoleMapper) Validate() error {
	var errs []error
	if m.Provider != ProviderOIDC && m.Provider != ProviderGitHub {
		errs = append(errs, fmt.Errorf("unknown provider %q", m.Provider))
	}
	if m.Default != "" && !m.Default.Valid() {
		errs = append(errs, fmt.Errorf("default role: %w %q", core.ErrUnknownRole, m.Default))
	}
	for role := range m.Bindings {
		if !role.Valid() {
			errs = append(errs, fmt.Errorf("binding: %w %q", core.ErrUnknownRole, role))
		}
	}
	return errors.Join(errs...)
}

func (m RoleMapper) Resolve(id Identity) (core.Role, bool) {
	roles := core.Roles()
	for _, role := range slices.Backward(roles) {
		binding, ok := m.Bindings[role]
		if ok && m.matches(binding, id) {
			return role, true
		}
	}
	if m.Default != "" {
		return m.Default, true
	}
	return "", false
}

func (m RoleMapper) matches(b Binding, id Identity) bool {
	for _, user := range b.Users {
		if m.matchesUser(user, id) {
			return true
		}
	}
	for _, group := range b.Groups {
		if slices.ContainsFunc(id.Groups, func(g string) bool { return m.equalGroup(group, g) }) {
			return true
		}
	}
	return false
}

func (m RoleMapper) matchesUser(entry string, id Identity) bool {
	if m.Provider == ProviderGitHub {
		return strings.EqualFold(entry, id.Username)
	}
	if entry == id.Subject {
		return true
	}
	return id.Email != "" && id.EmailVerified && strings.EqualFold(entry, id.Email)
}

func (m RoleMapper) equalGroup(entry, group string) bool {
	if m.Provider == ProviderGitHub {
		return strings.EqualFold(entry, group)
	}
	return entry == group
}

func (m RoleMapper) GitHubLookups() (orgs, teams bool) {
	for _, b := range m.Bindings {
		for _, g := range b.Groups {
			if strings.Contains(g, "/") {
				teams = true
			} else {
				orgs = true
			}
		}
	}
	return orgs, teams
}

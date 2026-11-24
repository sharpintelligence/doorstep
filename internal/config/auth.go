package config

import (
	"errors"
	"fmt"
	"maps"
	"net"
	"net/url"
	"slices"
	"strings"
	"time"
	"unicode"
)

const (
	ProviderOIDC   = "oidc"
	ProviderGitHub = "github"

	DefaultSessionTTL    = 12 * time.Hour
	MinSessionTTL        = 5 * time.Minute
	MaxSessionTTL        = 720 * time.Hour
	DefaultGroupsClaim   = "groups"
	DefaultUsernameClaim = "preferred_username"

	maxGitHubTeamSlug = 100
)

func DefaultOIDCScopes() []string {
	return []string{"openid", "profile", "email", "groups"}
}

func RoleNames() []string {
	return []string{"viewer", "deployer", "admin"}
}

type Auth struct {
	SessionTTL  *Duration              `yaml:"session_ttl"`
	DefaultRole string                 `yaml:"default_role"`
	OIDC        *OIDC                  `yaml:"oidc"`
	GitHub      *GitHubLogin           `yaml:"github"`
	Roles       map[string]RoleBinding `yaml:"roles"`
}

type OIDC struct {
	IssuerURL        string   `yaml:"issuer_url"`
	ClientID         string   `yaml:"client_id"`
	ClientSecretEnv  string   `yaml:"client_secret_env"`
	ClientSecretFile string   `yaml:"client_secret_file"`
	Scopes           []string `yaml:"scopes"`
	GroupsClaim      string   `yaml:"groups_claim"`
	UsernameClaim    string   `yaml:"username_claim"`
}

type GitHubLogin struct {
	ClientID         string `yaml:"client_id"`
	ClientSecretEnv  string `yaml:"client_secret_env"`
	ClientSecretFile string `yaml:"client_secret_file"`
}

type RoleBinding struct {
	Groups []string `yaml:"groups"`
	Users  []string `yaml:"users"`
}

func (a Auth) Enabled() bool {
	return a.OIDC != nil || a.GitHub != nil
}

func (a Auth) Provider() string {
	switch {
	case a.OIDC != nil:
		return ProviderOIDC
	case a.GitHub != nil:
		return ProviderGitHub
	default:
		return ""
	}
}

func (a Auth) TTL() time.Duration {
	if a.SessionTTL == nil {
		return DefaultSessionTTL
	}
	return a.SessionTTL.Std()
}

func (a Auth) ClientSecret() (SecretRef, bool) {
	switch {
	case a.OIDC != nil:
		return SecretRef{Label: "auth.oidc client secret", Env: a.OIDC.ClientSecretEnv, File: a.OIDC.ClientSecretFile}, true
	case a.GitHub != nil:
		return SecretRef{Label: "auth.github client secret", Env: a.GitHub.ClientSecretEnv, File: a.GitHub.ClientSecretFile}, true
	default:
		return SecretRef{}, false
	}
}

func (a Auth) withDefaults() Auth {
	out := a
	if a.SessionTTL != nil {
		ttl := *a.SessionTTL
		out.SessionTTL = &ttl
	}
	if a.OIDC != nil {
		o := *a.OIDC
		if o.Scopes == nil {
			o.Scopes = DefaultOIDCScopes()
		} else {
			o.Scopes = slices.Clone(o.Scopes)
		}
		o.GroupsClaim = orDefault(o.GroupsClaim, DefaultGroupsClaim)
		o.UsernameClaim = orDefault(o.UsernameClaim, DefaultUsernameClaim)
		out.OIDC = &o
	}
	if a.GitHub != nil {
		g := *a.GitHub
		out.GitHub = &g
	}
	if a.Roles != nil {
		out.Roles = make(map[string]RoleBinding, len(a.Roles))
		for name, b := range a.Roles {
			out.Roles[name] = RoleBinding{Groups: slices.Clone(b.Groups), Users: slices.Clone(b.Users)}
		}
	}
	return out
}

func (a Auth) validate() []error {
	var errs []error
	add := func(field string, err error) {
		if err != nil {
			errs = append(errs, fmt.Errorf("auth.%s: %w", field, err))
		}
	}
	addf := func(field, format string, args ...any) {
		add(field, fmt.Errorf(format, args...))
	}

	if a.OIDC != nil && a.GitHub != nil {
		addf("oidc, auth.github", "configure at most one sign-in provider")
	}
	if ttl := a.TTL(); ttl < MinSessionTTL || ttl > MaxSessionTTL {
		addf("session_ttl", "must be between %s and %s, got %s", MinSessionTTL, MaxSessionTTL, ttl)
	}
	if a.DefaultRole != "" && !slices.Contains(RoleNames(), a.DefaultRole) {
		addf("default_role", "unknown role %q (want %s)", a.DefaultRole, strings.Join(RoleNames(), ", "))
	}
	if !a.Enabled() && (a.DefaultRole != "" || len(a.Roles) > 0 || a.SessionTTL != nil) {
		addf("oidc, auth.github", "session_ttl, default_role and roles need a sign-in provider")
	}

	if a.OIDC != nil {
		o := a.OIDC
		add("oidc.issuer_url", validateIssuerURL(o.IssuerURL))
		add("oidc.client_id", validateClientID(o.ClientID))
		validateClientSecret(add, "oidc", o.ClientSecretEnv, o.ClientSecretFile)
		add("oidc.scopes", validateScopes(o.Scopes))
		if strings.TrimSpace(o.GroupsClaim) == "" {
			addf("oidc.groups_claim", "must not be empty")
		}
		if strings.TrimSpace(o.UsernameClaim) == "" {
			addf("oidc.username_claim", "must not be empty")
		}
	}
	if a.GitHub != nil {
		add("github.client_id", validateClientID(a.GitHub.ClientID))
		validateClientSecret(add, "github", a.GitHub.ClientSecretEnv, a.GitHub.ClientSecretFile)
	}

	for _, role := range slices.Sorted(maps.Keys(a.Roles)) {
		field := "roles." + role
		if !slices.Contains(RoleNames(), role) {
			addf(field, "unknown role %q (want %s)", role, strings.Join(RoleNames(), ", "))
			continue
		}
		binding := a.Roles[role]
		if len(binding.Groups) == 0 && len(binding.Users) == 0 {
			addf(field, "needs at least one group or user")
		}
		validateEntries(addf, field+".groups", binding.Groups, a.GitHub != nil, a.groupError)
		validateEntries(addf, field+".users", binding.Users, a.GitHub != nil, a.userError)
	}
	return errs
}

func (a Auth) groupError(entry string) error {
	if a.GitHub == nil {
		return nil
	}
	org, team, hasTeam := strings.Cut(entry, "/")
	if !isGitHubOwner(org) {
		return fmt.Errorf("%q must be an organization login or org/team-slug", entry)
	}
	if hasTeam && !isGitHubTeamSlug(team) {
		return fmt.Errorf("%q must be an organization login or org/team-slug", entry)
	}
	return nil
}

func (a Auth) userError(entry string) error {
	if a.GitHub != nil && !isGitHubOwner(entry) {
		return fmt.Errorf("%q must be a GitHub login", entry)
	}
	return nil
}

func validateEntries(addf func(field, format string, args ...any), field string, entries []string, fold bool, check func(string) error) {
	seen := make(map[string]bool, len(entries))
	for i, entry := range entries {
		f := fmt.Sprintf("%s[%d]", field, i)
		key := entry
		if fold {
			key = strings.ToLower(entry)
		}
		switch {
		case strings.TrimSpace(entry) == "":
			addf(f, "must not be empty")
		case strings.TrimSpace(entry) != entry:
			addf(f, "%q must not have surrounding whitespace", entry)
		case seen[key]:
			addf(f, "duplicate entry %q", entry)
		default:
			if err := check(entry); err != nil {
				addf(f, "%v", err)
			}
		}
		seen[key] = true
	}
}

func isGitHubTeamSlug(s string) bool {
	if s == "" || len(s) > maxGitHubTeamSlug {
		return false
	}
	for _, r := range s {
		if !isAlnum(r) && r != '-' && r != '_' {
			return false
		}
	}
	return true
}

func validateClientID(id string) error {
	switch {
	case id == "":
		return errors.New("is required")
	case strings.ContainsFunc(id, unicode.IsSpace):
		return fmt.Errorf("%q must not contain whitespace", id)
	}
	return nil
}

func validateClientSecret(add func(string, error), provider, env, file string) {
	switch {
	case (env == "") == (file == ""):
		add(provider+".client_secret_env, auth."+provider+".client_secret_file", errors.New("set exactly one"))
	case env != "":
		add(provider+".client_secret_env", validateEnvName(env))
	default:
		add(provider+".client_secret_file", validateSecretFile(file))
	}
}

func validateScopes(scopes []string) error {
	if !slices.Contains(scopes, "openid") {
		return errors.New("must include openid")
	}
	seen := make(map[string]bool, len(scopes))
	for _, s := range scopes {
		switch {
		case s == "" || strings.ContainsFunc(s, unicode.IsSpace):
			return fmt.Errorf("scope %q must be a non-empty word without whitespace", s)
		case seen[s]:
			return fmt.Errorf("duplicate scope %q", s)
		}
		seen[s] = true
	}
	return nil
}

func validateIssuerURL(raw string) error {
	if raw == "" {
		return errors.New("is required")
	}
	u, err := parseOrigin(raw)
	if err != nil {
		return err
	}
	if u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return errors.New("must not include a query string or fragment")
	}
	return nil
}

func ValidatePublicURL(raw string) error {
	u, err := parseOrigin(raw)
	if err != nil {
		return err
	}
	switch {
	case u.Path != "" && u.Path != "/":
		return fmt.Errorf("%q must not have a path; doorstep must be served from the root of its host", raw)
	case u.RawQuery != "" || u.ForceQuery || u.Fragment != "":
		return fmt.Errorf("%q must not include a query string or fragment", raw)
	}
	return nil
}

func parseOrigin(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, errors.New("is not a valid URL")
	}
	switch {
	case !u.IsAbs() || u.Opaque != "" || u.Host == "":
		return nil, fmt.Errorf("%q must be an absolute URL with a host", raw)
	case u.User != nil:
		return nil, fmt.Errorf("%q must not include user information", raw)
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !isLoopbackHost(u.Hostname()) {
			return nil, fmt.Errorf("%q must use https unless the host is loopback", raw)
		}
	default:
		return nil, fmt.Errorf("%q must use https", raw)
	}
	return u, nil
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (c Catalog) ValidateWith(cfg Config) error {
	if !c.Auth.Enabled() {
		return nil
	}
	if cfg.Server.PublicURL == "" {
		return errors.New("server.public_url: required when a sign-in provider is configured (--public-url, " + EnvPublicURL + ")")
	}
	if err := ValidatePublicURL(cfg.Server.PublicURL); err != nil {
		return fmt.Errorf("server.public_url: sign-in requires a stricter URL: %w", err)
	}
	return nil
}

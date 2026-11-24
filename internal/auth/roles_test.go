package auth

import (
	"testing"

	"github.com/sharpintelligence/doorstep/internal/core"
)

func TestRoleMapperResolveOIDC(t *testing.T) {
	t.Parallel()

	m := RoleMapper{
		Provider: ProviderOIDC,
		Bindings: map[core.Role]Binding{
			core.RoleAdmin:    {Users: []string{"sub-admin", "Boss@Example.com"}},
			core.RoleDeployer: {Groups: []string{"deployers"}},
			core.RoleViewer:   {Groups: []string{"staff"}},
		},
	}
	tests := []struct {
		name     string
		mapper   RoleMapper
		identity Identity
		want     core.Role
		wantOK   bool
	}{
		{name: "nothing matches", mapper: m, identity: Identity{Subject: "x", Groups: []string{"guests"}}},
		{name: "group", mapper: m, identity: Identity{Subject: "x", Groups: []string{"staff"}}, want: core.RoleViewer, wantOK: true},
		{name: "groups are case sensitive", mapper: m, identity: Identity{Subject: "x", Groups: []string{"Staff"}}},
		{name: "highest wins", mapper: m, identity: Identity{Subject: "x", Groups: []string{"staff", "deployers"}}, want: core.RoleDeployer, wantOK: true},
		{name: "subject", mapper: m, identity: Identity{Subject: "sub-admin", Groups: []string{"staff"}}, want: core.RoleAdmin, wantOK: true},
		{name: "subject is case sensitive", mapper: m, identity: Identity{Subject: "SUB-ADMIN"}},
		{name: "username is not a user key", mapper: m, identity: Identity{Subject: "x", Username: "sub-admin"}},
		{name: "verified email", mapper: m, identity: Identity{Subject: "x", Email: "boss@example.com", EmailVerified: true}, want: core.RoleAdmin, wantOK: true},
		{name: "unverified email", mapper: m, identity: Identity{Subject: "x", Email: "boss@example.com"}},
		{
			name:     "default role",
			mapper:   RoleMapper{Provider: ProviderOIDC, Default: core.RoleViewer, Bindings: m.Bindings},
			identity: Identity{Subject: "x"},
			want:     core.RoleViewer, wantOK: true,
		},
		{
			name:     "binding beats default",
			mapper:   RoleMapper{Provider: ProviderOIDC, Default: core.RoleViewer, Bindings: m.Bindings},
			identity: Identity{Subject: "x", Groups: []string{"deployers"}},
			want:     core.RoleDeployer, wantOK: true,
		},
		{name: "no bindings", mapper: RoleMapper{Provider: ProviderOIDC}, identity: Identity{Subject: "x", Groups: []string{"staff"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, ok := tt.mapper.Resolve(tt.identity)
			if got != tt.want || ok != tt.wantOK {
				t.Errorf("Resolve() = %q, %v, want %q, %v", got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

func TestRoleMapperResolveGitHub(t *testing.T) {
	t.Parallel()

	m := RoleMapper{
		Provider: ProviderGitHub,
		Bindings: map[core.Role]Binding{
			core.RoleAdmin:    {Users: []string{"id:583231"}},
			core.RoleDeployer: {Groups: []string{"example-org/platform"}},
			core.RoleViewer:   {Groups: []string{"example-org"}},
		},
	}
	tests := []struct {
		name     string
		identity Identity
		want     core.Role
		wantOK   bool
	}{
		{name: "numeric id", identity: Identity{Subject: "583231", Username: "octocat"}, want: core.RoleAdmin, wantOK: true},
		{name: "renamed login keeps access", identity: Identity{Subject: "583231", Username: "octocat-renamed"}, want: core.RoleAdmin, wantOK: true},
		{name: "reclaimed login gets nothing", identity: Identity{Subject: "999", Username: "octocat"}},
		{name: "bare id is not an entry", identity: Identity{Subject: "id:583231", Username: "x"}},
		{name: "team", identity: Identity{Username: "dev", Groups: []string{"Example-Org/Platform"}}, want: core.RoleDeployer, wantOK: true},
		{name: "org", identity: Identity{Username: "dev", Groups: []string{"EXAMPLE-ORG"}}, want: core.RoleViewer, wantOK: true},
		{name: "other team in org", identity: Identity{Username: "dev", Groups: []string{"example-org/other"}}},
		{name: "email ignored", identity: Identity{Username: "dev", Email: "octocat", EmailVerified: true}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, ok := m.Resolve(tt.identity)
			if got != tt.want || ok != tt.wantOK {
				t.Errorf("Resolve() = %q, %v, want %q, %v", got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

func TestRoleMapperGitHubLookups(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		bindings  map[core.Role]Binding
		wantOrgs  bool
		wantTeams bool
	}{
		{name: "users only", bindings: map[core.Role]Binding{core.RoleAdmin: {Users: []string{"id:1"}}}},
		{name: "orgs", bindings: map[core.Role]Binding{core.RoleViewer: {Groups: []string{"acme"}}}, wantOrgs: true},
		{name: "teams", bindings: map[core.Role]Binding{core.RoleViewer: {Groups: []string{"acme/ops"}}}, wantTeams: true},
		{name: "both", bindings: map[core.Role]Binding{core.RoleViewer: {Groups: []string{"acme"}}, core.RoleAdmin: {Groups: []string{"acme/ops"}}}, wantOrgs: true, wantTeams: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			orgs, teams := RoleMapper{Provider: ProviderGitHub, Bindings: tt.bindings}.GitHubLookups()
			if orgs != tt.wantOrgs || teams != tt.wantTeams {
				t.Errorf("GitHubLookups() = %v, %v, want %v, %v", orgs, teams, tt.wantOrgs, tt.wantTeams)
			}
		})
	}
}

func TestRoleMapperValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mapper RoleMapper
		ok     bool
	}{
		{name: "valid", mapper: RoleMapper{Provider: ProviderGitHub, Default: core.RoleViewer, Bindings: map[core.Role]Binding{core.RoleAdmin: {}}}, ok: true},
		{name: "unknown provider", mapper: RoleMapper{Provider: "saml"}},
		{name: "unknown default", mapper: RoleMapper{Provider: ProviderOIDC, Default: "root"}},
		{name: "unknown binding", mapper: RoleMapper{Provider: ProviderOIDC, Bindings: map[core.Role]Binding{"root": {}}}},
		{name: "github user ids", mapper: RoleMapper{Provider: ProviderGitHub, Bindings: map[core.Role]Binding{core.RoleAdmin: {Users: []string{"id:583231", "id:1"}}}}, ok: true},
		{name: "github login", mapper: RoleMapper{Provider: ProviderGitHub, Bindings: map[core.Role]Binding{core.RoleAdmin: {Users: []string{"octocat"}}}}},
		{name: "github id not numeric", mapper: RoleMapper{Provider: ProviderGitHub, Bindings: map[core.Role]Binding{core.RoleAdmin: {Users: []string{"id:12a"}}}}},
		{name: "github empty id", mapper: RoleMapper{Provider: ProviderGitHub, Bindings: map[core.Role]Binding{core.RoleAdmin: {Users: []string{"id:"}}}}},
		{name: "github id too long", mapper: RoleMapper{Provider: ProviderGitHub, Bindings: map[core.Role]Binding{core.RoleAdmin: {Users: []string{"id:12345678901234567890"}}}}},
		{name: "oidc users are free form", mapper: RoleMapper{Provider: ProviderOIDC, Bindings: map[core.Role]Binding{core.RoleAdmin: {Users: []string{"octocat"}}}}, ok: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if err := tt.mapper.Validate(); (err == nil) != tt.ok {
				t.Errorf("Validate() = %v, want ok %v", err, tt.ok)
			}
		})
	}
}

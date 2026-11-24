package config

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func parseCatalog(t *testing.T, yaml string) Catalog {
	t.Helper()
	f, err := Parse(strings.NewReader(yaml))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return f.Catalog()
}

func TestAuthValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		yaml    string
		wantErr []string
	}{
		{name: "no auth section"},
		{
			name: "oidc minimal",
			yaml: `
auth:
  oidc:
    issuer_url: https://id.example.com/application/o/doorstep/
    client_id: doorstep
    client_secret_env: OIDC_SECRET
`,
		},
		{
			name: "oidc full",
			yaml: `
auth:
  session_ttl: 8h
  default_role: viewer
  oidc:
    issuer_url: http://127.0.0.1:5556/dex
    client_id: doorstep
    client_secret_file: /run/secrets/oidc
    scopes: [openid, email, groups]
    groups_claim: roles
    username_claim: email
  roles:
    admin:
      users: [d3c1f0b4-sub, Admin@Example.com]
    deployer:
      groups: [Deployers, deployers]
`,
		},
		{
			name: "github",
			yaml: `
auth:
  github:
    client_id: Iv1.abc123
    client_secret_env: GITHUB_CLIENT_SECRET
  roles:
    admin:
      users: [octocat]
    deployer:
      groups: [example-org/platform-team, example-org/ops_team]
    viewer:
      groups: [example-org]
`,
		},
		{
			name: "both providers",
			yaml: `
auth:
  oidc: {issuer_url: "https://id.example.com", client_id: a, client_secret_env: A}
  github: {client_id: b, client_secret_env: B}
`,
			wantErr: []string{"auth.oidc, auth.github: configure at most one sign-in provider"},
		},
		{
			name: "settings without provider",
			yaml: `
auth:
  default_role: viewer
`,
			wantErr: []string{"need a sign-in provider"},
		},
		{
			name: "roles without provider",
			yaml: `
auth:
  roles:
    admin:
      users: [octocat]
`,
			wantErr: []string{"need a sign-in provider"},
		},
		{
			name: "session ttl bounds",
			yaml: `
auth:
  session_ttl: 1m
  github: {client_id: b, client_secret_env: B}
`,
			wantErr: []string{"auth.session_ttl: must be between 5m0s and 720h0m0s, got 1m0s"},
		},
		{
			name: "session ttl too long",
			yaml: `
auth:
  session_ttl: 721h
  github: {client_id: b, client_secret_env: B}
`,
			wantErr: []string{"auth.session_ttl"},
		},
		{
			name: "unknown default role",
			yaml: `
auth:
  default_role: root
  github: {client_id: b, client_secret_env: B}
`,
			wantErr: []string{`auth.default_role: unknown role "root" (want viewer, deployer, admin)`},
		},
		{
			name: "oidc missing fields",
			yaml: `
auth:
  oidc:
    scopes: [profile]
    groups_claim: " "
`,
			wantErr: []string{
				"auth.oidc.issuer_url: is required",
				"auth.oidc.client_id: is required",
				"auth.oidc.client_secret_env, auth.oidc.client_secret_file: set exactly one",
				"auth.oidc.scopes: must include openid",
				"auth.oidc.groups_claim: must not be empty",
			},
		},
		{
			name: "oidc bad values",
			yaml: `
auth:
  oidc:
    issuer_url: http://id.example.com
    client_id: "two words"
    client_secret_env: A
    client_secret_file: relative/path
    scopes: [openid, openid]
`,
			wantErr: []string{
				"auth.oidc.issuer_url",
				"must use https unless the host is loopback",
				`auth.oidc.client_id: "two words" must not contain whitespace`,
				"auth.oidc.client_secret_env, auth.oidc.client_secret_file: set exactly one",
				`auth.oidc.scopes: duplicate scope "openid"`,
			},
		},
		{
			name: "oidc issuer with query",
			yaml: `
auth:
  oidc: {issuer_url: "https://id.example.com/?tenant=x", client_id: a, client_secret_file: relative}
`,
			wantErr: []string{"auth.oidc.issuer_url: must not include a query string or fragment", `auth.oidc.client_secret_file: "relative" must be an absolute path`},
		},
		{
			name: "oidc bad secret env name",
			yaml: `
auth:
  oidc: {issuer_url: "https://id.example.com", client_id: a, client_secret_env: "1BAD"}
`,
			wantErr: []string{`auth.oidc.client_secret_env: "1BAD" is not a valid environment variable name`},
		},
		{
			name: "github missing fields",
			yaml: `
auth:
  github: {}
`,
			wantErr: []string{"auth.github.client_id: is required", "auth.github.client_secret_env, auth.github.client_secret_file: set exactly one"},
		},
		{
			name: "bad role bindings",
			yaml: `
auth:
  github: {client_id: b, client_secret_env: B}
  roles:
    root:
      users: [octocat]
    admin: {}
    deployer:
      users: ["", " octocat", octocat, OctoCat, "not a login"]
      groups: [-bad, "org/", "org/team/extra", "org/te am"]
`,
			wantErr: []string{
				`auth.roles.root: unknown role "root"`,
				"auth.roles.admin: needs at least one group or user",
				"auth.roles.deployer.users[0]: must not be empty",
				`auth.roles.deployer.users[1]: " octocat" must not have surrounding whitespace`,
				`auth.roles.deployer.users[3]: duplicate entry "OctoCat"`,
				`auth.roles.deployer.users[4]: "not a login" must be a GitHub login`,
				`auth.roles.deployer.groups[0]: "-bad" must be an organization login or org/team-slug`,
				`auth.roles.deployer.groups[1]: "org/"`,
				`auth.roles.deployer.groups[2]: "org/team/extra"`,
				`auth.roles.deployer.groups[3]: "org/te am"`,
			},
		},
		{
			name: "oidc groups are case sensitive",
			yaml: `
auth:
  oidc: {issuer_url: "https://id.example.com", client_id: a, client_secret_env: A}
  roles:
    viewer:
      groups: [Ops, ops, ops]
`,
			wantErr: []string{`auth.roles.viewer.groups[2]: duplicate entry "ops"`},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assertErrorContains(t, parseCatalog(t, tt.yaml).Validate(), tt.wantErr)
		})
	}
}

func TestAuthDefaults(t *testing.T) {
	t.Parallel()

	c := parseCatalog(t, `
auth:
  oidc:
    issuer_url: https://id.example.com
    client_id: doorstep
    client_secret_env: OIDC_SECRET
`)
	want := OIDC{
		IssuerURL:       "https://id.example.com",
		ClientID:        "doorstep",
		ClientSecretEnv: "OIDC_SECRET",
		Scopes:          []string{"openid", "profile", "email", "groups"},
		GroupsClaim:     "groups",
		UsernameClaim:   "preferred_username",
	}
	if c.Auth.OIDC == nil || !reflect.DeepEqual(*c.Auth.OIDC, want) {
		t.Errorf("OIDC = %+v, want %+v", c.Auth.OIDC, want)
	}
	if got := c.Auth.TTL(); got != 12*time.Hour {
		t.Errorf("TTL() = %v, want 12h", got)
	}
	if c.Auth.Provider() != ProviderOIDC || !c.Auth.Enabled() {
		t.Errorf("Provider() = %q, Enabled() = %v", c.Auth.Provider(), c.Auth.Enabled())
	}
	if (Auth{}).Provider() != "" || (Auth{}).Enabled() {
		t.Error("empty auth reports a provider")
	}
	if p := (Auth{GitHub: &GitHubLogin{}}).Provider(); p != ProviderGitHub {
		t.Errorf("github Provider() = %q", p)
	}

	custom := parseCatalog(t, `
auth:
  session_ttl: 30m
  oidc:
    issuer_url: https://id.example.com
    client_id: doorstep
    client_secret_env: OIDC_SECRET
    scopes: [openid]
`)
	if got := custom.Auth.TTL(); got != 30*time.Minute {
		t.Errorf("TTL() = %v, want 30m", got)
	}
	if got := custom.Auth.OIDC.Scopes; !reflect.DeepEqual(got, []string{"openid"}) {
		t.Errorf("Scopes = %v", got)
	}
}

func TestAuthCatalogDoesNotAlias(t *testing.T) {
	t.Parallel()

	f, err := Parse(strings.NewReader(`
auth:
  oidc: {issuer_url: "https://id.example.com", client_id: a, client_secret_env: A, scopes: [openid]}
  roles:
    admin:
      users: [alice]
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	c := f.Catalog()
	c.Auth.OIDC.ClientID = "changed"
	c.Auth.OIDC.Scopes[0] = "changed"
	c.Auth.Roles["admin"].Users[0] = "mallory"
	if f.Auth.OIDC.ClientID != "a" || f.Auth.OIDC.Scopes[0] != "openid" || f.Auth.Roles["admin"].Users[0] != "alice" {
		t.Errorf("catalog aliases the parsed file: %+v", f.Auth)
	}
}

func TestAuthClientSecret(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		auth   Auth
		want   SecretRef
		wantOK bool
	}{
		{name: "none", auth: Auth{}},
		{
			name:   "oidc file",
			auth:   Auth{OIDC: &OIDC{ClientSecretFile: "/run/secrets/oidc"}},
			want:   SecretRef{Label: "auth.oidc client secret", File: "/run/secrets/oidc"},
			wantOK: true,
		},
		{
			name:   "github env",
			auth:   Auth{GitHub: &GitHubLogin{ClientSecretEnv: "GH_SECRET"}},
			want:   SecretRef{Label: "auth.github client secret", Env: "GH_SECRET"},
			wantOK: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, ok := tt.auth.ClientSecret()
			if got != tt.want || ok != tt.wantOK {
				t.Errorf("ClientSecret() = %+v, %v, want %+v, %v", got, ok, tt.want, tt.wantOK)
			}
			refs := Catalog{Auth: tt.auth}.SecretRefs()
			if tt.wantOK != (len(refs) == 1 && refs[0] == tt.want) {
				t.Errorf("SecretRefs() = %+v", refs)
			}
		})
	}
}

func TestCatalogCheckSecretsCoversAuth(t *testing.T) {
	t.Parallel()

	c := Catalog{Auth: Auth{OIDC: &OIDC{ClientSecretEnv: "OIDC_SECRET"}}}
	assertErrorContains(t, c.CheckSecrets(envFrom(nil), filesFrom(nil)), []string{"auth.oidc client secret: environment variable OIDC_SECRET is not set"})
	assertErrorContains(t, c.CheckSecrets(envFrom(map[string]string{"OIDC_SECRET": "s"}), filesFrom(nil)), nil)
}

func TestCatalogValidateWith(t *testing.T) {
	t.Parallel()

	withProvider := Catalog{Auth: Auth{GitHub: &GitHubLogin{}}}
	tests := []struct {
		name    string
		catalog Catalog
		public  string
		wantErr []string
	}{
		{name: "no provider, no public url", catalog: Catalog{}},
		{name: "provider and public url", catalog: withProvider, public: "https://deploy.example.com"},
		{name: "provider with loopback http public url", catalog: withProvider, public: "http://localhost:8080"},
		{name: "provider with plain http public url", catalog: withProvider, public: "http://deploy.example.com", wantErr: []string{"server.public_url", "must use https unless the host is loopback"}},
		{name: "provider with public url path", catalog: withProvider, public: "https://example.com/doorstep", wantErr: []string{"server.public_url", "must not have a path"}},
		{name: "provider with public url query", catalog: withProvider, public: "https://example.com/?a=b", wantErr: []string{"server.public_url", "query string or fragment"}},
		{name: "provider with public url credentials", catalog: withProvider, public: "https://user:pw@example.com", wantErr: []string{"server.public_url", "user information"}},
		{name: "provider without public url", catalog: withProvider, wantErr: []string{"server.public_url: required when a sign-in provider is configured", EnvPublicURL}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := Default()
			cfg.Server.PublicURL = tt.public
			assertErrorContains(t, tt.catalog.ValidateWith(cfg), tt.wantErr)
		})
	}
}

func TestParseRejectsUnknownAuthKeys(t *testing.T) {
	t.Parallel()

	for _, yaml := range []string{
		"auth:\n  oidc:\n    issuer: https://x\n",
		"auth:\n  roles:\n    admin:\n      members: [a]\n",
		"auth:\n  github:\n    client_secret: inline\n",
		"auth:\n  session_ttl: 3600\n",
	} {
		if _, err := Parse(strings.NewReader(yaml)); err == nil {
			t.Errorf("Parse accepted %q", yaml)
		}
	}
}

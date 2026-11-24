package config

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestMigrationsTrailerEnabled(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   *bool
		want bool
	}{
		{name: "omitted", want: true},
		{name: "true", in: new(true), want: true},
		{name: "false", in: new(false), want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := (Migrations{Trailer: tt.in}).TrailerEnabled(); got != tt.want {
				t.Errorf("TrailerEnabled() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestGateWebhookRefs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		gate       Gate
		endpoint   SecretRef
		hasWebhook bool
		secret     SecretRef
		hasSecret  bool
		secretRefs int
	}{
		{name: "no webhook", gate: Gate{Type: GateChecks, Name: "checks"}},
		{
			name:       "inline url without secret",
			gate:       Gate{Type: GateBackup, Name: "backup", Webhook: &GateWebhook{URL: "https://b.example"}},
			endpoint:   SecretRef{Label: "app blog gate backup url", Value: "https://b.example"},
			hasWebhook: true,
			secret:     SecretRef{Label: "app blog gate backup secret"},
		},
		{
			name:       "url env with secret env",
			gate:       Gate{Type: GateBackup, Name: "snapshot", Webhook: &GateWebhook{URLEnv: "BACKUP_URL", SecretEnv: "BACKUP_SECRET"}},
			endpoint:   SecretRef{Label: "app blog gate snapshot url", Env: "BACKUP_URL"},
			hasWebhook: true,
			secret:     SecretRef{Label: "app blog gate snapshot secret", Env: "BACKUP_SECRET"},
			hasSecret:  true,
			secretRefs: 2,
		},
		{
			name:       "inline url with secret file",
			gate:       Gate{Type: GateBackup, Name: "backup", Webhook: &GateWebhook{URL: "https://b.example", SecretFile: "/s"}},
			endpoint:   SecretRef{Label: "app blog gate backup url", Value: "https://b.example"},
			hasWebhook: true,
			secret:     SecretRef{Label: "app blog gate backup secret", File: "/s"},
			hasSecret:  true,
			secretRefs: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			endpoint, ok := tt.gate.WebhookEndpoint("blog")
			if endpoint != tt.endpoint || ok != tt.hasWebhook {
				t.Errorf("WebhookEndpoint() = %+v, %t; want %+v, %t", endpoint, ok, tt.endpoint, tt.hasWebhook)
			}
			secret, ok := tt.gate.WebhookSecret("blog")
			if (ok && secret != tt.secret) || ok != tt.hasSecret {
				t.Errorf("WebhookSecret() = %+v, %t; want %+v, %t", secret, ok, tt.secret, tt.hasSecret)
			}
			c := Catalog{Apps: []App{{Name: "blog", Gates: Gates{Before: []Gate{tt.gate}}}}}
			if refs := c.SecretRefs(); len(refs) != tt.secretRefs {
				t.Errorf("SecretRefs() = %+v, want %d refs", refs, tt.secretRefs)
			}
		})
	}
}

func TestCheckSecretsCoversBackupGates(t *testing.T) {
	t.Parallel()

	c := parseCatalog(t, `
apps:
  - name: blog
    source: {repo: example-user/blog}
    image: {repository: ghcr.io/example-user/blog}
    gitops: {repo: example-user/homelab, path: apps/blog}
    gates:
      before:
        - type: checks
        - type: backup
          webhook: {url_env: BACKUP_URL, secret_file: /run/secrets/backup}
`)
	err := c.CheckSecrets(envFrom(nil), func(string) ([]byte, error) { return nil, errors.New("missing") })
	assertErrorContains(t, err, []string{"app blog gate backup url: environment variable BACKUP_URL is not set", "app blog gate backup secret: missing"})

	err = c.CheckSecrets(envFrom(map[string]string{"BACKUP_URL": "https://b.example"}), func(string) ([]byte, error) { return []byte("s3cret\n"), nil })
	if err != nil {
		t.Errorf("CheckSecrets() = %v", err)
	}
}

func TestParseMigrationsAndBackupGate(t *testing.T) {
	t.Parallel()

	c := parseCatalog(t, `
apps:
  - name: blog
    source: {repo: example-user/blog}
    image: {repository: ghcr.io/example-user/blog}
    gitops: {repo: example-user/homelab, path: apps/blog}
    migrations:
      labels: [db-migration]
    gates:
      before:
        - type: checks
        - type: backup
          name: snapshot
          when: always
          apply_to_rollbacks: true
          timeout: 45m
          webhook:
            url: https://backup.example.com/doorstep
            secret_env: BACKUP_SECRET
`)
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate() = %v", err)
	}
	app := c.Apps[0]
	if !reflect.DeepEqual(app.Migrations, &Migrations{Labels: []string{"db-migration"}, Trailer: new(true)}) {
		t.Errorf("Migrations = %+v", app.Migrations)
	}
	want := Gate{
		Type: GateBackup, Name: "snapshot", When: GateWhenAlways, ApplyToRollbacks: true, Timeout: durPtr(45 * time.Minute),
		Webhook: &GateWebhook{URL: "https://backup.example.com/doorstep", SecretEnv: "BACKUP_SECRET"},
	}
	if !reflect.DeepEqual(app.Gates.Before[1], want) {
		t.Errorf("backup gate = %+v, want %+v", app.Gates.Before[1], want)
	}
}

func TestParseRejectsUnknownMigrationAndWebhookKeys(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		yaml string
		want string
	}{
		{name: "migrations", yaml: "migrations: {label: [db]}", want: "label"},
		{name: "webhook", yaml: "gates: {before: [{type: backup, webhook: {uri: https://x.example}}]}", want: "uri"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			doc := "apps:\n  - name: blog\n    " + tt.yaml + "\n"
			_, err := Parse(strings.NewReader(doc))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Parse() error = %v, want it to mention %q", err, tt.want)
			}
		})
	}
}

func TestBackupGateIsBeforePhaseWithTimeout(t *testing.T) {
	t.Parallel()

	spec, ok := gateSpecs()[GateBackup]
	if !ok || spec.phase != PhaseBefore || !spec.allowsTimeout || !spec.allowsRollbackOptIn || !spec.webhook {
		t.Fatalf("backup gate spec = %+v, %v", spec, ok)
	}
	if got := spec.defaultTimeout(Gate{}); got != DefaultBackupTimeout {
		t.Errorf("default timeout = %s, want %s", got, DefaultBackupTimeout)
	}
	for gateType, s := range gateSpecs() {
		if s.webhook != (gateType == GateBackup) {
			t.Errorf("gate type %s webhook = %v", gateType, s.webhook)
		}
	}
}

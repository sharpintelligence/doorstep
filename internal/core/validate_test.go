package core

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

const (
	validSHA    = "0123456789abcdef0123456789abcdef01234567"
	validDigest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
)

func TestParseDigest(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		in      string
		wantErr bool
	}{
		{name: "sha256", in: validDigest},
		{name: "empty", in: "", wantErr: true},
		{name: "tag instead of digest", in: "v1.2.3", wantErr: true},
		{name: "commit SHA tag", in: validSHA, wantErr: true},
		{name: "missing algorithm", in: strings.TrimPrefix(validDigest, "sha256:"), wantErr: true},
		{name: "uppercase hex", in: strings.ToUpper(validDigest[:10]) + validDigest[10:], wantErr: true},
		{name: "uppercase algorithm", in: "SHA256:" + strings.TrimPrefix(validDigest, "sha256:"), wantErr: true},
		{name: "short", in: validDigest[:len(validDigest)-1], wantErr: true},
		{name: "long", in: validDigest + "0", wantErr: true},
		{name: "non hex", in: validDigest[:len(validDigest)-1] + "g", wantErr: true},
		{name: "sha512", in: "sha512:" + strings.Repeat("a", 128), wantErr: true},
		{name: "image reference with digest", in: "ghcr.io/acme/app@" + validDigest, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := ParseDigest(tt.in)
			if tt.wantErr {
				if !errors.Is(err, ErrInvalidDigest) {
					t.Errorf("ParseDigest(%q) error = %v, want ErrInvalidDigest", tt.in, err)
				}
				if Digest(tt.in).Validate() == nil {
					t.Errorf("Digest(%q).Validate() = nil", tt.in)
				}
				return
			}
			if err != nil || got.String() != tt.in {
				t.Errorf("ParseDigest(%q) = %q, %v", tt.in, got, err)
			}
			if err := got.Validate(); err != nil {
				t.Errorf("Validate() = %v", err)
			}
		})
	}
}

func TestCommitValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		commit  Commit
		wantErr []string
		wantIs  error
	}{
		{name: "valid", commit: Commit{Repository: "acme/app", SHA: validSHA}},
		{name: "short SHA", commit: Commit{Repository: "acme/app", SHA: validSHA[:7]}, wantErr: []string{"40 lowercase hex"}, wantIs: ErrInvalidCommitSHA},
		{name: "uppercase SHA", commit: Commit{Repository: "acme/app", SHA: strings.ToUpper(validSHA)}, wantErr: []string{"commit SHA"}, wantIs: ErrInvalidCommitSHA},
		{name: "non hex SHA", commit: Commit{Repository: "acme/app", SHA: validSHA[:39] + "z"}, wantIs: ErrInvalidCommitSHA},
		{name: "branch name", commit: Commit{Repository: "acme/app", SHA: "main"}, wantIs: ErrInvalidCommitSHA},
		{name: "missing repository", commit: Commit{SHA: validSHA}, wantErr: []string{"repository"}},
		{name: "blank repository", commit: Commit{Repository: "  ", SHA: validSHA}, wantErr: []string{"repository"}},
		{name: "everything missing", commit: Commit{}, wantErr: []string{"repository", "commit SHA"}, wantIs: ErrInvalidCommitSHA},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assertError(t, tt.commit.Validate(), tt.wantErr, tt.wantIs)
		})
	}
}

func TestTarget(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		target  Target
		want    string
		wantErr []string
	}{
		{name: "valid", target: Target{App: "web", Environment: "prod"}, want: "web/prod"},
		{name: "missing app", target: Target{Environment: "prod"}, want: "/prod", wantErr: []string{"app"}},
		{name: "blank environment", target: Target{App: "web", Environment: " "}, want: "web/ ", wantErr: []string{"environment"}},
		{name: "empty", target: Target{}, want: "/", wantErr: []string{"app", "environment"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.target.String(); got != tt.want {
				t.Errorf("String() = %q, want %q", got, tt.want)
			}
			assertError(t, tt.target.Validate(), tt.wantErr, nil)
		})
	}
}

func TestLockScope(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		lock Lock
		want string
	}{
		{name: "app", lock: Lock{Target: Target{App: "web", Environment: DefaultEnvironment}}, want: "web"},
		{name: "global", lock: Lock{Global: true}, want: GlobalScope},
		{name: "global ignores target", lock: Lock{Global: true, Target: Target{App: "web"}}, want: GlobalScope},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.lock.Scope(); got != tt.want {
				t.Errorf("Scope() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestImageRef(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		ref     ImageRef
		want    string
		wantErr []string
	}{
		{name: "valid", ref: ImageRef{Repository: "ghcr.io/acme/web", Tag: validSHA}, want: "ghcr.io/acme/web:" + validSHA},
		{name: "registry with port", ref: ImageRef{Repository: "registry.local:5000/web", Tag: "v1"}, want: "registry.local:5000/web:v1"},
		{name: "missing tag", ref: ImageRef{Repository: "ghcr.io/acme/web"}, want: "ghcr.io/acme/web:", wantErr: []string{"tag"}},
		{name: "missing repository", ref: ImageRef{Tag: "v1"}, want: ":v1", wantErr: []string{"repository"}},
		{name: "empty", ref: ImageRef{}, want: ":", wantErr: []string{"repository", "tag"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.ref.String(); got != tt.want {
				t.Errorf("String() = %q, want %q", got, tt.want)
			}
			assertError(t, tt.ref.Validate(), tt.wantErr, nil)
		})
	}
}

func TestEnumValues(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		values  []string
		valid   func(string) bool
		invalid []string
	}{
		{name: "kind", values: stringsOf(Kinds()), valid: func(s string) bool { return Kind(s).Valid() }, invalid: []string{"", "Deploy", "redeploy"}},
		{name: "source", values: stringsOf(Sources()), valid: func(s string) bool { return Source(s).Valid() }, invalid: []string{"", "cron", "API"}},
		{name: "lock kind", values: stringsOf(LockKinds()), valid: func(s string) bool { return LockKind(s).Valid() }, invalid: []string{"", "manual"}},
		{name: "gate status", values: stringsOf(GateStatuses()), valid: func(s string) bool { return GateStatus(s).Valid() }, invalid: []string{"", "running"}},
		{name: "event type", values: stringsOf(EventTypes()), valid: func(s string) bool { return EventType(s).Valid() }, invalid: []string{"", "comment"}},
		{name: "lock action", values: stringsOf(LockActions()), valid: func(s string) bool { return LockAction(s).Valid() }, invalid: []string{"", "Locked", "stolen"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			seen := map[string]bool{}
			for _, v := range tt.values {
				if seen[v] {
					t.Errorf("duplicate value %q", v)
				}
				seen[v] = true
				if !tt.valid(v) {
					t.Errorf("%q is not valid", v)
				}
			}
			for _, v := range tt.invalid {
				if tt.valid(v) {
					t.Errorf("%q is valid", v)
				}
			}
		})
	}
}

func TestDeploymentValidate(t *testing.T) {
	t.Parallel()

	parent := DeploymentID(7)
	valid := func() Deployment {
		return Deployment{
			Target: Target{App: "web", Environment: "prod"},
			Kind:   KindDeploy,
			State:  StateQueued,
			Source: SourceWebhook,
			Commit: Commit{Repository: "acme/web", SHA: validSHA},
			Image:  ImageRef{Repository: "ghcr.io/acme/web", Tag: validSHA},
		}
	}
	tests := []struct {
		name    string
		mutate  func(*Deployment)
		wantErr []string
		wantIs  error
	}{
		{name: "minimal", mutate: func(*Deployment) {}},
		{name: "with digests and pin", mutate: func(d *Deployment) {
			d.Digest = validDigest
			d.PreviousDigest = validDigest
			d.PinCommitSHA = validSHA
		}},
		{name: "rollback", mutate: func(d *Deployment) {
			d.Kind = KindRollback
			d.RollbackOfID = &parent
		}},
		{name: "rollback without origin", mutate: func(d *Deployment) { d.Kind = KindRollback }, wantErr: []string{"rolls back"}},
		{name: "deploy with rollback origin", mutate: func(d *Deployment) { d.RollbackOfID = &parent }, wantErr: []string{"only rollback"}},
		{name: "unknown kind", mutate: func(d *Deployment) { d.Kind = "hotfix" }, wantErr: []string{`kind "hotfix"`}},
		{name: "unknown state", mutate: func(d *Deployment) { d.State = "paused" }, wantErr: []string{`"paused"`}, wantIs: ErrUnknownState},
		{name: "unknown source", mutate: func(d *Deployment) { d.Source = "cron" }, wantErr: []string{`source "cron"`}},
		{name: "tag as digest", mutate: func(d *Deployment) { d.Digest = "latest" }, wantIs: ErrInvalidDigest},
		{name: "bad previous digest", mutate: func(d *Deployment) { d.PreviousDigest = "sha256:abc" }, wantErr: []string{"previous digest"}, wantIs: ErrInvalidDigest},
		{name: "short pin commit", mutate: func(d *Deployment) { d.PinCommitSHA = "abc1234" }, wantErr: []string{"pin commit"}, wantIs: ErrInvalidCommitSHA},
		{name: "short source commit", mutate: func(d *Deployment) { d.Commit.SHA = "abc1234" }, wantIs: ErrInvalidCommitSHA},
		{name: "migration with reason", mutate: func(d *Deployment) {
			d.Migration = true
			d.MigrationReason = "label db-migration on #12 (0123456)"
		}},
		{name: "migration without reason", mutate: func(d *Deployment) { d.Migration = true }},
		{name: "reason without migration", mutate: func(d *Deployment) { d.MigrationReason = "x" }, wantErr: []string{"migration reason must be empty"}},
		{name: "migration reason too long", mutate: func(d *Deployment) {
			d.Migration = true
			d.MigrationReason = strings.Repeat("é", MaxMigrationReasonLength+1)
		}, wantErr: []string{"at most 200"}},
		{name: "migration reason at limit", mutate: func(d *Deployment) {
			d.Migration = true
			d.MigrationReason = strings.Repeat("é", MaxMigrationReasonLength)
		}},
		{name: "migration reason with control", mutate: func(d *Deployment) {
			d.Migration = true
			d.MigrationReason = "a\nb"
		}, wantErr: []string{"control characters"}},
		{name: "migration reason invalid UTF-8", mutate: func(d *Deployment) {
			d.Migration = true
			d.MigrationReason = "a\xffb"
		}, wantErr: []string{"UTF-8"}},
		{name: "missing target and image", mutate: func(d *Deployment) {
			d.Target = Target{}
			d.Image = ImageRef{}
		}, wantErr: []string{"target app", "target environment", "image repository", "image tag"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			d := valid()
			tt.mutate(&d)
			assertError(t, d.Validate(), tt.wantErr, tt.wantIs)
		})
	}
}

func stringsOf[T ~string](values []T) []string {
	out := make([]string, len(values))
	for i, v := range values {
		out[i] = string(v)
	}
	return out
}

func assertError(t *testing.T, err error, contains []string, is error) {
	t.Helper()
	if len(contains) == 0 && is == nil {
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		return
	}
	if err == nil {
		t.Fatalf("expected error containing %q, got nil", contains)
	}
	for _, s := range contains {
		if !strings.Contains(err.Error(), s) {
			t.Errorf("error %q does not contain %q", err, s)
		}
	}
	if is != nil && !errors.Is(err, is) {
		t.Errorf("error %v does not wrap %v", err, is)
	}
}

func TestLockHeldError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		lock Lock
		want string
	}{
		{name: "app", lock: Lock{Target: Target{App: "web", Environment: DefaultEnvironment}, Owner: "alice", Reason: "migration"}, want: "web is already locked by alice: migration"},
		{name: "global", lock: Lock{Global: true, Owner: "bob", Reason: "holiday"}, want: "global is already locked by bob: holiday"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var err error = &LockHeldError{Lock: tt.lock}
			if err.Error() != tt.want {
				t.Errorf("Error() = %q, want %q", err.Error(), tt.want)
			}
			if !errors.Is(err, ErrLockHeld) {
				t.Errorf("%v does not wrap ErrLockHeld", err)
			}
			held, ok := errors.AsType[*LockHeldError](fmt.Errorf("wrapped: %w", err))
			if !ok || held.Lock.Owner != tt.lock.Owner {
				t.Errorf("AsType = %+v, %v", held, ok)
			}
		})
	}
}

func TestLockRequestScope(t *testing.T) {
	t.Parallel()

	target := Target{App: "web", Environment: DefaultEnvironment}
	if got := (LockRequest{Target: &target}).Scope(); got != "web" {
		t.Errorf("app Scope() = %q", got)
	}
	if got := (LockRequest{}).Scope(); got != GlobalScope {
		t.Errorf("global Scope() = %q", got)
	}
}

package core

import "testing"

func TestTrackedCommitValidate(t *testing.T) {
	t.Parallel()

	valid := func() TrackedCommit {
		return TrackedCommit{
			Target:      Target{App: "blog", Environment: "prod"},
			Commit:      Commit{Repository: "acme/blog", SHA: validSHA},
			Branch:      "main",
			Source:      SourceWebhook,
			ChecksState: ChecksPending,
		}
	}
	tests := []struct {
		name    string
		mutate  func(*TrackedCommit)
		wantErr []string
		wantIs  error
	}{
		{name: "minimal", mutate: func(*TrackedCommit) {}},
		{name: "every checks state", mutate: func(c *TrackedCommit) { c.ChecksState = ChecksFailure }},
		{name: "missing branch", mutate: func(c *TrackedCommit) { c.Branch = " " }, wantErr: []string{"branch"}},
		{name: "unknown source", mutate: func(c *TrackedCommit) { c.Source = "cron" }, wantErr: []string{`source "cron"`}},
		{name: "empty checks state", mutate: func(c *TrackedCommit) { c.ChecksState = "" }, wantErr: []string{`checks state ""`}},
		{name: "unknown checks state", mutate: func(c *TrackedCommit) { c.ChecksState = "neutral" }, wantErr: []string{`checks state "neutral"`}},
		{name: "short SHA", mutate: func(c *TrackedCommit) { c.Commit.SHA = "abc1234" }, wantIs: ErrInvalidCommitSHA},
		{name: "missing target", mutate: func(c *TrackedCommit) { c.Target = Target{} }, wantErr: []string{"target app", "target environment"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := valid()
			tt.mutate(&c)
			assertError(t, c.Validate(), tt.wantErr, tt.wantIs)
		})
	}
}

func TestTrackedCommitOpen(t *testing.T) {
	t.Parallel()

	deployment := DeploymentID(3)
	newer := TrackedCommitID(9)
	tests := []struct {
		name   string
		commit TrackedCommit
		want   bool
	}{
		{name: "new", commit: TrackedCommit{}, want: true},
		{name: "enqueued", commit: TrackedCommit{DeploymentID: &deployment}},
		{name: "superseded", commit: TrackedCommit{SupersededByID: &newer}},
		{name: "both", commit: TrackedCommit{DeploymentID: &deployment, SupersededByID: &newer}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.commit.Open(); got != tt.want {
				t.Errorf("Open() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestChecksEnums(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		values  []string
		valid   func(string) bool
		invalid []string
	}{
		{name: "checks state", values: stringsOf(ChecksStates()), valid: func(s string) bool { return ChecksState(s).Valid() }, invalid: []string{"", "neutral", "Success"}},
		{name: "check kind", values: stringsOf(CheckKinds()), valid: func(s string) bool { return CheckKind(s).Valid() }, invalid: []string{"", "run", "commit_status"}},
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

package intake_test

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/github"
	"github.com/sharpintelligence/doorstep/internal/intake"
)

func TestLiveChecks(t *testing.T) {
	t.Parallel()

	const (
		repo = "acme/web"
		sha  = "0123456789abcdef0123456789abcdef01234567"
	)
	readAt := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name      string
		snapshot  intake.Snapshot
		required  []string
		err       error
		wantState core.ChecksState
		wantNames []string
		wantQuery []string
	}{
		{
			name:      "all required checks green",
			snapshot:  intake.Snapshot{CheckRuns: []github.CheckRun{{ID: 1, Name: "test", Status: "completed", Conclusion: "success"}}},
			required:  []string{"test", "test"},
			wantState: core.ChecksSuccess,
			wantNames: []string{"test"},
			wantQuery: []string{"test"},
		},
		{
			name:      "required check not reported",
			snapshot:  intake.Snapshot{CheckRuns: []github.CheckRun{{ID: 1, Name: "lint", Status: "completed", Conclusion: "success"}}},
			required:  []string{"test"},
			wantState: core.ChecksPending,
			wantNames: []string{"test"},
			wantQuery: []string{"test"},
		},
		{
			name:      "failing status without required checks",
			snapshot:  intake.Snapshot{Statuses: []github.CommitStatus{{Context: "ci/build", State: "failure"}}},
			wantState: core.ChecksFailure,
			wantNames: []string{"ci/build"},
		},
		{
			name:      "nothing reported",
			wantState: core.ChecksPending,
		},
		{
			name: "source error",
			err:  errors.New("github unavailable"),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			source := newFakeChecks()
			source.set(repo, sha, tt.snapshot)
			if tt.err != nil {
				source.fail(tt.err)
			}
			live := intake.LiveChecks{Source: source, Now: func() time.Time { return readAt }}
			got, err := live.Checks(t.Context(), repo, sha, tt.required)
			if tt.err != nil {
				if !errors.Is(err, tt.err) {
					t.Fatalf("Checks() error = %v, want %v", err, tt.err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Checks(): %v", err)
			}
			if err := got.Validate(); err != nil {
				t.Errorf("evaluation invalid: %v", err)
			}
			if got.State != tt.wantState || !got.ReadAt.Equal(readAt) {
				t.Errorf("Checks() = %s at %s, want %s at %s", got.State, got.ReadAt, tt.wantState, readAt)
			}
			names := make([]string, 0, len(got.Checks))
			for _, c := range got.Checks {
				names = append(names, c.Name)
			}
			if !slices.Equal(names, tt.wantNames) {
				t.Errorf("check names = %v, want %v", names, tt.wantNames)
			}
			call := source.lastCall()
			if call.repository != repo || call.sha != sha || !slices.Equal(call.names, tt.wantQuery) {
				t.Errorf("source called with %+v, want %s@%s names %v", call, repo, sha, tt.wantQuery)
			}
		})
	}
}

func TestLiveChecksDefaults(t *testing.T) {
	t.Parallel()

	if _, err := (intake.LiveChecks{}).Checks(t.Context(), "acme/web", "abc", nil); err == nil {
		t.Error("LiveChecks without a source returned no error")
	}
	before := time.Now()
	got, err := intake.LiveChecks{Source: newFakeChecks()}.Checks(t.Context(), "acme/web", "abc", nil)
	if err != nil {
		t.Fatalf("Checks(): %v", err)
	}
	if got.ReadAt.Before(before) {
		t.Errorf("ReadAt = %s, want the wall clock (after %s)", got.ReadAt, before)
	}
}

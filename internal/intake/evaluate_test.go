package intake_test

import (
	"slices"
	"testing"

	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/github"
	"github.com/sharpintelligence/doorstep/internal/intake"
)

func run(id int64, name, status, conclusion string) github.CheckRun {
	return github.CheckRun{ID: id, Name: name, Status: status, Conclusion: conclusion}
}

func done(id int64, name, conclusion string) github.CheckRun {
	return run(id, name, "completed", conclusion)
}

func status(context, state string) github.CommitStatus {
	return github.CommitStatus{Context: context, State: state}
}

func suite(id int64, status string, runs int, slug string) github.CheckSuite {
	return github.CheckSuite{ID: id, Status: status, LatestCheckRunsCount: runs, App: github.AppRef{Slug: slug}}
}

func TestEvaluateCheckRunConclusions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		run        github.CheckRun
		want       core.ChecksState
		wantDetail string
	}{
		{run: run(1, "ci", "queued", ""), want: core.ChecksPending, wantDetail: "queued"},
		{run: run(1, "ci", "in_progress", ""), want: core.ChecksPending, wantDetail: "in_progress"},
		{run: run(1, "ci", "waiting", ""), want: core.ChecksPending, wantDetail: "waiting"},
		{run: run(1, "ci", "requested", ""), want: core.ChecksPending, wantDetail: "requested"},
		{run: run(1, "ci", "pending", ""), want: core.ChecksPending, wantDetail: "pending"},
		{run: run(1, "ci", "in_progress", "success"), want: core.ChecksPending, wantDetail: "in_progress"},
		{run: done(1, "ci", "success"), want: core.ChecksSuccess, wantDetail: "success"},
		{run: done(1, "ci", "neutral"), want: core.ChecksSuccess, wantDetail: "neutral"},
		{run: done(1, "ci", "skipped"), want: core.ChecksSuccess, wantDetail: "skipped"},
		{run: done(1, "ci", "failure"), want: core.ChecksFailure, wantDetail: "failure"},
		{run: done(1, "ci", "cancelled"), want: core.ChecksFailure, wantDetail: "cancelled"},
		{run: done(1, "ci", "timed_out"), want: core.ChecksFailure, wantDetail: "timed_out"},
		{run: done(1, "ci", "action_required"), want: core.ChecksFailure, wantDetail: "action_required"},
		{run: done(1, "ci", "stale"), want: core.ChecksFailure, wantDetail: "stale"},
		{run: done(1, "ci", "startup_failure"), want: core.ChecksFailure, wantDetail: "startup_failure"},
		{run: done(1, "ci", "Success"), want: core.ChecksFailure, wantDetail: "Success"},
		{run: done(1, "ci", ""), want: core.ChecksFailure, wantDetail: "no conclusion"},
	}
	for _, tt := range tests {
		t.Run(tt.run.Status+"/"+tt.run.Conclusion, func(t *testing.T) {
			t.Parallel()
			for _, required := range [][]string{nil, {"ci"}} {
				state, checks := intake.Evaluate(required, intake.Snapshot{CheckRuns: []github.CheckRun{tt.run}})
				want := []core.Check{{Name: "ci", Kind: core.CheckKindRun, State: tt.want, Detail: tt.wantDetail}}
				if state != tt.want || !slices.Equal(checks, want) {
					t.Errorf("Evaluate(%v) = %s, %+v, want %s, %+v", required, state, checks, tt.want, want)
				}
			}
		})
	}
}

func TestEvaluateStatusStates(t *testing.T) {
	t.Parallel()

	tests := []struct {
		state string
		want  core.ChecksState
	}{
		{state: "success", want: core.ChecksSuccess},
		{state: "pending", want: core.ChecksPending},
		{state: "failure", want: core.ChecksFailure},
		{state: "error", want: core.ChecksFailure},
		{state: "", want: core.ChecksFailure},
		{state: "SUCCESS", want: core.ChecksFailure},
	}
	for _, tt := range tests {
		t.Run(tt.state, func(t *testing.T) {
			t.Parallel()
			for _, required := range [][]string{nil, {"ci/build"}} {
				state, checks := intake.Evaluate(required, intake.Snapshot{Statuses: []github.CommitStatus{status("ci/build", tt.state)}})
				want := []core.Check{{Name: "ci/build", Kind: core.CheckKindStatus, State: tt.want, Detail: tt.state}}
				if state != tt.want || !slices.Equal(checks, want) {
					t.Errorf("Evaluate(%v) = %s, %+v, want %s, %+v", required, state, checks, tt.want, want)
				}
			}
		})
	}
}

func TestEvaluate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		required   []string
		snapshot   intake.Snapshot
		want       core.ChecksState
		wantChecks []core.Check
	}{
		{
			name: "all checks: nothing reported yet",
			want: core.ChecksPending,
		},
		{
			name:     "all checks: only suites reported",
			snapshot: intake.Snapshot{Suites: []github.CheckSuite{suite(5, "queued", 0, "stale-app")}},
			want:     core.ChecksPending,
		},
		{
			name: "all checks: every run and status passes",
			snapshot: intake.Snapshot{
				CheckRuns: []github.CheckRun{done(1, "test", "success"), done(2, "docs", "skipped")},
				Statuses:  []github.CommitStatus{status("ci/legacy", "success")},
			},
			want: core.ChecksSuccess,
			wantChecks: []core.Check{
				{Name: "ci/legacy", Kind: core.CheckKindStatus, State: core.ChecksSuccess, Detail: "success"},
				{Name: "docs", Kind: core.CheckKindRun, State: core.ChecksSuccess, Detail: "skipped"},
				{Name: "test", Kind: core.CheckKindRun, State: core.ChecksSuccess, Detail: "success"},
			},
		},
		{
			name: "all checks: failure beats pending",
			snapshot: intake.Snapshot{
				CheckRuns: []github.CheckRun{run(1, "build", "in_progress", ""), done(2, "test", "failure")},
			},
			want: core.ChecksFailure,
			wantChecks: []core.Check{
				{Name: "build", Kind: core.CheckKindRun, State: core.ChecksPending, Detail: "in_progress"},
				{Name: "test", Kind: core.CheckKindRun, State: core.ChecksFailure, Detail: "failure"},
			},
		},
		{
			name: "all checks: suite still running with green runs",
			snapshot: intake.Snapshot{
				CheckRuns: []github.CheckRun{done(1, "build", "success")},
				Suites:    []github.CheckSuite{suite(10, "in_progress", 1, "github-actions")},
			},
			want: core.ChecksPending,
			wantChecks: []core.Check{
				{Name: "build", Kind: core.CheckKindRun, State: core.ChecksSuccess, Detail: "success"},
				{Name: "check suite 10 (github-actions)", Kind: core.CheckKindSuite, State: core.ChecksPending, Detail: "in_progress"},
			},
		},
		{
			name: "all checks: suite without app slug",
			snapshot: intake.Snapshot{
				CheckRuns: []github.CheckRun{done(1, "build", "success")},
				Suites:    []github.CheckSuite{suite(11, "queued", 2, "")},
			},
			want: core.ChecksPending,
			wantChecks: []core.Check{
				{Name: "build", Kind: core.CheckKindRun, State: core.ChecksSuccess, Detail: "success"},
				{Name: "check suite 11", Kind: core.CheckKindSuite, State: core.ChecksPending, Detail: "queued"},
			},
		},
		{
			name: "all checks: zero-run and completed suites ignored",
			snapshot: intake.Snapshot{
				CheckRuns: []github.CheckRun{done(1, "build", "success")},
				Suites: []github.CheckSuite{
					suite(10, "queued", 0, "forever-queued"),
					suite(11, "completed", 3, "github-actions"),
				},
			},
			want: core.ChecksSuccess,
			wantChecks: []core.Check{
				{Name: "build", Kind: core.CheckKindRun, State: core.ChecksSuccess, Detail: "success"},
			},
		},
		{
			name:     "required: suites ignored",
			required: []string{"build"},
			snapshot: intake.Snapshot{
				CheckRuns: []github.CheckRun{done(1, "build", "success")},
				Suites:    []github.CheckSuite{suite(10, "in_progress", 4, "github-actions")},
			},
			want: core.ChecksSuccess,
			wantChecks: []core.Check{
				{Name: "build", Kind: core.CheckKindRun, State: core.ChecksSuccess, Detail: "success"},
			},
		},
		{
			name:     "required: others ignored",
			required: []string{"test", "lint"},
			snapshot: intake.Snapshot{
				CheckRuns: []github.CheckRun{done(1, "test", "success"), done(2, "flaky-e2e", "failure"), run(3, "deploy-preview", "queued", "")},
				Statuses:  []github.CommitStatus{status("lint", "success"), status("coverage", "error")},
			},
			want: core.ChecksSuccess,
			wantChecks: []core.Check{
				{Name: "lint", Kind: core.CheckKindStatus, State: core.ChecksSuccess, Detail: "success"},
				{Name: "test", Kind: core.CheckKindRun, State: core.ChecksSuccess, Detail: "success"},
			},
		},
		{
			name:     "required: missing check is pending",
			required: []string{"test", "lint"},
			snapshot: intake.Snapshot{CheckRuns: []github.CheckRun{done(1, "test", "success")}},
			want:     core.ChecksPending,
			wantChecks: []core.Check{
				{Name: "lint", State: core.ChecksPending, Detail: "not reported"},
				{Name: "test", Kind: core.CheckKindRun, State: core.ChecksSuccess, Detail: "success"},
			},
		},
		{
			name:     "required: failure beats missing",
			required: []string{"test", "lint"},
			snapshot: intake.Snapshot{CheckRuns: []github.CheckRun{done(1, "test", "timed_out")}},
			want:     core.ChecksFailure,
			wantChecks: []core.Check{
				{Name: "lint", State: core.ChecksPending, Detail: "not reported"},
				{Name: "test", Kind: core.CheckKindRun, State: core.ChecksFailure, Detail: "timed_out"},
			},
		},
		{
			name:     "required: nothing reported",
			required: []string{"test"},
			want:     core.ChecksPending,
			wantChecks: []core.Check{
				{Name: "test", State: core.ChecksPending, Detail: "not reported"},
			},
		},
		{
			name:     "required: matching is case-sensitive",
			required: []string{"Test"},
			snapshot: intake.Snapshot{CheckRuns: []github.CheckRun{done(1, "test", "success")}},
			want:     core.ChecksPending,
			wantChecks: []core.Check{
				{Name: "Test", State: core.ChecksPending, Detail: "not reported"},
			},
		},
		{
			name:     "required: duplicate names listed once",
			required: []string{"test", "test"},
			snapshot: intake.Snapshot{CheckRuns: []github.CheckRun{done(1, "test", "success")}},
			want:     core.ChecksSuccess,
			wantChecks: []core.Check{
				{Name: "test", Kind: core.CheckKindRun, State: core.ChecksSuccess, Detail: "success"},
			},
		},
		{
			name:     "re-run: highest run id wins over an older failure",
			required: []string{"test"},
			snapshot: intake.Snapshot{CheckRuns: []github.CheckRun{done(9, "test", "success"), done(3, "test", "failure")}},
			want:     core.ChecksSuccess,
			wantChecks: []core.Check{
				{Name: "test", Kind: core.CheckKindRun, State: core.ChecksSuccess, Detail: "success"},
			},
		},
		{
			name:     "re-run: in-progress re-run replaces an older success",
			required: []string{"test"},
			snapshot: intake.Snapshot{CheckRuns: []github.CheckRun{done(3, "test", "success"), run(9, "test", "in_progress", "")}},
			want:     core.ChecksPending,
			wantChecks: []core.Check{
				{Name: "test", Kind: core.CheckKindRun, State: core.ChecksPending, Detail: "in_progress"},
			},
		},
		{
			name:     "run and status share a name: worst wins",
			required: []string{"ci"},
			snapshot: intake.Snapshot{
				CheckRuns: []github.CheckRun{done(1, "ci", "success")},
				Statuses:  []github.CommitStatus{status("ci", "failure")},
			},
			want: core.ChecksFailure,
			wantChecks: []core.Check{
				{Name: "ci", Kind: core.CheckKindStatus, State: core.ChecksFailure, Detail: "failure"},
			},
		},
		{
			name: "run and status share a name: pending beats success",
			snapshot: intake.Snapshot{
				CheckRuns: []github.CheckRun{run(1, "ci", "queued", "")},
				Statuses:  []github.CommitStatus{status("ci", "success")},
			},
			want: core.ChecksPending,
			wantChecks: []core.Check{
				{Name: "ci", Kind: core.CheckKindRun, State: core.ChecksPending, Detail: "queued"},
			},
		},
		{
			name: "duplicate statuses: worst wins",
			snapshot: intake.Snapshot{
				Statuses: []github.CommitStatus{status("ci", "success"), status("ci", "pending")},
			},
			want: core.ChecksPending,
			wantChecks: []core.Check{
				{Name: "ci", Kind: core.CheckKindStatus, State: core.ChecksPending, Detail: "pending"},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			state, checks := intake.Evaluate(tt.required, tt.snapshot)
			if state != tt.want {
				t.Errorf("state = %s, want %s", state, tt.want)
			}
			if !slices.Equal(checks, tt.wantChecks) {
				t.Errorf("checks = %+v\nwant %+v", checks, tt.wantChecks)
			}
		})
	}
}

func TestEvaluateOrderIsStable(t *testing.T) {
	t.Parallel()

	snapshot := intake.Snapshot{
		CheckRuns: []github.CheckRun{done(1, "zeta", "success"), done(2, "alpha", "success"), done(3, "mid", "success")},
		Statuses:  []github.CommitStatus{status("beta", "success")},
	}
	_, first := intake.Evaluate(nil, snapshot)
	for range 20 {
		slices.Reverse(snapshot.CheckRuns)
		_, again := intake.Evaluate(nil, snapshot)
		if !slices.Equal(first, again) {
			t.Fatalf("Evaluate order changed: %+v then %+v", first, again)
		}
	}
	names := make([]string, len(first))
	for i, c := range first {
		names[i] = c.Name
	}
	if !slices.IsSorted(names) {
		t.Errorf("checks are not sorted by name: %v", names)
	}
}

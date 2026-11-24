package store_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/store"
	"github.com/sharpintelligence/doorstep/internal/store/storetest"
)

type historyStep struct {
	name      string
	sha       string
	app       string
	rollback  bool
	migration bool
	state     core.State
}

func recordHistory(t *testing.T, st *store.Store, steps []historyStep) map[string]core.Deployment {
	t.Helper()
	shas := map[string]string{}
	rows := map[string]core.Deployment{}
	var last core.Deployment
	for _, s := range steps {
		sha, ok := shas[s.sha]
		if !ok {
			sha = randomSHA(t)
			shas[s.sha] = sha
		}
		app := s.app
		if app == "" {
			app = "web"
		}
		d := deploymentOf(app, sha)
		if s.rollback {
			d.Kind = core.KindRollback
			d.RollbackOfID = &last.ID
		}
		if s.migration {
			d.Migration = true
			d.MigrationReason = "flagged " + s.name
		}
		state := s.state
		if state == "" {
			state = core.StateCancelled
		}
		created := mustCreateIn(t, st, d, state)
		rows[s.name] = created
		if app == "web" {
			last = created
		}
	}
	return rows
}

func TestMigrationsBetween(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		steps   []historyStep
		to      string
		current string
		want    []string
	}{
		{
			name:    "plain crossing",
			steps:   []historyStep{{name: "v1", sha: "a"}, {name: "v2", sha: "b", migration: true}, {name: "v3", sha: "c"}},
			to:      "v1",
			current: "v3",
			want:    []string{"v2"},
		},
		{
			name:    "rolling back the flagged release itself",
			steps:   []historyStep{{name: "v1", sha: "a"}, {name: "v2", sha: "b", migration: true}},
			to:      "v1",
			current: "v2",
			want:    []string{"v2"},
		},
		{
			name:    "rolling back to a flagged release",
			steps:   []historyStep{{name: "v1", sha: "a", migration: true}, {name: "v2", sha: "b"}},
			to:      "v1",
			current: "v2",
		},
		{
			name: "superseded flagged release still counts",
			steps: []historyStep{
				{name: "v1", sha: "a"},
				{name: "v2", sha: "b", migration: true, state: core.StateSuperseded},
				{name: "v3", sha: "c"},
			},
			to:      "v1",
			current: "v3",
			want:    []string{"v2"},
		},
		{
			name: "earlier rollback to the target",
			steps: []historyStep{
				{name: "v1", sha: "a"},
				{name: "v2", sha: "b", migration: true},
				{name: "rb", sha: "a", rollback: true},
				{name: "v3", sha: "c"},
			},
			to:      "rb",
			current: "v3",
			want:    []string{"v2"},
		},
		{
			name: "re-enqueue of the target commit",
			steps: []historyStep{
				{name: "v1", sha: "a"},
				{name: "v2", sha: "b", migration: true},
				{name: "v1-again", sha: "a"},
				{name: "v3", sha: "c"},
			},
			to:      "v1-again",
			current: "v3",
			want:    []string{"v2"},
		},
		{
			name: "flagged release newer than the current one",
			steps: []historyStep{
				{name: "v1", sha: "a"},
				{name: "v2", sha: "b"},
				{name: "v3", sha: "c", migration: true},
			},
			to:      "v1",
			current: "v2",
		},
		{
			name: "rollback rows never count",
			steps: []historyStep{
				{name: "v1", sha: "a"},
				{name: "v2", sha: "b"},
				{name: "rb", sha: "a", rollback: true, migration: true},
				{name: "v3", sha: "c"},
			},
			to:      "v1",
			current: "v3",
		},
		{
			name: "other apps never count",
			steps: []historyStep{
				{name: "v1", sha: "a"},
				{name: "other", sha: "x", app: "api", migration: true},
				{name: "v2", sha: "b"},
			},
			to:      "v1",
			current: "v2",
		},
		{
			name: "several crossed releases in id order",
			steps: []historyStep{
				{name: "v1", sha: "a"},
				{name: "v2", sha: "b", migration: true},
				{name: "v3", sha: "c"},
				{name: "v4", sha: "d", migration: true},
			},
			to:      "v1",
			current: "v4",
			want:    []string{"v2", "v4"},
		},
		{
			name: "rolling forward crosses nothing",
			steps: []historyStep{
				{name: "v1", sha: "a"},
				{name: "v2", sha: "b", migration: true},
				{name: "v3", sha: "c"},
			},
			to:      "v3",
			current: "v1",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			st := storetest.Open(t)
			rows := recordHistory(t, st, tt.steps)
			got, err := st.MigrationsBetween(t.Context(), webTarget(), rows[tt.to], rows[tt.current])
			if err != nil {
				t.Fatalf("MigrationsBetween: %v", err)
			}
			want := make([]core.DeploymentID, 0, len(tt.want))
			for _, name := range tt.want {
				want = append(want, rows[name].ID)
			}
			if !slices.Equal(ids(got), want) {
				t.Errorf("MigrationsBetween = %v, want %v", ids(got), want)
			}
			for _, d := range got {
				if !d.Migration || !strings.HasPrefix(d.MigrationReason, "flagged ") {
					t.Errorf("deployment %d migration = %v %q", d.ID, d.Migration, d.MigrationReason)
				}
			}
		})
	}
}

func TestMigrationsBetweenLimit(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	steps := []historyStep{{name: "v0", sha: "base"}}
	for i := range store.MaxMigrationsBetween + 5 {
		name := "m" + string(rune('a'+i))
		steps = append(steps, historyStep{name: name, sha: name, migration: true})
	}
	rows := recordHistory(t, st, steps)
	got, err := st.MigrationsBetween(t.Context(), webTarget(), rows["v0"], rows["m"+string(rune('a'+store.MaxMigrationsBetween+4))])
	if err != nil {
		t.Fatalf("MigrationsBetween: %v", err)
	}
	if len(got) != store.MaxMigrationsBetween || got[0].ID != rows["ma"].ID {
		t.Errorf("MigrationsBetween returned %d rows starting at %v, want %d starting at %d", len(got), ids(got)[:1], store.MaxMigrationsBetween, rows["ma"].ID)
	}
}

func TestMigrationsBetweenValidates(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	if _, err := st.MigrationsBetween(t.Context(), core.Target{}, core.Deployment{Commit: core.Commit{SHA: testSHA}}, core.Deployment{}); err == nil {
		t.Error("MigrationsBetween accepted an invalid target")
	}
	if _, err := st.MigrationsBetween(t.Context(), webTarget(), core.Deployment{}, core.Deployment{}); err == nil {
		t.Error("MigrationsBetween accepted a target deployment without a commit")
	}
}

func TestDeploymentMigrationRoundTrip(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	d := newDeployment("web")
	d.Migration = true
	d.MigrationReason = "label db-migration on #12 (0123456)"
	created := mustCreate(t, st, d, 0)
	got, err := st.GetDeployment(t.Context(), created.ID)
	if err != nil {
		t.Fatalf("GetDeployment: %v", err)
	}
	if !got.Migration || got.MigrationReason != d.MigrationReason {
		t.Errorf("migration = %v %q, want true %q", got.Migration, got.MigrationReason, d.MigrationReason)
	}
	plain := mustCreate(t, st, deploymentOf("api", otherSHA), 0)
	if plain.Migration || plain.MigrationReason != "" {
		t.Errorf("unflagged deployment = %v %q", plain.Migration, plain.MigrationReason)
	}

	bad := newDeployment("web")
	bad.MigrationReason = "no flag"
	if _, err := st.CreateDeployment(t.Context(), bad, 0); err == nil {
		t.Error("CreateDeployment accepted a migration reason without the flag")
	}
}

func TestLastDeployCommit(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	if _, found, err := st.LastDeployCommit(t.Context(), webTarget()); err != nil || found {
		t.Fatalf("LastDeployCommit on an empty history = %v, %v", found, err)
	}
	rows := recordHistory(t, st, []historyStep{
		{name: "v1", sha: "a"},
		{name: "v2", sha: "b", state: core.StateSuperseded},
		{name: "rb", sha: "a", rollback: true},
		{name: "other", sha: "x", app: "api"},
	})
	got, found, err := st.LastDeployCommit(t.Context(), webTarget())
	if err != nil || !found {
		t.Fatalf("LastDeployCommit = %v, %v", found, err)
	}
	if got != rows["v2"].Commit {
		t.Errorf("LastDeployCommit = %+v, want %+v", got, rows["v2"].Commit)
	}
	if _, _, err := st.LastDeployCommit(t.Context(), core.Target{}); err == nil {
		t.Error("LastDeployCommit accepted an invalid target")
	}
}

func webTarget() core.Target {
	return core.Target{App: "web", Environment: "prod"}
}

package store_test

import (
	"errors"
	"reflect"
	"testing"

	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/store"
	"github.com/sharpintelligence/doorstep/internal/store/storetest"
)

func TestSetLineagePatches(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	d := mustCreateIn(t, st, newDeployment("web"), core.StateResolving)

	steps := []struct {
		patch    core.Lineage
		want     core.Lineage
		wantData map[string]any
	}{
		{
			patch:    core.Lineage{Digest: testDigest},
			want:     core.Lineage{Digest: testDigest},
			wantData: map[string]any{"image_digest": testDigest},
		},
		{
			patch: core.Lineage{PreviousDigest: otherDigest, GitOpsRepository: "acme/gitops", PinCommitSHA: testSHA},
			want:  core.Lineage{Digest: testDigest, PreviousDigest: otherDigest, GitOpsRepository: "acme/gitops", PinCommitSHA: testSHA},
			wantData: map[string]any{
				"previous_digest": otherDigest, "gitops_repository": "acme/gitops", "pin_commit_sha": testSHA,
			},
		},
		{
			patch:    core.Lineage{PinCommitSHA: otherSHA, ArgoCDRevision: otherSHA},
			want:     core.Lineage{Digest: testDigest, PreviousDigest: otherDigest, GitOpsRepository: "acme/gitops", PinCommitSHA: otherSHA, ArgoCDRevision: otherSHA},
			wantData: map[string]any{"pin_commit_sha": otherSHA, "argocd_revision": otherSHA},
		},
		{
			patch:    core.Lineage{Digest: otherDigest},
			want:     core.Lineage{Digest: otherDigest, PreviousDigest: otherDigest, GitOpsRepository: "acme/gitops", PinCommitSHA: otherSHA, ArgoCDRevision: otherSHA},
			wantData: map[string]any{"image_digest": otherDigest},
		},
	}
	for i, step := range steps {
		got, err := st.SetLineage(t.Context(), d.ID, step.patch, "worker-1")
		if err != nil {
			t.Fatalf("step %d SetLineage: %v", i, err)
		}
		if lineageOf(got) != step.want {
			t.Errorf("step %d lineage = %+v, want %+v", i, lineageOf(got), step.want)
		}
		if got.State != core.StateResolving {
			t.Errorf("step %d changed state to %s", i, got.State)
		}
		e := lastEvent(t, st, d.ID, core.EventNote)
		if e.actor != "worker-1" || e.message != "lineage recorded" || e.from != "" || e.to != "" || !reflect.DeepEqual(e.data, step.wantData) {
			t.Errorf("step %d note event = %+v, want data %v", i, e, step.wantData)
		}
	}
	if n := queryInt(t, st, "SELECT count(*) FROM deployment_events WHERE deployment_id = $1 AND type = 'note'", d.ID); n != len(steps) {
		t.Errorf("note events = %d, want %d", n, len(steps))
	}
}

func TestSetLineageRejects(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		state core.State
		id    func(core.DeploymentID) core.DeploymentID
		patch core.Lineage
		want  error
		text  string
	}{
		{name: "empty patch", state: core.StateQueued, text: "sets no fields"},
		{name: "invalid digest", state: core.StateQueued, patch: core.Lineage{Digest: "latest"}, want: core.ErrInvalidDigest},
		{name: "short pin commit", state: core.StateQueued, patch: core.Lineage{PinCommitSHA: "abc"}, want: core.ErrInvalidCommitSHA},
		{name: "blank revision", state: core.StateQueued, patch: core.Lineage{ArgoCDRevision: " "}, text: "argo cd revision"},
		{name: "missing deployment", state: core.StateQueued, id: func(core.DeploymentID) core.DeploymentID { return 1 << 40 }, patch: core.Lineage{Digest: testDigest}, want: core.ErrNotFound},
		{name: "succeeded", state: core.StateSucceeded, patch: core.Lineage{Digest: testDigest}, want: core.ErrStateConflict, text: "is succeeded"},
		{name: "failed", state: core.StateFailed, patch: core.Lineage{ArgoCDRevision: "abc"}, want: core.ErrStateConflict},
		{name: "cancelled", state: core.StateCancelled, patch: core.Lineage{PinCommitSHA: testSHA}, want: core.ErrStateConflict},
		{name: "superseded", state: core.StateSuperseded, patch: core.Lineage{GitOpsRepository: "acme/gitops"}, want: core.ErrStateConflict},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			st := storetest.Open(t)
			d := mustCreateIn(t, st, newDeployment("web"), tt.state)
			events := countEvents(t, st, d.ID)
			id := d.ID
			if tt.id != nil {
				id = tt.id(id)
			}
			_, err := st.SetLineage(t.Context(), id, tt.patch, "worker")
			if err == nil {
				t.Fatal("SetLineage succeeded")
			}
			if tt.want != nil && !errors.Is(err, tt.want) {
				t.Errorf("error = %v, want %v", err, tt.want)
			}
			if tt.text != "" && !containsText(err, tt.text) {
				t.Errorf("error %q does not mention %q", err, tt.text)
			}
			got, err := st.GetDeployment(t.Context(), d.ID)
			if err != nil {
				t.Fatalf("GetDeployment: %v", err)
			}
			if !lineageOf(got).IsZero() || countEvents(t, st, d.ID) != events {
				t.Errorf("rejected SetLineage changed the deployment: %+v", got)
			}
		})
	}
}

func TestSetLineageInsideTransaction(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	d := mustCreate(t, st, newDeployment("web"), 0)
	errBoom := errors.New("boom")
	err := st.InTx(t.Context(), func(tx *store.Store) error {
		if _, err := tx.SetLineage(t.Context(), d.ID, core.Lineage{Digest: testDigest}, "worker"); err != nil {
			return err
		}
		return errBoom
	})
	if !errors.Is(err, errBoom) {
		t.Fatalf("InTx = %v, want %v", err, errBoom)
	}
	got, err := st.GetDeployment(t.Context(), d.ID)
	if err != nil {
		t.Fatalf("GetDeployment: %v", err)
	}
	if got.Digest != "" || countEvents(t, st, d.ID) != 1 {
		t.Errorf("rolled back SetLineage persisted: digest %q, %d events", got.Digest, countEvents(t, st, d.ID))
	}
}

func lineageOf(d core.Deployment) core.Lineage {
	return core.Lineage{
		Digest:           d.Digest,
		PreviousDigest:   d.PreviousDigest,
		GitOpsRepository: d.GitOpsRepository,
		PinCommitSHA:     d.PinCommitSHA,
		ArgoCDRevision:   d.ArgoCDRevision,
	}
}

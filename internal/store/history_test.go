package store_test

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/store"
	"github.com/sharpintelligence/doorstep/internal/store/storetest"
)

func ids(ds []core.Deployment) []core.DeploymentID {
	out := make([]core.DeploymentID, 0, len(ds))
	for _, d := range ds {
		out = append(out, d.ID)
	}
	return out
}

func insertBulk(t *testing.T, st *store.Store, app string, n int) {
	t.Helper()
	appID, err := st.EnsureApp(t.Context(), core.Target{App: app, Environment: "prod"})
	if err != nil {
		t.Fatalf("EnsureApp: %v", err)
	}
	mustExec(t, st, `
		INSERT INTO deployments (app_id, kind, state, source, source_repository, commit_sha, image_repository, image_tag)
		SELECT $1, 'deploy', 'cancelled', 'api', 'acme/bulk', $2, 'ghcr.io/acme/bulk', 'v1'
		FROM generate_series(1, $3)`, appID, testSHA, n)
}

func TestListDeploymentsFilters(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	queued := mustCreate(t, st, newDeployment("web"), 0)
	succeededDeploy := newDeployment("web")
	succeededDeploy.Commit.SHA = otherSHA
	succeeded := mustCreateIn(t, st, succeededDeploy, core.StateSucceeded)
	staging := newDeployment("web")
	staging.Target.Environment = "staging"
	failed := mustCreateIn(t, st, staging, core.StateFailed)
	rb := newDeployment("web")
	rb.Kind = core.KindRollback
	rb.RollbackOfID = &succeeded.ID
	rollback := mustCreateIn(t, st, rb, core.StateCancelled)
	api := mustCreate(t, st, newDeployment("api"), 0)
	mustExec(t, st, "UPDATE deployments SET image_digest = $2 WHERE id = $1", succeeded.ID, otherDigest)
	mustExec(t, st, "UPDATE deployments SET image_digest = $2 WHERE id = $1", failed.ID, testDigest)

	all := []core.DeploymentID{api.ID, rollback.ID, failed.ID, succeeded.ID, queued.ID}
	tests := []struct {
		name   string
		filter store.DeploymentFilter
		want   []core.DeploymentID
	}{
		{name: "everything", want: all},
		{name: "app", filter: store.DeploymentFilter{App: "web"}, want: []core.DeploymentID{rollback.ID, failed.ID, succeeded.ID, queued.ID}},
		{name: "app and environment", filter: store.DeploymentFilter{App: "web", Environment: "prod"}, want: []core.DeploymentID{rollback.ID, succeeded.ID, queued.ID}},
		{name: "environment", filter: store.DeploymentFilter{Environment: "staging"}, want: []core.DeploymentID{failed.ID}},
		{name: "one state", filter: store.DeploymentFilter{States: []core.State{core.StateQueued}}, want: []core.DeploymentID{api.ID, queued.ID}},
		{name: "several states", filter: store.DeploymentFilter{States: []core.State{core.StateSucceeded, core.StateFailed}}, want: []core.DeploymentID{failed.ID, succeeded.ID}},
		{name: "rollbacks", filter: store.DeploymentFilter{Kinds: []core.Kind{core.KindRollback}}, want: []core.DeploymentID{rollback.ID}},
		{name: "every kind", filter: store.DeploymentFilter{Kinds: core.Kinds()}, want: all},
		{name: "commit", filter: store.DeploymentFilter{CommitSHA: otherSHA}, want: []core.DeploymentID{succeeded.ID}},
		{name: "digest", filter: store.DeploymentFilter{Digest: otherDigest}, want: []core.DeploymentID{succeeded.ID}},
		{name: "app and digest", filter: store.DeploymentFilter{App: "web", Digest: testDigest}, want: []core.DeploymentID{failed.ID}},
		{name: "unknown digest", filter: store.DeploymentFilter{App: "web", Digest: core.Digest("sha256:" + strings.Repeat("a", 64))}, want: []core.DeploymentID{}},
		{name: "before cursor", filter: store.DeploymentFilter{Before: failed.ID}, want: []core.DeploymentID{succeeded.ID, queued.ID}},
		{name: "combined", filter: store.DeploymentFilter{App: "web", States: []core.State{core.StateQueued}, CommitSHA: testSHA}, want: []core.DeploymentID{queued.ID}},
		{name: "unknown app", filter: store.DeploymentFilter{App: "nope"}, want: []core.DeploymentID{}},
		{name: "unknown environment", filter: store.DeploymentFilter{App: "web", Environment: "dev"}, want: []core.DeploymentID{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			page, err := st.ListDeployments(t.Context(), tt.filter)
			if err != nil {
				t.Fatalf("ListDeployments: %v", err)
			}
			if got := ids(page.Items); !slices.Equal(got, tt.want) {
				t.Errorf("ids = %v, want %v", got, tt.want)
			}
			if page.Next != 0 {
				t.Errorf("Next = %d, want 0", page.Next)
			}
		})
	}

	page, err := st.ListDeployments(t.Context(), store.DeploymentFilter{App: "web", Kinds: []core.Kind{core.KindRollback}})
	if err != nil {
		t.Fatalf("ListDeployments: %v", err)
	}
	if len(page.Items) != 1 {
		t.Fatalf("items = %d, want 1", len(page.Items))
	}
	got := page.Items[0]
	if got.Target != rb.Target || got.State != core.StateCancelled || got.RollbackOfID == nil || *got.RollbackOfID != succeeded.ID || got.FinishedAt == nil {
		t.Errorf("listed rollback = %+v", got)
	}
}

func TestListDeploymentsPaginates(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	var want []core.DeploymentID
	for range 7 {
		want = append(want, mustCreate(t, st, newDeployment("paged"), 0).ID)
	}
	mustCreate(t, st, newDeployment("other"), 0)
	slices.Reverse(want)

	var got []core.DeploymentID
	var pages []int
	filter := store.DeploymentFilter{App: "paged", Limit: 3}
	for {
		page, err := st.ListDeployments(t.Context(), filter)
		if err != nil {
			t.Fatalf("ListDeployments: %v", err)
		}
		got = append(got, ids(page.Items)...)
		pages = append(pages, len(page.Items))
		if page.Next == 0 {
			break
		}
		if page.Next != page.Items[len(page.Items)-1].ID {
			t.Errorf("Next = %d, want last item %d", page.Next, page.Items[len(page.Items)-1].ID)
		}
		mustCreate(t, st, newDeployment("paged"), 0)
		filter.Before = page.Next
	}
	if !slices.Equal(got, want) {
		t.Errorf("paged ids = %v, want %v", got, want)
	}
	if !slices.Equal(pages, []int{3, 3, 1}) {
		t.Errorf("page sizes = %v, want [3 3 1]", pages)
	}

	exact, err := st.ListDeployments(t.Context(), store.DeploymentFilter{App: "other", Limit: 1})
	if err != nil {
		t.Fatalf("ListDeployments: %v", err)
	}
	if len(exact.Items) != 1 || exact.Next != 0 {
		t.Errorf("exactly full page = %d items, next %d, want 1 item and no next", len(exact.Items), exact.Next)
	}
}

func TestListDeploymentsClampsLimit(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	insertBulk(t, st, "bulk", store.MaxPageSize+10)
	tests := []struct {
		limit    int
		want     int
		wantNext bool
	}{
		{limit: 0, want: store.DefaultPageSize, wantNext: true},
		{limit: -5, want: store.DefaultPageSize, wantNext: true},
		{limit: 1, want: 1, wantNext: true},
		{limit: store.MaxPageSize, want: store.MaxPageSize, wantNext: true},
		{limit: 1000, want: store.MaxPageSize, wantNext: true},
	}
	for _, tt := range tests {
		page, err := st.ListDeployments(t.Context(), store.DeploymentFilter{App: "bulk", Limit: tt.limit})
		if err != nil {
			t.Fatalf("ListDeployments(limit %d): %v", tt.limit, err)
		}
		if len(page.Items) != tt.want || (page.Next != 0) != tt.wantNext {
			t.Errorf("limit %d: %d items, next %d; want %d items, next %v", tt.limit, len(page.Items), page.Next, tt.want, tt.wantNext)
		}
	}
}

func TestListDeploymentsRejectsInvalidFilters(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	tests := []struct {
		name   string
		filter store.DeploymentFilter
		want   error
		text   string
	}{
		{name: "unknown state", filter: store.DeploymentFilter{States: []core.State{"paused"}}, want: core.ErrUnknownState},
		{name: "unknown kind", filter: store.DeploymentFilter{Kinds: []core.Kind{"hotfix"}}, text: "kind"},
		{name: "short commit", filter: store.DeploymentFilter{CommitSHA: "abc1234"}, want: core.ErrInvalidCommitSHA},
		{name: "invalid digest", filter: store.DeploymentFilter{Digest: "sha256:abc"}, want: core.ErrInvalidDigest},
		{name: "negative cursor", filter: store.DeploymentFilter{Before: -1}, text: "cursor"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := st.ListDeployments(t.Context(), tt.filter)
			if err == nil {
				t.Fatal("ListDeployments succeeded")
			}
			if tt.want != nil && !errors.Is(err, tt.want) {
				t.Errorf("error = %v, want %v", err, tt.want)
			}
			if tt.text != "" && !containsText(err, tt.text) {
				t.Errorf("error %q does not mention %q", err, tt.text)
			}
		})
	}
}

func TestGetDeploymentHistory(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	origin := mustCreateIn(t, st, newDeployment("web"), core.StatePinning)
	if _, err := st.SetLineage(t.Context(), origin.ID, core.Lineage{Digest: testDigest}, "resolver"); err != nil {
		t.Fatalf("SetLineage: %v", err)
	}
	origin = mustAdvance(t, st, origin, core.StateVerifying)
	if _, err := st.TransitionDeployment(t.Context(), store.Transition{
		ID: origin.ID, From: core.StateVerifying, To: core.StateFailed, Actor: "gates", FailureReason: "health",
		Lineage: core.Lineage{ArgoCDRevision: otherSHA},
	}); err != nil {
		t.Fatalf("fail: %v", err)
	}
	mustExec(t, st, "INSERT INTO gate_results (deployment_id, name, status, message, evidence, finished_at) VALUES ($1, 'soak', 'passed', 'quiet', '{\"minutes\":10}', now())", origin.ID)
	mustExec(t, st, "INSERT INTO gate_results (deployment_id, name, status) VALUES ($1, 'health', 'failed')", origin.ID)

	rb := newDeployment("web")
	rb.Kind = core.KindRollback
	rb.Source = core.SourceSystem
	rb.RollbackOfID = &origin.ID
	rollback := mustCreate(t, st, rb, 100)
	retry := newDeployment("web")
	retry.ParentID = &origin.ID
	retried := mustCreate(t, st, retry, 0)
	mustCreate(t, st, newDeployment("web"), 0)

	h, err := st.GetDeploymentHistory(t.Context(), origin.ID)
	if err != nil {
		t.Fatalf("GetDeploymentHistory: %v", err)
	}
	d := h.Deployment
	if d.ID != origin.ID || d.State != core.StateFailed || d.FailureReason != "health" || d.Digest != testDigest || d.ArgoCDRevision != otherSHA {
		t.Errorf("deployment = %+v", d)
	}

	type step struct {
		typ      core.EventType
		from, to core.State
	}
	wantEvents := []step{
		{core.EventTransition, "", core.StateQueued},
		{core.EventTransition, core.StateQueued, core.StateResolving},
		{core.EventTransition, core.StateResolving, core.StatePinning},
		{core.EventNote, "", ""},
		{core.EventTransition, core.StatePinning, core.StateSyncing},
		{core.EventTransition, core.StateSyncing, core.StateVerifying},
		{core.EventTransition, core.StateVerifying, core.StateFailed},
	}
	if len(h.Events) != len(wantEvents) || h.EventsTruncated {
		t.Fatalf("events = %+v (truncated %v), want %d", h.Events, h.EventsTruncated, len(wantEvents))
	}
	for i, e := range h.Events {
		if (step{e.Type, e.From, e.To}) != wantEvents[i] {
			t.Errorf("event %d = %s %q -> %q, want %+v", i, e.Type, e.From, e.To, wantEvents[i])
		}
		if e.DeploymentID != origin.ID || e.Target != origin.Target || e.OccurredAt.IsZero() || len(e.Data) == 0 {
			t.Errorf("event %d = %+v", i, e)
		}
		if i > 0 && e.ID <= h.Events[i-1].ID {
			t.Errorf("event %d id %d not after %d", i, e.ID, h.Events[i-1].ID)
		}
	}
	if last := h.Events[len(h.Events)-1]; last.Actor != "gates" || string(last.Data) != `{"argocd_revision": "`+otherSHA+`"}` {
		t.Errorf("failed event = %+v data %s", last, last.Data)
	}

	if len(h.Gates) != 2 || h.Gates[0].Name != "health" || h.Gates[1].Name != "soak" {
		t.Fatalf("gates = %+v", h.Gates)
	}
	soak := h.Gates[1]
	if soak.Status != core.GatePassed || soak.Message != "quiet" || string(soak.Evidence) != `{"minutes": 10}` || soak.FinishedAt == nil || soak.StartedAt.IsZero() {
		t.Errorf("soak gate = %+v evidence %s", soak, soak.Evidence)
	}
	if h.Gates[0].Status != core.GateFailed || h.Gates[0].FinishedAt != nil {
		t.Errorf("health gate = %+v", h.Gates[0])
	}
	if got := ids(h.Rollbacks); !slices.Equal(got, []core.DeploymentID{rollback.ID}) {
		t.Errorf("rollbacks = %v, want [%d]", got, rollback.ID)
	}
	if got := ids(h.Retries); !slices.Equal(got, []core.DeploymentID{retried.ID}) {
		t.Errorf("retries = %v, want [%d]", got, retried.ID)
	}

	plain, err := st.GetDeploymentHistory(t.Context(), retried.ID)
	if err != nil {
		t.Fatalf("GetDeploymentHistory(retry): %v", err)
	}
	if len(plain.Events) != 1 || len(plain.Gates) != 0 || len(plain.Rollbacks) != 0 || len(plain.Retries) != 0 {
		t.Errorf("retry history = %+v", plain)
	}
}

func TestGetDeploymentHistoryNotFound(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	if _, err := st.GetDeploymentHistory(t.Context(), 42); !errors.Is(err, core.ErrNotFound) {
		t.Errorf("GetDeploymentHistory error = %v, want ErrNotFound", err)
	}
}

func TestGetDeploymentHistoryTruncatesEvents(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	d := mustCreate(t, st, newDeployment("web"), 0)
	mustExec(t, st, `
		INSERT INTO deployment_events (deployment_id, app_id, type, message)
		SELECT d.id, d.app_id, 'note', 'note ' || n
		FROM deployments d, generate_series(1, $2) n
		WHERE d.id = $1`, d.ID, store.MaxHistoryEvents+20)
	mustAdvance(t, st, d, core.StateCancelled)

	h, err := st.GetDeploymentHistory(t.Context(), d.ID)
	if err != nil {
		t.Fatalf("GetDeploymentHistory: %v", err)
	}
	if len(h.Events) != store.MaxHistoryEvents || !h.EventsTruncated {
		t.Fatalf("events = %d, truncated %v; want %d and truncated", len(h.Events), h.EventsTruncated, store.MaxHistoryEvents)
	}
	if first := h.Events[0]; first.Message != "note 22" {
		t.Errorf("first kept event = %+v, want note 22, the oldest of the newest %d", first, store.MaxHistoryEvents)
	}
	if last := h.Events[len(h.Events)-1]; last.Type != core.EventTransition || last.From != core.StateQueued || last.To != core.StateCancelled {
		t.Errorf("last event = %+v, want the terminal transition", last)
	}
	for i := 1; i < len(h.Events); i++ {
		if h.Events[i].ID <= h.Events[i-1].ID {
			t.Fatalf("events are not oldest first: %d then %d", h.Events[i-1].ID, h.Events[i].ID)
		}
	}
}

func TestGetDeploymentHistoryAtTheCap(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	d := mustCreate(t, st, newDeployment("web"), 0)
	mustExec(t, st, `
		INSERT INTO deployment_events (deployment_id, app_id, type, message)
		SELECT d.id, d.app_id, 'note', 'note ' || n
		FROM deployments d, generate_series(1, $2) n
		WHERE d.id = $1`, d.ID, store.MaxHistoryEvents-1)

	h, err := st.GetDeploymentHistory(t.Context(), d.ID)
	if err != nil {
		t.Fatalf("GetDeploymentHistory: %v", err)
	}
	if len(h.Events) != store.MaxHistoryEvents || h.EventsTruncated {
		t.Fatalf("events = %d, truncated %v; want %d and not truncated", len(h.Events), h.EventsTruncated, store.MaxHistoryEvents)
	}
	if h.Events[0].To != core.StateQueued || h.Events[len(h.Events)-1].Message != fmt.Sprintf("note %d", store.MaxHistoryEvents-1) {
		t.Errorf("events run from %+v to %+v", h.Events[0], h.Events[len(h.Events)-1])
	}
}

func TestGetDeploymentHistoryInsideTransaction(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	err := st.InTx(t.Context(), func(tx *store.Store) error {
		d, err := tx.CreateDeployment(t.Context(), newDeployment("web"), 0)
		if err != nil {
			return err
		}
		h, err := tx.GetDeploymentHistory(t.Context(), d.ID)
		if err != nil {
			return err
		}
		if h.Deployment.ID != d.ID || len(h.Events) != 1 {
			t.Errorf("history inside transaction = %+v", h)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("InTx: %v", err)
	}
}

func TestLastSuccessfulDeployment(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	web := core.Target{App: "web", Environment: "prod"}
	succeed := func(d core.Deployment, digest core.Digest) core.Deployment {
		t.Helper()
		d = mustAdvance(t, st, d, core.StateVerifying)
		got, err := st.TransitionDeployment(t.Context(), store.Transition{
			ID: d.ID, From: core.StateVerifying, To: core.StateSucceeded, Lineage: core.Lineage{Digest: digest},
		})
		if err != nil {
			t.Fatalf("succeed %d: %v", d.ID, err)
		}
		return got
	}

	if _, err := st.LastSuccessfulDeployment(t.Context(), web, ""); !errors.Is(err, core.ErrNotFound) {
		t.Errorf("no history: error = %v, want ErrNotFound", err)
	}

	first := succeed(mustCreate(t, st, newDeployment("web"), 0), testDigest)
	slow := mustCreate(t, st, newDeployment("web"), 0)
	rb := newDeployment("web")
	rb.Kind = core.KindRollback
	rb.RollbackOfID = &first.ID
	rollback := succeed(mustCreate(t, st, rb, 100), testDigest)
	slowDone := succeed(slow, otherDigest)
	mustCreateIn(t, st, newDeployment("web"), core.StateFailed)
	mustCreateIn(t, st, newDeployment("web"), core.StateCancelled)
	staging := newDeployment("web")
	staging.Target.Environment = "staging"
	mustCreateIn(t, st, staging, core.StateSucceeded)

	if !slowDone.FinishedAt.After(*rollback.FinishedAt) || slowDone.ID > rollback.ID {
		t.Fatalf("fixture: deployment %d finished %v, rollback %d finished %v", slowDone.ID, slowDone.FinishedAt, rollback.ID, rollback.FinishedAt)
	}

	tests := []struct {
		name    string
		target  core.Target
		exclude core.Digest
		want    core.DeploymentID
		wantErr error
	}{
		{name: "latest finish wins over latest id", target: web, want: slowDone.ID},
		{name: "exclude the bad digest finds the rollback", target: web, exclude: otherDigest, want: rollback.ID},
		{name: "exclude the rollback digest", target: web, exclude: testDigest, want: slowDone.ID},
		{name: "unknown target", target: core.Target{App: "nope", Environment: "prod"}, wantErr: core.ErrNotFound},
		{name: "invalid target", target: core.Target{App: "web"}},
		{name: "invalid digest", target: web, exclude: "latest", wantErr: core.ErrInvalidDigest},
	}
	t.Run("lookups", func(t *testing.T) {
		t.Parallel()
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()
				got, err := st.LastSuccessfulDeployment(t.Context(), tt.target, tt.exclude)
				if tt.want == 0 {
					if err == nil || (tt.wantErr != nil && !errors.Is(err, tt.wantErr)) {
						t.Errorf("error = %v, want %v", err, tt.wantErr)
					}
					return
				}
				if err != nil {
					t.Fatalf("LastSuccessfulDeployment: %v", err)
				}
				if got.ID != tt.want || got.State != core.StateSucceeded {
					t.Errorf("got deployment %d (%s), want %d", got.ID, got.State, tt.want)
				}
			})
		}
	})
	t.Run("deployments without digest", func(t *testing.T) {
		t.Parallel()
		ref := newDeployment("nodigest")
		pinned := succeed(mustCreate(t, st, ref, 0), testDigest)
		mustCreateIn(t, st, ref, core.StateSucceeded)
		for _, exclude := range []core.Digest{"", otherDigest} {
			got, err := st.LastSuccessfulDeployment(t.Context(), ref.Target, exclude)
			if err != nil || got.ID != pinned.ID {
				t.Errorf("exclude %q: got %d, %v; want %d, the latest with a digest", exclude, got.ID, err, pinned.ID)
			}
		}
		if _, err := st.LastSuccessfulDeployment(t.Context(), ref.Target, testDigest); !errors.Is(err, core.ErrNotFound) {
			t.Errorf("excluding the only digest: error = %v, want ErrNotFound", err)
		}
	})
}

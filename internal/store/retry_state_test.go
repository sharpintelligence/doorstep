package store_test

import (
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/store"
	"github.com/sharpintelligence/doorstep/internal/store/storetest"
)

func TestRetryState(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	d := mustCreateIn(t, st, newDeployment("web"), core.StateSyncing)
	mustExec(t, st, `UPDATE deployment_events SET occurred_at = now() - interval '90 minutes' WHERE deployment_id = $1 AND to_state = 'syncing'`, d.ID)

	got, err := st.RetryState(t.Context(), d.ID)
	if err != nil || got != (store.RetryState{}) {
		t.Fatalf("RetryState before retries = %+v, %v", got, err)
	}

	retries := []string{
		`{"retry":{"attempt":1,"delay_ms":60000,"max_attempts":3}}`,
		`{"note":"not a retry"}`,
		`{"retry":{"attempt":2,"delay_ms":120000,"max_attempts":4}}`,
	}
	for _, data := range retries {
		if _, err := st.TransitionDeployment(t.Context(), store.Transition{
			ID: d.ID, From: core.StateSyncing, To: core.StateSyncing, Actor: "doorstep", Message: "retry", Data: json.RawMessage(data),
		}); err != nil {
			t.Fatalf("self-transition: %v", err)
		}
	}

	got, err = st.RetryState(t.Context(), d.ID)
	want := store.RetryState{Retries: 2, Delay: 2 * time.Minute, MaxAttempts: 4}
	if err != nil || got != want {
		t.Fatalf("RetryState = %+v, %v, want %+v", got, err, want)
	}
	elapsed, err := st.TimeInState(t.Context(), d.ID, core.StateSyncing)
	if err != nil || elapsed > measureSlack {
		t.Fatalf("TimeInState after the self-transition = %v, %v, want a fresh start", elapsed, err)
	}
	if e := lastEvent(t, st, d.ID, core.EventTransition); e.from != "syncing" || e.to != "syncing" {
		t.Errorf("last event = %+v", e)
	}
	reloaded, err := st.GetDeployment(t.Context(), d.ID)
	if err != nil || reloaded.State != core.StateSyncing || reloaded.FinishedAt != nil {
		t.Errorf("deployment after retries = %+v, %v", reloaded, err)
	}

	other := mustCreateIn(t, st, newDeployment("api"), core.StateSyncing)
	if got, err := st.RetryState(t.Context(), other.ID); err != nil || got.Retries != 0 {
		t.Errorf("another deployment's RetryState = %+v, %v", got, err)
	}
}

func TestDiagnoses(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	withDigest := func(app string, digest core.Digest, state core.State) core.Deployment {
		d := mustCreateIn(t, st, deploymentOf(app, randomSHA(t)), state)
		mustExec(t, st, `UPDATE deployments SET image_digest = $2 WHERE id = $1`, d.ID, digest)
		return d
	}
	record := func(d core.Deployment, data string, age time.Duration) {
		mustExec(t, st, `INSERT INTO deployment_events (deployment_id, app_id, type, from_state, to_state, actor, data, occurred_at)
			SELECT id, app_id, 'transition', 'syncing', 'syncing', 'doorstep', $2::jsonb, now() - $3::interval FROM deployments WHERE id = $1`, d.ID, json.RawMessage(data), age)
	}

	recent := withDigest("web", otherDigest, core.StateFailed)
	record(recent, `{"diagnosis":{"nodes":["pi-1"]}}`, 24*time.Hour)
	stale := withDigest("web", thirdDigest, core.StateFailed)
	record(stale, `{"diagnosis":{"nodes":["pi-2"]}}`, 8*24*time.Hour)
	sameDigest := withDigest("web", testDigest, core.StateFailed)
	record(sameDigest, `{"diagnosis":{"nodes":["pi-3"]}}`, time.Hour)
	otherApp := withDigest("api", otherDigest, core.StateFailed)
	record(otherApp, `{"diagnosis":{"nodes":["pi-4"]}}`, time.Hour)
	recentAgain := withDigest("web", thirdDigest, core.StateFailed)
	record(recentAgain, `{"diagnosis":{"nodes":["pi-5"]}}`, 2*time.Hour)
	record(recentAgain, `{"diagnosis":"not an object"}`, time.Hour)

	cur := withDigest("web", testDigest, core.StateSyncing)
	record(cur, `{"diagnosis":{"attempt":1}}`, 20*time.Minute)
	record(cur, `{"retry":{"attempt":2}}`, 10*time.Minute)
	record(cur, `{"diagnosis":{"attempt":2}}`, 5*time.Minute)
	later := withDigest("web", otherDigest, core.StateCancelled)
	record(later, `{"diagnosis":{"nodes":["pi-6"]}}`, time.Minute)

	got, err := st.Diagnoses(t.Context(), cur.ID, 0, 0)
	if err != nil {
		t.Fatalf("Diagnoses: %v", err)
	}
	if want := []string{`{"attempt": 1}`, `{"attempt": 2}`}; !slices.Equal(texts(got.Deployment), want) {
		t.Errorf("deployment diagnoses = %v, want %v", texts(got.Deployment), want)
	}
	if want := []string{`{"nodes": ["pi-1"]}`, `{"nodes": ["pi-5"]}`}; !slices.Equal(texts(got.Recent), want) {
		t.Errorf("recent diagnoses = %v, want %v", texts(got.Recent), want)
	}

	limited, err := st.Diagnoses(t.Context(), cur.ID, 3*time.Hour, 1)
	if err != nil {
		t.Fatalf("Diagnoses with a limit: %v", err)
	}
	if len(limited.Deployment) != 1 || len(limited.Recent) != 0 || string(limited.Deployment[0]) != `{"attempt": 2}` {
		t.Errorf("limited diagnoses = %v %v, want only the newest", texts(limited.Deployment), texts(limited.Recent))
	}

	none, err := st.Diagnoses(t.Context(), cur.ID+later.ID+1000, 0, 0)
	if err != nil || len(none.Deployment)+len(none.Recent) != 0 {
		t.Errorf("unknown deployment diagnoses = %+v, %v", none, err)
	}
}

func texts(raw []json.RawMessage) []string {
	out := make([]string, 0, len(raw))
	for _, r := range raw {
		out = append(out, string(r))
	}
	return out
}

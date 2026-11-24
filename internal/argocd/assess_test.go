package argocd

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sharpintelligence/doorstep/internal/argocd/argocdtest"
	"github.com/sharpintelligence/doorstep/internal/core"
)

const (
	repository = "acme/gitops"
	repoURL    = "https://github.com/acme/gitops.git"
	pinSHA     = "1111111111111111111111111111111111111111"
	laterSHA   = "2222222222222222222222222222222222222222"
	otherSHA   = "3333333333333333333333333333333333333333"
	chartURL   = "https://charts.example.com"
)

func decode(t *testing.T, app argocdtest.Application) application {
	t.Helper()
	raw, err := json.Marshal(app.Object(7))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out application
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return out
}

func synced(revision string) argocdtest.Application {
	return argocdtest.Synced("argocd", "blog", repoURL, revision)
}

func with(app argocdtest.Application, fn func(*argocdtest.Application)) argocdtest.Application {
	fn(&app)
	return app
}

func degradedResources(n int, message string) []argocdtest.Resource {
	out := make([]argocdtest.Resource, 0, n)
	for i := range n {
		out = append(out, argocdtest.Resource{
			Group: "apps", Kind: "Deployment", Namespace: "blog", Name: "web-" + strconv.Itoa(i),
			Status: "Synced", Health: "Degraded", HealthMessage: message,
		})
	}
	return out
}

func TestAssess(t *testing.T) {
	t.Parallel()

	reconciled := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	now := time.Date(2026, 5, 1, 12, 5, 0, 0, time.UTC)
	long := strings.Repeat("é", 600)

	tests := []struct {
		name          string
		app           argocdtest.Application
		mutate        func(*application)
		pin           string
		contained     map[string]bool
		wantReady     bool
		wantReached   bool
		wantState     core.SyncState
		wantHealth    core.HealthStatus
		wantRevision  string
		wantOperation string
		wantReason    core.SyncFailureReason
		wantClass     core.FailureClass
		wantMessage   string
		wantResources int
		check         func(*testing.T, core.SyncStatus)
	}{
		{
			name:          "synced to the pin and healthy",
			app:           with(synced(pinSHA), func(a *argocdtest.Application) { a.ReconciledAt = reconciled }),
			wantReady:     true,
			wantReached:   true,
			wantState:     core.SyncSynced,
			wantHealth:    core.HealthHealthy,
			wantRevision:  pinSHA,
			wantOperation: core.OperationSucceeded,
			check: func(t *testing.T, s core.SyncStatus) {
				t.Helper()
				if !s.Health.ObservedAt.Equal(reconciled) {
					t.Errorf("ObservedAt = %s, want %s", s.Health.ObservedAt, reconciled)
				}
			},
		},
		{
			name:          "later commit that contains the pin",
			app:           synced(laterSHA),
			contained:     map[string]bool{laterSHA: true},
			wantReady:     true,
			wantReached:   true,
			wantState:     core.SyncSynced,
			wantHealth:    core.HealthHealthy,
			wantRevision:  laterSHA,
			wantOperation: core.OperationSucceeded,
			check: func(t *testing.T, s core.SyncStatus) {
				t.Helper()
				if !s.Health.ObservedAt.Equal(now) {
					t.Errorf("ObservedAt = %s, want now", s.Health.ObservedAt)
				}
			},
		},
		{
			name:          "older or diverged commit",
			app:           synced(otherSHA),
			contained:     map[string]bool{otherSHA: false},
			wantState:     core.SyncSynced,
			wantHealth:    core.HealthHealthy,
			wantRevision:  otherSHA,
			wantOperation: core.OperationSucceeded,
			wantReason:    core.SyncReasonNotObserved,
			wantClass:     core.FailureTransient,
			wantMessage:   "synced revision " + otherSHA + " does not contain " + pinSHA + " yet",
		},
		{
			name:         "no revision yet",
			app:          with(synced(""), func(a *argocdtest.Application) { a.Operation = nil; a.Sync = "Unknown" }),
			wantState:    core.SyncUnknown,
			wantHealth:   core.HealthHealthy,
			wantReason:   core.SyncReasonNotObserved,
			wantClass:    core.FailureTransient,
			wantMessage:  "Argo CD has not reported a synced revision yet",
			wantRevision: "",
		},
		{
			name: "comparison error while not observed",
			app: with(synced(otherSHA), func(a *argocdtest.Application) {
				a.Conditions = []argocdtest.Condition{{Type: "ComparisonError", Message: "repository not accessible"}}
			}),
			contained:     map[string]bool{otherSHA: false},
			wantState:     core.SyncSynced,
			wantHealth:    core.HealthHealthy,
			wantRevision:  otherSHA,
			wantOperation: core.OperationSucceeded,
			wantReason:    core.SyncReasonNotObserved,
			wantClass:     core.FailureTransient,
			wantMessage:   "; ComparisonError: repository not accessible",
		},
		{
			name: "multi-source with a Helm chart",
			app: with(synced(""), func(a *argocdtest.Application) {
				a.Sources = []string{chartURL, "git@github.com:Acme/GitOps.git"}
				a.Revisions = []string{"4.5.6", laterSHA}
				a.Operation.Revisions = []string{"4.5.6", laterSHA}
			}),
			contained:     map[string]bool{laterSHA: true},
			wantReady:     true,
			wantReached:   true,
			wantState:     core.SyncSynced,
			wantHealth:    core.HealthHealthy,
			wantRevision:  "4.5.6, " + laterSHA,
			wantOperation: core.OperationSucceeded,
			check: func(t *testing.T, s core.SyncStatus) {
				t.Helper()
				if len(s.Revisions) != 2 || s.Revisions[0] != "4.5.6" {
					t.Errorf("Revisions = %v", s.Revisions)
				}
			},
		},
		{
			name: "multi-source without a matching repository uses every commit",
			app: with(synced(""), func(a *argocdtest.Application) {
				a.Sources = []string{chartURL, "https://github.com/acme/values"}
				a.Revisions = []string{"4.5.6", pinSHA}
				a.Operation.Revisions = []string{"4.5.6", pinSHA}
			}),
			wantReady:     true,
			wantReached:   true,
			wantState:     core.SyncSynced,
			wantHealth:    core.HealthHealthy,
			wantRevision:  "4.5.6, " + pinSHA,
			wantOperation: core.OperationSucceeded,
		},
		{
			name: "multi-source only checks the gitops source",
			app: with(synced(""), func(a *argocdtest.Application) {
				a.Sources = []string{"ssh://git@github.com/acme/gitops", "https://github.com/acme/other"}
				a.Revisions = []string{otherSHA, pinSHA}
				a.Operation.Revisions = []string{otherSHA, pinSHA}
			}),
			contained:     map[string]bool{otherSHA: false},
			wantState:     core.SyncSynced,
			wantHealth:    core.HealthHealthy,
			wantRevision:  otherSHA + ", " + pinSHA,
			wantOperation: core.OperationSucceeded,
			wantReason:    core.SyncReasonNotObserved,
			wantClass:     core.FailureTransient,
		},
		{
			name:          "out of sync with automated sync",
			app:           with(synced(pinSHA), func(a *argocdtest.Application) { a.Sync = "OutOfSync" }),
			wantReached:   true,
			wantState:     core.SyncOutOfSync,
			wantHealth:    core.HealthHealthy,
			wantRevision:  pinSHA,
			wantOperation: core.OperationSucceeded,
			wantReason:    core.SyncReasonOutOfSync,
			wantClass:     core.FailureUnknown,
			wantMessage:   "application is OutOfSync",
		},
		{
			name:          "out of sync with manual sync",
			app:           with(synced(pinSHA), func(a *argocdtest.Application) { a.Sync = "OutOfSync"; a.Automated = false }),
			wantReached:   true,
			wantState:     core.SyncOutOfSync,
			wantHealth:    core.HealthHealthy,
			wantRevision:  pinSHA,
			wantOperation: core.OperationSucceeded,
			wantReason:    core.SyncReasonOutOfSync,
			wantClass:     core.FailureConfiguration,
			wantMessage:   "automated sync is disabled",
		},
		{
			name: "automated sync switched off with enabled false",
			app:  with(synced(pinSHA), func(a *argocdtest.Application) { a.Sync = "OutOfSync" }),
			mutate: func(a *application) {
				disabled := false
				a.Spec.SyncPolicy.Automated.Enabled = &disabled
			},
			wantReached:   true,
			wantState:     core.SyncOutOfSync,
			wantHealth:    core.HealthHealthy,
			wantRevision:  pinSHA,
			wantOperation: core.OperationSucceeded,
			wantReason:    core.SyncReasonOutOfSync,
			wantClass:     core.FailureConfiguration,
		},
		{
			name: "sync operation running",
			app: with(synced(pinSHA), func(a *argocdtest.Application) {
				a.Operation = &argocdtest.Operation{Phase: "Running", Message: "waiting for completion of hook batch/Job/migrate"}
			}),
			wantReached:   true,
			wantState:     core.SyncSynced,
			wantHealth:    core.HealthHealthy,
			wantRevision:  pinSHA,
			wantOperation: core.OperationRunning,
			wantReason:    core.SyncReasonRunning,
			wantClass:     core.FailureUnknown,
			wantMessage:   "still running: waiting for completion of hook",
		},
		{
			name: "stale failed operation from an older revision is ignored",
			app: with(synced(pinSHA), func(a *argocdtest.Application) {
				a.Operation = &argocdtest.Operation{Phase: "Failed", Message: "old failure", Revision: otherSHA}
			}),
			wantReady:    true,
			wantReached:  true,
			wantState:    core.SyncSynced,
			wantHealth:   core.HealthHealthy,
			wantRevision: pinSHA,
		},
		{
			name: "failed operation before a sync result on the pin",
			app: with(synced(pinSHA), func(a *argocdtest.Application) {
				a.Sync = "OutOfSync"
				a.Operation = &argocdtest.Operation{
					Phase: "Failed", Message: "ComparisonError: failed to generate manifest", Revision: pinSHA, WithoutResult: true,
				}
			}),
			wantReached:   true,
			wantState:     core.SyncOutOfSync,
			wantHealth:    core.HealthHealthy,
			wantRevision:  pinSHA,
			wantOperation: core.OperationFailed,
			wantReason:    core.SyncReasonFailed,
			wantClass:     core.FailureVersion,
			wantMessage:   "sync operation failed: ComparisonError: failed to generate manifest",
		},
		{
			name: "failed operation before a sync result on an older revision is ignored",
			app: with(synced(pinSHA), func(a *argocdtest.Application) {
				a.Operation = &argocdtest.Operation{Phase: "Failed", Message: "old failure", Revision: otherSHA, WithoutResult: true}
			}),
			wantReady:    true,
			wantReached:  true,
			wantState:    core.SyncSynced,
			wantHealth:   core.HealthHealthy,
			wantRevision: pinSHA,
		},
		{
			name: "failed operation without a requested revision is ignored",
			app: with(synced(pinSHA), func(a *argocdtest.Application) {
				a.Operation = &argocdtest.Operation{Phase: "Failed", Message: "old failure", WithoutResult: true}
			}),
			wantReady:    true,
			wantReached:  true,
			wantState:    core.SyncSynced,
			wantHealth:   core.HealthHealthy,
			wantRevision: pinSHA,
		},
		{
			name: "multi-source failed operation before a sync result",
			app: with(synced(""), func(a *argocdtest.Application) {
				a.Sync = "OutOfSync"
				a.Sources = []string{chartURL, repoURL}
				a.Revisions = []string{"4.5.6", pinSHA}
				a.Operation = &argocdtest.Operation{Phase: "Failed", Message: "manifest generation failed", Revisions: []string{"4.5.6", pinSHA}, WithoutResult: true}
			}),
			wantReached:   true,
			wantState:     core.SyncOutOfSync,
			wantHealth:    core.HealthHealthy,
			wantRevision:  "4.5.6, " + pinSHA,
			wantOperation: core.OperationFailed,
			wantReason:    core.SyncReasonFailed,
			wantClass:     core.FailureVersion,
			wantMessage:   "manifest generation failed",
		},
		{
			name: "single-source Helm chart can never reach a commit",
			app: with(synced("4.5.6"), func(a *argocdtest.Application) {
				a.RepoURL = chartURL
			}),
			wantState:     core.SyncSynced,
			wantHealth:    core.HealthHealthy,
			wantRevision:  "4.5.6",
			wantOperation: core.OperationSucceeded,
			wantReason:    core.SyncReasonUntracked,
			wantClass:     core.FailureConfiguration,
			wantMessage:   "revision 4.5.6, which is not a Git commit",
		},
		{
			name: "health read before the operation finished",
			app: with(synced(pinSHA), func(a *argocdtest.Application) {
				a.ReconciledAt = reconciled
				a.Operation.FinishedAt = reconciled.Add(time.Second)
			}),
			wantReached:   true,
			wantState:     core.SyncSynced,
			wantHealth:    core.HealthHealthy,
			wantRevision:  pinSHA,
			wantOperation: core.OperationSucceeded,
			wantReason:    core.SyncReasonProgressing,
			wantClass:     core.FailureUnknown,
			wantMessage:   "reassess health after the sync operation finished",
			check: func(t *testing.T, s core.SyncStatus) {
				t.Helper()
				if !s.HealthStale {
					t.Error("HealthStale = false, want true")
				}
			},
		},
		{
			name: "health read when the operation finished",
			app: with(synced(pinSHA), func(a *argocdtest.Application) {
				a.ReconciledAt = reconciled
				a.Operation.FinishedAt = reconciled
			}),
			wantReady:     true,
			wantReached:   true,
			wantState:     core.SyncSynced,
			wantHealth:    core.HealthHealthy,
			wantRevision:  pinSHA,
			wantOperation: core.OperationSucceeded,
		},
		{
			name: "operation finish time without a reconcile time is not stale",
			app: with(synced(pinSHA), func(a *argocdtest.Application) {
				a.Operation.FinishedAt = reconciled
			}),
			wantReady:     true,
			wantReached:   true,
			wantState:     core.SyncSynced,
			wantHealth:    core.HealthHealthy,
			wantRevision:  pinSHA,
			wantOperation: core.OperationSucceeded,
		},
		{
			name: "failed operation on the pin",
			app: with(synced(pinSHA), func(a *argocdtest.Application) {
				a.Sync = "OutOfSync"
				a.Health = "Degraded"
				a.Operation = &argocdtest.Operation{
					Phase: "Failed", Message: "one or more objects failed to apply", Revision: pinSHA,
					Resources: []argocdtest.SyncResource{
						{Group: "apps", Kind: "Deployment", Namespace: "blog", Name: "web", Status: "SyncFailed", Message: "admission webhook denied"},
						{Kind: "Service", Namespace: "blog", Name: "web", Status: "Synced"},
						{Group: "batch", Kind: "Job", Namespace: "blog", Name: "migrate", Status: "Synced", HookPhase: "Failed", Message: "job failed"},
					},
				}
				a.Resources = []argocdtest.Resource{{Group: "apps", Kind: "Deployment", Namespace: "blog", Name: "web", Status: "OutOfSync", Health: "Degraded", HealthMessage: "rollout stuck"}}
			}),
			wantReached:   true,
			wantState:     core.SyncOutOfSync,
			wantHealth:    core.HealthDegraded,
			wantRevision:  pinSHA,
			wantOperation: core.OperationFailed,
			wantReason:    core.SyncReasonFailed,
			wantClass:     core.FailureVersion,
			wantMessage:   "sync operation failed: one or more objects failed to apply",
			wantResources: 2,
			check: func(t *testing.T, s core.SyncStatus) {
				t.Helper()
				first := s.Failure.Resources[0]
				if first.Kind != "Deployment" || first.Message != "admission webhook denied" || first.Sync != "SyncFailed" {
					t.Errorf("first resource = %+v", first)
				}
				if job := s.Failure.Resources[1]; job.Kind != "Job" || job.Message != "job failed" {
					t.Errorf("second resource = %+v", job)
				}
			},
		},
		{
			name: "errored operation on the pin",
			app: with(synced(pinSHA), func(a *argocdtest.Application) {
				a.Sync = "OutOfSync"
				a.Operation = &argocdtest.Operation{Phase: "Error", Message: "rpc error: repository unavailable", Revision: pinSHA}
			}),
			wantReached:   true,
			wantState:     core.SyncOutOfSync,
			wantHealth:    core.HealthHealthy,
			wantRevision:  pinSHA,
			wantOperation: core.OperationError,
			wantReason:    core.SyncReasonError,
			wantClass:     core.FailureTransient,
			wantMessage:   "sync operation errored: rpc error",
		},
		{
			name: "sync error condition",
			app: with(synced(pinSHA), func(a *argocdtest.Application) {
				a.Health = "Progressing"
				a.Conditions = []argocdtest.Condition{{Type: "OrphanedResourceWarning", Message: "ignored"}, {Type: "SyncError", Message: "failed to sync"}}
			}),
			wantReached:   true,
			wantState:     core.SyncSynced,
			wantHealth:    core.HealthProgressing,
			wantRevision:  pinSHA,
			wantOperation: core.OperationSucceeded,
			wantReason:    core.SyncReasonError,
			wantClass:     core.FailureTransient,
			wantMessage:   "SyncError: failed to sync",
		},
		{
			name: "degraded with resource reasons",
			app: with(synced(pinSHA), func(a *argocdtest.Application) {
				a.Health = "Degraded"
				a.Resources = []argocdtest.Resource{
					{Kind: "Service", Namespace: "blog", Name: "web", Status: "Synced", Health: "Healthy"},
					{Kind: "ConfigMap", Namespace: "blog", Name: "settings", Status: "Synced"},
					{Group: "apps", Kind: "Deployment", Namespace: "blog", Name: "web", Status: "Synced", Health: "Degraded", HealthMessage: "Deployment exceeded its progress deadline"},
				}
			}),
			wantReached:   true,
			wantState:     core.SyncSynced,
			wantHealth:    core.HealthDegraded,
			wantRevision:  pinSHA,
			wantOperation: core.OperationSucceeded,
			wantReason:    core.SyncReasonDegraded,
			wantClass:     core.FailureVersion,
			wantMessage:   "application health is degraded; Deployment web: Deployment exceeded its progress deadline",
			wantResources: 1,
		},
		{
			name:          "missing",
			app:           with(synced(pinSHA), func(a *argocdtest.Application) { a.Health = "Missing" }),
			wantReached:   true,
			wantState:     core.SyncSynced,
			wantHealth:    core.HealthMissing,
			wantRevision:  pinSHA,
			wantOperation: core.OperationSucceeded,
			wantReason:    core.SyncReasonMissing,
			wantClass:     core.FailureUnknown,
		},
		{
			name:          "suspended",
			app:           with(synced(pinSHA), func(a *argocdtest.Application) { a.Health = "Suspended"; a.HealthMessage = "paused" }),
			wantReached:   true,
			wantState:     core.SyncSynced,
			wantHealth:    core.HealthSuspended,
			wantRevision:  pinSHA,
			wantOperation: core.OperationSucceeded,
			wantReason:    core.SyncReasonSuspended,
			wantClass:     core.FailureUnknown,
			wantMessage:   "application health is suspended: paused",
		},
		{
			name:          "progressing",
			app:           with(synced(pinSHA), func(a *argocdtest.Application) { a.Health = "Progressing" }),
			wantReached:   true,
			wantState:     core.SyncSynced,
			wantHealth:    core.HealthProgressing,
			wantRevision:  pinSHA,
			wantOperation: core.OperationSucceeded,
			wantReason:    core.SyncReasonProgressing,
			wantClass:     core.FailureUnknown,
		},
		{
			name:          "unknown health",
			app:           with(synced(pinSHA), func(a *argocdtest.Application) { a.Health = "" }),
			wantReached:   true,
			wantState:     core.SyncSynced,
			wantHealth:    core.HealthUnknown,
			wantRevision:  pinSHA,
			wantOperation: core.OperationSucceeded,
			wantReason:    core.SyncReasonProgressing,
			wantClass:     core.FailureUnknown,
		},
		{
			name: "resources are capped and messages cut",
			app: with(synced(pinSHA), func(a *argocdtest.Application) {
				a.Health = "Degraded"
				a.Resources = degradedResources(30, long)
			}),
			wantReached:   true,
			wantState:     core.SyncSynced,
			wantHealth:    core.HealthDegraded,
			wantRevision:  pinSHA,
			wantOperation: core.OperationSucceeded,
			wantReason:    core.SyncReasonDegraded,
			wantClass:     core.FailureVersion,
			wantResources: MaxResources,
			check: func(t *testing.T, s core.SyncStatus) {
				t.Helper()
				for _, r := range s.Failure.Resources {
					if len(r.Message) > MaxMessageBytes || !strings.HasPrefix(long, r.Message) {
						t.Fatalf("message of %d bytes is not a clean prefix", len(r.Message))
					}
				}
				if len(s.Failure.Message) > MaxMessageBytes {
					t.Fatalf("failure message is %d bytes", len(s.Failure.Message))
				}
			},
		},
		{
			name:          "no pin reports status only",
			app:           with(synced(otherSHA), func(a *argocdtest.Application) { a.Health = "Degraded" }),
			pin:           "-",
			wantState:     core.SyncSynced,
			wantHealth:    core.HealthDegraded,
			wantRevision:  otherSHA,
			wantOperation: core.OperationSucceeded,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			app := decode(t, tt.app)
			if tt.mutate != nil {
				tt.mutate(&app)
			}
			pin := pinSHA
			if tt.pin == "-" {
				pin = ""
			}
			contains := func(revision string) bool {
				if err := core.ValidateCommitSHA(revision); err != nil || revision == pin {
					t.Errorf("contains(%q) called", revision)
				}
				contained, ok := tt.contained[revision]
				if !ok {
					t.Errorf("unexpected contains(%q)", revision)
				}
				return contained
			}
			got := assess(app, repository, pin, contains, now)

			if got.Ready() != tt.wantReady || got.Reached != tt.wantReached {
				t.Errorf("Ready = %v, Reached = %v, want %v, %v", got.Ready(), got.Reached, tt.wantReady, tt.wantReached)
			}
			if got.State != tt.wantState || got.Health.Status != tt.wantHealth {
				t.Errorf("State = %s, Health = %s, want %s, %s", got.State, got.Health.Status, tt.wantState, tt.wantHealth)
			}
			if got.Revision != tt.wantRevision || got.OperationPhase != tt.wantOperation {
				t.Errorf("Revision = %q, OperationPhase = %q, want %q, %q", got.Revision, got.OperationPhase, tt.wantRevision, tt.wantOperation)
			}
			switch {
			case tt.wantReason == "" && got.Failure != nil:
				t.Errorf("Failure = %+v, want none", got.Failure)
			case tt.wantReason != "" && got.Failure == nil:
				t.Fatalf("Failure = nil, want %s", tt.wantReason)
			case tt.wantReason != "":
				if got.Failure.Reason != tt.wantReason || got.Failure.Class != tt.wantClass {
					t.Errorf("Failure = %s/%s, want %s/%s", got.Failure.Reason, got.Failure.Class, tt.wantReason, tt.wantClass)
				}
				if !strings.Contains(got.Failure.Message, tt.wantMessage) {
					t.Errorf("Failure.Message = %q, want it to contain %q", got.Failure.Message, tt.wantMessage)
				}
				if len(got.Failure.Resources) != tt.wantResources {
					t.Errorf("Failure.Resources = %d, want %d: %+v", len(got.Failure.Resources), tt.wantResources, got.Failure.Resources)
				}
			}
			if tt.check != nil {
				tt.check(t, got)
			}
		})
	}
}

func TestAssessWithoutAncestryUsesEquality(t *testing.T) {
	t.Parallel()

	got := assess(decode(t, synced(laterSHA)), repository, pinSHA, nil, time.Now())
	if got.Reached || got.Failure == nil || got.Failure.Reason != core.SyncReasonNotObserved {
		t.Fatalf("assess = %+v", got)
	}
}

func TestNormaliseRepo(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in   string
		want string
	}{
		{"https://github.com/Acme/GitOps", "acme/gitops"},
		{"https://github.com/acme/gitops.git", "acme/gitops"},
		{"https://github.com/acme/gitops/", "acme/gitops"},
		{"https://github.example.com/acme/gitops.git", "acme/gitops"},
		{"git@github.com:acme/gitops.git", "acme/gitops"},
		{"git@github.com:acme/gitops", "acme/gitops"},
		{"ssh://git@github.com/acme/gitops.git", "acme/gitops"},
		{"ssh://git@github.com:22/acme/gitops", "acme/gitops"},
		{"https://gitlab.com/group/sub/project", ""},
		{"oci://ghcr.io/acme/charts", ""},
		{"ghcr.io/acme/charts", ""},
		{"https://charts.example.com", ""},
		{"git@github.com", ""},
		{"", ""},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			t.Parallel()
			if got := normaliseRepo(tt.in); got != tt.want {
				t.Fatalf("normaliseRepo(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

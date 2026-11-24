package retry_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/engine"
	"github.com/sharpintelligence/doorstep/internal/retry"
)

const (
	digestNew = core.Digest("sha256:2222222222222222222222222222222222222222222222222222222222222222")
	digestOld = core.Digest("sha256:1111111111111111111111111111111111111111111111111111111111111111")
	image     = "ghcr.io/example-user/blog@"
)

func deployment() core.Deployment {
	return core.Deployment{ID: 42, Target: core.AppTarget("blog"), Kind: core.KindDeploy, State: core.StateSyncing, Digest: digestNew, PreviousDigest: digestOld, PinCommitSHA: "89abcdef0123456789abcdef0123456789abcdef"}
}

func syncFailure(reason core.SyncFailureReason, class core.FailureClass, message string) engine.Failure {
	return engine.Failure{
		Deployment: deployment(),
		Stage:      core.StateSyncing,
		Class:      class,
		Reason:     "Argo CD did not sync and become healthy within 10m0s",
		Sync:       &core.SyncFailure{Reason: reason, Class: class, Message: message},
		Attempt:    1,
	}
}

func degraded(message string) engine.Failure {
	return syncFailure(core.SyncReasonDegraded, core.FailureVersion, message)
}

type podOption func(*core.PodStatus)

func pod(name, node string, digest core.Digest, opts ...podOption) core.PodStatus {
	p := core.PodStatus{Namespace: "blog", Name: name, Node: node, Images: []string{image + string(digest)}, Phase: "Running", Scheduled: node != ""}
	for _, opt := range opts {
		opt(&p)
	}
	return p
}

func ready(p *core.PodStatus) {
	p.Ready = true
	p.Containers = []core.ContainerStatus{{Name: "app", Ready: true}}
}

func waiting(reason string) podOption {
	return func(p *core.PodStatus) {
		p.Containers = append(p.Containers, core.ContainerStatus{Name: "app", Waiting: reason, Message: reason + " detail"})
	}
}

func crashing(exitCode int) podOption {
	return func(p *core.PodStatus) {
		p.Containers = append(p.Containers, core.ContainerStatus{Name: "app", Restarts: 4, Waiting: "CrashLoopBackOff", Terminated: "Error", ExitCode: exitCode})
	}
}

func oomKilled(p *core.PodStatus) {
	p.Containers = append(p.Containers, core.ContainerStatus{Name: "app", Restarts: 2, Waiting: "CrashLoopBackOff", Terminated: "OOMKilled", ExitCode: 137})
}

func pending(p *core.PodStatus) {
	p.Phase = "Pending"
	p.Scheduled = false
	p.Node = ""
	p.Message = "0/3 nodes are available: 3 Insufficient memory."
}

func reason(r string) podOption {
	return func(p *core.PodStatus) {
		p.Phase = "Failed"
		p.Reason = r
	}
}

func terminating(phase string) podOption {
	return func(p *core.PodStatus) {
		p.Terminating = true
		p.Phase = phase
	}
}

func TestClassify(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		in           retry.Input
		wantVerdict  core.FailureClass
		wantSignals  []retry.Signal
		wantNodes    []string
		wantSummary  string
		wantEvidence string
	}{
		{
			name:        "failure outside syncing keeps the engine's class",
			in:          retry.Input{Failure: engine.Failure{Deployment: deployment(), Stage: core.StateVerifying, Class: core.FailureVersion, Reason: "after gates failed: health"}},
			wantVerdict: core.FailureVersion,
			wantSummary: "version: after gates failed: health",
		},
		{
			name:        "syncing failure without an Argo CD failure",
			in:          retry.Input{Failure: engine.Failure{Deployment: deployment(), Stage: core.StateSyncing, Class: core.FailureConfiguration, Reason: "app blog is not configured"}},
			wantVerdict: core.FailureConfiguration,
		},
		{
			name:        "application not found",
			in:          retry.Input{Failure: syncFailure(core.SyncReasonNotFound, core.FailureConfiguration, "application argocd/blog not found")},
			wantVerdict: core.FailureConfiguration,
			wantSummary: "configuration: not_found: application argocd/blog not found",
		},
		{
			name:        "forbidden",
			in:          retry.Input{Failure: syncFailure(core.SyncReasonForbidden, core.FailureConfiguration, "rbac")},
			wantVerdict: core.FailureConfiguration,
		},
		{
			name:        "untracked revision",
			in:          retry.Input{Failure: syncFailure(core.SyncReasonUntracked, core.FailureConfiguration, "HEAD")},
			wantVerdict: core.FailureConfiguration,
		},
		{
			name:        "out of sync with manual sync",
			in:          retry.Input{Failure: syncFailure(core.SyncReasonOutOfSync, core.FailureConfiguration, "sync by hand")},
			wantVerdict: core.FailureConfiguration,
		},
		{
			name:        "out of sync with automated sync",
			in:          retry.Input{Failure: syncFailure(core.SyncReasonOutOfSync, core.FailureUnknown, "application is OutOfSync")},
			wantVerdict: core.FailureUnknown,
		},
		{
			name:        "sync failed is the version even when pods look transient",
			in:          retry.Input{Failure: syncFailure(core.SyncReasonFailed, core.FailureVersion, "migration hook failed"), Pods: []core.PodStatus{pod("a", "pi-1", digestNew, waiting("ImagePullBackOff"))}},
			wantVerdict: core.FailureVersion,
			wantSummary: "version: Argo CD sync failed: migration hook failed",
		},
		{
			name:        "degraded without signals is unknown",
			in:          retry.Input{Failure: degraded("application health is degraded")},
			wantVerdict: core.FailureUnknown,
			wantSummary: "unknown: degraded: application health is degraded",
		},
		{
			name:        "progressing without signals",
			in:          retry.Input{Failure: syncFailure(core.SyncReasonProgressing, core.FailureUnknown, "application health is progressing")},
			wantVerdict: core.FailureUnknown,
		},
		{
			name:        "unavailable without signals keeps its transient class",
			in:          retry.Input{Failure: syncFailure(core.SyncReasonUnavailable, core.FailureTransient, "connection refused")},
			wantVerdict: core.FailureTransient,
		},
		{
			name:        "image pull",
			in:          retry.Input{Failure: degraded("x"), Pods: []core.PodStatus{pod("a", "pi-3", digestNew, waiting("ErrImagePull")), pod("b", "pi-3", digestNew, waiting("ImagePullBackOff"))}},
			wantVerdict: core.FailureTransient,
			wantSignals: []retry.Signal{retry.SignalImagePull, retry.SignalImagePull},
			wantNodes:   []string{"pi-3"},
			wantSummary: "transient: image pull failing on node pi-3",
		},
		{
			name:        "unschedulable",
			in:          retry.Input{Failure: degraded("x"), Pods: []core.PodStatus{pod("a", "", digestNew, pending)}},
			wantVerdict: core.FailureTransient,
			wantSignals: []retry.Signal{retry.SignalUnschedulable},
			wantSummary: "transient: pods cannot be scheduled",
		},
		{
			name:        "evicted",
			in:          retry.Input{Failure: degraded("x"), Pods: []core.PodStatus{pod("a", "pi-1", digestNew, reason("Evicted"))}},
			wantVerdict: core.FailureTransient,
			wantSignals: []retry.Signal{retry.SignalEvicted},
		},
		{
			name:        "node lost",
			in:          retry.Input{Failure: degraded("x"), Pods: []core.PodStatus{pod("a", "pi-2", digestNew, reason("NodeLost"))}},
			wantVerdict: core.FailureTransient,
			wantSignals: []retry.Signal{retry.SignalNodeLost},
			wantSummary: "transient: node lost on node pi-2",
		},
		{
			name:        "terminating on an unreachable node",
			in:          retry.Input{Failure: degraded("x"), Pods: []core.PodStatus{pod("a", "pi-2", digestNew, terminating("Unknown"))}},
			wantVerdict: core.FailureTransient,
			wantSignals: []retry.Signal{retry.SignalNodeLost},
		},
		{
			name:        "terminating pods are otherwise ignored",
			in:          retry.Input{Failure: degraded("x"), Pods: []core.PodStatus{pod("a", "pi-2", digestNew, terminating("Running"))}},
			wantVerdict: core.FailureUnknown,
		},
		{
			name:        "container creating",
			in:          retry.Input{Failure: degraded("x"), Pods: []core.PodStatus{pod("a", "pi-1", digestNew, waiting("ContainerCreating"))}},
			wantVerdict: core.FailureTransient,
			wantSignals: []retry.Signal{retry.SignalContainerCreating},
		},
		{
			name:        "running without restarts but not ready is a probe timeout",
			in:          retry.Input{Failure: syncFailure(core.SyncReasonProgressing, core.FailureUnknown, "x"), Pods: []core.PodStatus{pod("a", "pi-1", digestNew)}},
			wantVerdict: core.FailureTransient,
			wantSignals: []retry.Signal{retry.SignalNotReady},
			wantSummary: "transient: pods running but not ready on node pi-1",
		},
		{
			name:        "invalid image name is the version",
			in:          retry.Input{Failure: degraded("x"), Pods: []core.PodStatus{pod("a", "pi-1", digestNew, waiting("InvalidImageName")), pod("b", "pi-2", digestNew, waiting("ImagePullBackOff"))}},
			wantVerdict: core.FailureVersion,
			wantSignals: []retry.Signal{retry.SignalBadImage, retry.SignalImagePull},
			wantSummary: "version: image cannot be started on node pi-1; image pull failing on node pi-2; the image cannot be started",
		},
		{
			name:        "run container error is the version",
			in:          retry.Input{Failure: degraded("x"), Pods: []core.PodStatus{pod("a", "pi-1", digestNew, waiting("RunContainerError"))}},
			wantVerdict: core.FailureVersion,
			wantSignals: []retry.Signal{retry.SignalBadImage},
		},
		{
			name:         "crash loop with no evidence is unknown",
			in:           retry.Input{Failure: degraded("x"), Pods: []core.PodStatus{pod("a", "pi-1", digestNew, crashing(1))}},
			wantVerdict:  core.FailureUnknown,
			wantSignals:  []retry.Signal{retry.SignalCrashLoop},
			wantSummary:  "unknown: crash loop (exit code 1) on node pi-1; no evidence yet whether the node or the release is at fault",
			wantEvidence: "no evidence yet",
		},
		{
			name: "previous version also unhealthy on the node",
			in: retry.Input{Failure: degraded("x"), Pods: []core.PodStatus{
				pod("a", "pi-3", digestNew, crashing(1)),
				pod("old", "pi-3", digestOld, crashing(139)),
			}},
			wantVerdict:  core.FailureTransient,
			wantSignals:  []retry.Signal{retry.SignalCrashLoop},
			wantEvidence: "the previous version is also unhealthy on node pi-3 (hardware, not the release)",
		},
		{
			name: "node failed other versions recently",
			in: retry.Input{
				Failure: degraded("x"),
				Pods:    []core.PodStatus{pod("a", "pi-3", digestNew, oomKilled)},
				Recent:  []retry.Diagnosis{{Nodes: []string{"pi-3"}}},
			},
			wantVerdict:  core.FailureTransient,
			wantSignals:  []retry.Signal{retry.SignalOOMKilled},
			wantEvidence: "other versions failed on node pi-3 in the last 7 days",
		},
		{
			name: "same crash on two nodes in one attempt",
			in: retry.Input{Failure: degraded("x"), Pods: []core.PodStatus{
				pod("a", "pi-1", digestNew, crashing(1)),
				pod("b", "pi-2", digestNew, crashing(1)),
			}},
			wantVerdict:  core.FailureVersion,
			wantNodes:    []string{"pi-1", "pi-2"},
			wantSummary:  "version: crash loop (exit code 1) on nodes pi-1, pi-2; the same crash_loop (Error, exit code 1) on 2 nodes: pi-1, pi-2",
			wantEvidence: "on 2 nodes",
		},
		{
			name: "same crash across attempts",
			in: retry.Input{
				Failure: degraded("x"),
				Pods:    []core.PodStatus{pod("a", "pi-2", digestNew, crashing(1))},
				Earlier: []retry.Diagnosis{{Findings: []retry.Finding{{Signal: retry.SignalCrashLoop, Node: "pi-1", Reason: "Error", ExitCode: 1, Pods: 1}}}},
			},
			wantVerdict:  core.FailureVersion,
			wantEvidence: "the same crash_loop (Error, exit code 1) on 2 nodes: pi-1, pi-2",
		},
		{
			name: "different exit codes on two nodes are not the same crash",
			in: retry.Input{Failure: degraded("x"), Pods: []core.PodStatus{
				pod("a", "pi-1", digestNew, crashing(1)),
				pod("b", "pi-2", digestNew, crashing(2)),
			}},
			wantVerdict: core.FailureUnknown,
		},
		{
			name: "same crash twice on one node is not enough",
			in: retry.Input{
				Failure: degraded("x"),
				Pods:    []core.PodStatus{pod("a", "pi-1", digestNew, crashing(1)), pod("b", "pi-1", digestNew, crashing(1))},
				Earlier: []retry.Diagnosis{{Findings: []retry.Finding{{Signal: retry.SignalCrashLoop, Node: "pi-1", Reason: "Error", ExitCode: 1}}}},
			},
			wantVerdict: core.FailureUnknown,
			wantSignals: []retry.Signal{retry.SignalCrashLoop, retry.SignalCrashLoop},
			wantNodes:   []string{"pi-1"},
		},
		{
			name: "previous version is ready on the same node",
			in: retry.Input{Failure: degraded("x"), Pods: []core.PodStatus{
				pod("a", "pi-1", digestNew, oomKilled),
				pod("old", "pi-1", digestOld, ready),
			}},
			wantVerdict:  core.FailureVersion,
			wantEvidence: "the previous version runs fine on node pi-1",
		},
		{
			name: "hardware evidence wins over a repeated crash",
			in: retry.Input{
				Failure: degraded("x"),
				Pods:    []core.PodStatus{pod("a", "pi-1", digestNew, crashing(1)), pod("b", "pi-2", digestNew, crashing(1))},
				Recent:  []retry.Diagnosis{{Nodes: []string{"pi-2"}}},
			},
			wantVerdict:  core.FailureTransient,
			wantEvidence: "pi-2",
		},
		{
			name: "single-node homelab with an earlier failure of another version",
			in: retry.Input{
				Failure: degraded("x"),
				Pods:    []core.PodStatus{pod("a", "homelab", digestNew, crashing(1))},
				Recent:  []retry.Diagnosis{{Nodes: []string{"homelab"}}},
			},
			wantVerdict: core.FailureTransient,
		},
		{
			name: "single-node homelab with a healthy previous version",
			in: retry.Input{
				Failure: degraded("x"),
				Pods:    []core.PodStatus{pod("a", "homelab", digestNew, crashing(1)), pod("old", "homelab", digestOld, ready)},
			},
			wantVerdict: core.FailureVersion,
		},
		{
			name: "healthy pods and previous pods give no signal",
			in: retry.Input{Failure: degraded("x"), Pods: []core.PodStatus{
				pod("a", "pi-1", digestNew, ready),
				pod("old", "pi-2", digestOld, waiting("ImagePullBackOff")),
			}},
			wantVerdict: core.FailureUnknown,
		},
		{
			name: "pods without a matching image count as new",
			in: retry.Input{Failure: degraded("x"), Pods: []core.PodStatus{
				{Name: "a", Node: "pi-1", Images: []string{"ghcr.io/example-user/blog:v2"}, Phase: "Pending", Scheduled: true, Containers: []core.ContainerStatus{{Waiting: "ImagePullBackOff"}}},
			}},
			wantVerdict: core.FailureTransient,
			wantSignals: []retry.Signal{retry.SignalImagePull},
		},
		{
			name:        "text fallback finds a crash loop",
			in:          retry.Input{Failure: degraded("application health is degraded; Deployment blog: back-off restarting failed container: CrashLoopBackOff")},
			wantVerdict: core.FailureUnknown,
			wantSignals: []retry.Signal{retry.SignalCrashLoop},
		},
		{
			name: "text fallback reads resource messages",
			in: retry.Input{Failure: engine.Failure{
				Deployment: deployment(), Stage: core.StateSyncing, Class: core.FailureVersion, Attempt: 1,
				Sync: &core.SyncFailure{Reason: core.SyncReasonDegraded, Class: core.FailureVersion, Message: "degraded", Resources: []core.ResourceStatus{
					{Kind: "Deployment", Name: "blog", Message: "Back-off pulling image: ImagePullBackOff"},
				}},
			}},
			wantVerdict: core.FailureTransient,
			wantSignals: []retry.Signal{retry.SignalImagePull},
		},
		{
			name:        "text fallback for an invalid image",
			in:          retry.Input{Failure: degraded("InvalidImageName: couldn't parse image reference")},
			wantVerdict: core.FailureVersion,
		},
		{
			name:        "text fallback for an unschedulable pod",
			in:          retry.Input{Failure: syncFailure(core.SyncReasonProgressing, core.FailureUnknown, "0/3 nodes are available: Insufficient cpu")},
			wantVerdict: core.FailureTransient,
			wantSignals: []retry.Signal{retry.SignalUnschedulable},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := retry.Classify(tt.in)
			if got.Verdict != tt.wantVerdict {
				t.Errorf("verdict = %s, want %s (%+v)", got.Verdict, tt.wantVerdict, got)
			}
			if tt.wantSignals != nil {
				var signals []retry.Signal
				for _, f := range got.Findings {
					for range f.Pods {
						signals = append(signals, f.Signal)
					}
					if f.Pods == 0 {
						signals = append(signals, f.Signal)
					}
				}
				if !slices.Equal(signals, tt.wantSignals) {
					t.Errorf("signals = %v, want %v", signals, tt.wantSignals)
				}
			}
			if tt.wantNodes != nil && !slices.Equal(got.Nodes, tt.wantNodes) {
				t.Errorf("nodes = %v, want %v", got.Nodes, tt.wantNodes)
			}
			if tt.wantSummary != "" && got.Summary != tt.wantSummary {
				t.Errorf("summary = %q, want %q", got.Summary, tt.wantSummary)
			}
			if !strings.HasPrefix(got.Summary, string(got.Verdict)+": ") {
				t.Errorf("summary %q does not start with the verdict", got.Summary)
			}
			if tt.wantEvidence != "" && !slices.ContainsFunc(got.Evidence, func(e string) bool { return strings.Contains(e, tt.wantEvidence) }) {
				t.Errorf("evidence = %v, want one containing %q", got.Evidence, tt.wantEvidence)
			}
			if got.Attempt != 1 {
				t.Errorf("attempt = %d, want 1", got.Attempt)
			}
		})
	}
}

func TestClassifyCaps(t *testing.T) {
	t.Parallel()

	var pods []core.PodStatus
	for i := range 30 {
		node := "node-" + string(rune('a'+i%26)) + strings.Repeat("x", i/26)
		pods = append(pods, pod("p", node, digestNew, waiting("ImagePullBackOff"), func(p *core.PodStatus) {
			p.Containers[0].Message = strings.Repeat("é", 500)
		}))
	}
	got := retry.Classify(retry.Input{Failure: degraded(strings.Repeat("x", 2000)), Pods: pods, PodsInspected: true})
	if len(got.Findings) != retry.MaxFindings || len(got.Nodes) != retry.MaxNodes {
		t.Errorf("findings = %d, nodes = %d, want %d and %d", len(got.Findings), len(got.Nodes), retry.MaxFindings, retry.MaxNodes)
	}
	for _, f := range got.Findings {
		if len(f.Detail) > 200 || !strings.HasPrefix(f.Detail, "é") || strings.ContainsRune(f.Detail, '�') {
			t.Errorf("detail is %d bytes, want at most 200 valid ones", len(f.Detail))
		}
	}
	if len([]rune(got.Summary)) > 300 {
		t.Errorf("summary has %d runes", len([]rune(got.Summary)))
	}
	if !got.PodsInspected {
		t.Error("PodsInspected = false")
	}
	if !strings.Contains(got.Summary, "on nodes node-a, node-ax, node-b") || strings.Contains(got.Summary, "node-c") {
		t.Errorf("summary = %q, want at most three nodes", got.Summary)
	}
}

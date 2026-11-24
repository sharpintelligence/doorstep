package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/sharpintelligence/doorstep/internal/core"
)

var update = flag.Bool("update", false, "rewrite golden files under testdata")

const (
	testSHA      = "0123456789abcdef0123456789abcdef01234567"
	testPinSHA   = "89abcdef0123456789abcdef0123456789abcdef"
	testDigest   = "sha256:9b2c4e6a8d0f1b3c5e7a9d1f3b5c7e9a1d3f5b7c9e1a3d5f7b9c1e3a5d7f9b1c"
	testPrevious = "sha256:1a3c5e7a9d1f3b5c7e9a1d3f5b7c9e1a3d5f7b9c1e3a5d7f9b1c9b2c4e6a8d0f"
)

var (
	testTarget = core.Target{App: "blog", Environment: "prod"}
	testTime   = time.Date(2026, 11, 23, 10, 30, 0, 0, time.UTC)
)

func testLinks() Links {
	return Links{
		DeploymentURL: func(id core.DeploymentID) string {
			return "https://doorstep.example.com/deployments/" + strconv.FormatInt(int64(id), 10)
		},
		CommitURL: func(repository, sha string) string {
			return "https://github.com/" + repository + "/commit/" + sha
		},
	}
}

func baseDeployment(id core.DeploymentID, state core.State) *core.Deployment {
	started := testTime.Add(-95 * time.Second)
	return &core.Deployment{
		ID:             id,
		Target:         testTarget,
		Kind:           core.KindDeploy,
		State:          state,
		Source:         core.SourceWebhook,
		RequestedBy:    "alice",
		Commit:         core.Commit{Repository: "example-user/blog", SHA: testSHA},
		Image:          core.ImageRef{Repository: "ghcr.io/example-user/blog", Tag: testSHA},
		Digest:         testDigest,
		PreviousDigest: testPrevious,
		CreatedAt:      testTime.Add(-2 * time.Minute),
		UpdatedAt:      testTime,
		StartedAt:      &started,
	}
}

func finished(d *core.Deployment) *core.Deployment {
	d.FinishedAt = new(testTime)
	return d
}

func rollbackDeployment(state core.State) *core.Deployment {
	d := baseDeployment(43, state)
	d.Kind = core.KindRollback
	d.Source = core.SourceSystem
	d.RequestedBy = "doorstep"
	d.RollbackOfID = new(core.DeploymentID(42))
	d.Commit.SHA = "fedcba9876543210fedcba9876543210fedcba98"
	d.Image.Tag = d.Commit.SHA
	d.Digest, d.PreviousDigest = testPrevious, testDigest
	return d
}

func freezeLock() *core.Lock {
	expires := testTime.Add(4 * time.Hour)
	return &core.Lock{
		Target:    testTarget,
		Kind:      core.LockFreeze,
		Owner:     "bob",
		Reason:    "database migration",
		CreatedAt: testTime.Add(-3 * time.Hour),
		ExpiresAt: &expires,
	}
}

type sample struct {
	name string
	n    core.Notification
}

func samples() []sample {
	failed := finished(baseDeployment(42, core.StateFailed))
	failed.FailureReason = "health gate: Application blog is Degraded"
	failed.PinCommitSHA = testPinSHA
	succeeded := finished(baseDeployment(42, core.StateSucceeded))
	succeeded.PinCommitSHA = testPinSHA
	rollbackFailed := finished(rollbackDeployment(core.StateFailed))
	rollbackFailed.FailureReason = "argocd: sync timed out after 10m0s"
	superseded := finished(baseDeployment(42, core.StateSuperseded))
	superseded.SupersededByID = new(core.DeploymentID(44))
	queued := baseDeployment(42, core.StateQueued)
	queued.StartedAt = nil

	n := func(kind core.NotificationKind) core.Notification {
		return core.Notification{Kind: kind, Target: testTarget, OccurredAt: testTime}
	}
	withDeployment := func(kind core.NotificationKind, d *core.Deployment, rollbackOf *core.Deployment) core.Notification {
		out := n(kind)
		out.Deployment, out.RollbackOf = d, rollbackOf
		return out
	}
	withLock := func(kind core.NotificationKind, change core.LockChange, actor string) core.Notification {
		out := n(kind)
		out.Lock, out.LockChange, out.Actor = freezeLock(), change, actor
		return out
	}
	cancelled := withDeployment(core.NotificationDeployCancelled, finished(baseDeployment(42, core.StateCancelled)), nil)
	cancelled.Actor = "carol"
	cancelled.Message = "Cancelled from the UI"

	return []sample{
		{"deploy-queued", withDeployment(core.NotificationDeployQueued, queued, nil)},
		{"deploy-started", withDeployment(core.NotificationDeployStarted, baseDeployment(42, core.StateResolving), nil)},
		{"deploy-succeeded", withDeployment(core.NotificationDeploySucceeded, succeeded, nil)},
		{"deploy-failed", withDeployment(core.NotificationDeployFailed, failed, nil)},
		{"rollback-started", withDeployment(core.NotificationDeployStarted, rollbackDeployment(core.StateResolving), failed)},
		{"deploy-rolled-back", withDeployment(core.NotificationDeployRolledBack, finished(rollbackDeployment(core.StateSucceeded)), failed)},
		{"rollback-failed", withDeployment(core.NotificationDeployFailed, rollbackFailed, failed)},
		{"deploy-cancelled", cancelled},
		{"deploy-superseded", withDeployment(core.NotificationDeploySuperseded, superseded, nil)},
		{"lock-acquired", withLock(core.NotificationLockChanged, core.LockAcquired, "")},
		{"lock-released", withLock(core.NotificationLockChanged, core.LockReleased, "carol")},
		{"lock-reminder", withLock(core.NotificationLockReminder, "", "")},
	}
}

func sampleNamed(t *testing.T, name string) core.Notification {
	t.Helper()
	for _, s := range samples() {
		if s.name == name {
			return s.n
		}
	}
	t.Fatalf("no sample %q", name)
	return core.Notification{}
}

func assertGolden(t *testing.T, path string, got []byte) {
	t.Helper()
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, got, "", "  "); err != nil {
		t.Fatalf("indent %s: %v", path, err)
	}
	pretty.WriteByte('\n')
	full := filepath.Join("testdata", path)
	if *update {
		if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(full, pretty.Bytes(), 0o600); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		return
	}
	want, err := os.ReadFile(filepath.Clean(full))
	if err != nil {
		t.Fatalf("read golden (run go test ./internal/notify -update to create it): %v", err)
	}
	if !bytes.Equal(pretty.Bytes(), want) {
		t.Errorf("%s differs from golden file:\ngot:\n%s\nwant:\n%s", full, pretty.Bytes(), want)
	}
}

type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *logBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (b *logBuffer) entries(t *testing.T) []map[string]any {
	t.Helper()
	var out []map[string]any
	for line := range bytes.SplitSeq([]byte(b.String()), []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var entry map[string]any
		if err := json.Unmarshal(line, &entry); err != nil {
			t.Fatalf("log line %q: %v", line, err)
		}
		out = append(out, entry)
	}
	return out
}

func (b *logBuffer) count(t *testing.T, msg, level string) int {
	t.Helper()
	n := 0
	for _, e := range b.entries(t) {
		if e["msg"] == msg && (level == "" || e["level"] == level) {
			n++
		}
	}
	return n
}

func newTestLogger() (*slog.Logger, *logBuffer) {
	buf := &logBuffer{}
	return slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})), buf
}

type observation struct {
	sinkType string
	result   Result
}

type recordingObserver struct {
	mu   sync.Mutex
	seen []observation
}

func (o *recordingObserver) Delivered(sinkType string, result Result) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.seen = append(o.seen, observation{sinkType, result})
}

func (o *recordingObserver) count(sinkType string, result Result) int {
	o.mu.Lock()
	defer o.mu.Unlock()
	n := 0
	for _, s := range o.seen {
		if s.sinkType == sinkType && s.result == result {
			n++
		}
	}
	return n
}

func noSleep(ctx context.Context, _ time.Duration) error {
	return ctx.Err()
}

func fastPolicy() Policy {
	return Policy{Attempts: 4, Sleep: noSleep, Random: func() float64 { return 0.5 }}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

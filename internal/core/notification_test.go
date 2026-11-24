package core

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestNotificationKinds(t *testing.T) {
	t.Parallel()

	tests := []struct {
		kind      NotificationKind
		deploy    bool
		lock      bool
		byDefault bool
	}{
		{kind: NotificationDeployQueued, deploy: true},
		{kind: NotificationDeployStarted, deploy: true, byDefault: true},
		{kind: NotificationDeployRetrying, deploy: true, byDefault: true},
		{kind: NotificationDeploySucceeded, deploy: true, byDefault: true},
		{kind: NotificationDeployFailed, deploy: true, byDefault: true},
		{kind: NotificationDeployRolledBack, deploy: true, byDefault: true},
		{kind: NotificationDeployCancelled, deploy: true},
		{kind: NotificationDeploySuperseded, deploy: true},
		{kind: NotificationLockChanged, lock: true, byDefault: true},
		{kind: NotificationLockReminder, lock: true, byDefault: true},
	}

	var covered []NotificationKind
	for _, tt := range tests {
		covered = append(covered, tt.kind)
		t.Run(tt.kind.String(), func(t *testing.T) {
			t.Parallel()
			if !tt.kind.Valid() {
				t.Error("Valid() = false")
			}
			if got := tt.kind.IsDeploy(); got != tt.deploy {
				t.Errorf("IsDeploy() = %v, want %v", got, tt.deploy)
			}
			if got := tt.kind.IsLock(); got != tt.lock {
				t.Errorf("IsLock() = %v, want %v", got, tt.lock)
			}
			if got := slices.Contains(DefaultNotificationKinds(), tt.kind); got != tt.byDefault {
				t.Errorf("in DefaultNotificationKinds() = %v, want %v", got, tt.byDefault)
			}
			if strings.Contains(tt.kind.String(), "_") && !strings.HasSuffix(tt.kind.String(), "rolled_back") {
				t.Errorf("wire name %q must use dots between words", tt.kind)
			}
			got, err := ParseNotificationKind(tt.kind.String())
			if err != nil || got != tt.kind {
				t.Errorf("ParseNotificationKind(%q) = %q, %v", tt.kind, got, err)
			}
		})
	}
	if !slices.Equal(covered, NotificationKinds()) {
		t.Errorf("test table covers %v, NotificationKinds() = %v", covered, NotificationKinds())
	}
}

func TestParseNotificationKindRejects(t *testing.T) {
	t.Parallel()

	for _, in := range []string{"", "deploy_succeeded", "Deploy.Succeeded", "deploy", "deploy.rolled-back", " lock.changed"} {
		t.Run(in, func(t *testing.T) {
			t.Parallel()
			kind := NotificationKind(in)
			if kind.Valid() || kind.IsDeploy() || kind.IsLock() {
				t.Errorf("%q is classified as a known kind", in)
			}
			_, err := ParseNotificationKind(in)
			if !errors.Is(err, ErrUnknownNotificationKind) {
				t.Errorf("ParseNotificationKind(%q) error = %v, want ErrUnknownNotificationKind", in, err)
			}
		})
	}
}

func TestLockChanges(t *testing.T) {
	t.Parallel()

	for _, c := range []LockChange{LockAcquired, LockReleased} {
		if !c.Valid() {
			t.Errorf("%q.Valid() = false", c)
		}
	}
	for _, c := range []LockChange{"", "stolen", "Acquired"} {
		if c.Valid() {
			t.Errorf("%q.Valid() = true", c)
		}
	}
}

func TestNotificationValidate(t *testing.T) {
	t.Parallel()

	target := Target{App: "blog", Environment: "prod"}
	other := Target{App: "api", Environment: "prod"}
	rollbackOf := DeploymentID(41)
	deploy := func() *Deployment {
		return &Deployment{ID: 41, Target: target, Kind: KindDeploy, State: StateFailed}
	}
	rollback := func() *Deployment {
		return &Deployment{ID: 42, Target: target, Kind: KindRollback, State: StateSucceeded, RollbackOfID: &rollbackOf}
	}
	lock := func() *Lock {
		return &Lock{Target: target, Kind: LockFreeze, Owner: "alice", CreatedAt: time.Unix(0, 0)}
	}

	tests := []struct {
		name    string
		n       Notification
		wantErr []string
		wantIs  error
	}{
		{name: "deploy succeeded", n: Notification{Kind: NotificationDeploySucceeded, Target: target, Deployment: deploy()}},
		{name: "rollback started", n: Notification{Kind: NotificationDeployStarted, Target: target, Deployment: rollback(), RollbackOf: deploy()}},
		{name: "rolled back", n: Notification{Kind: NotificationDeployRolledBack, Target: target, Deployment: rollback(), RollbackOf: deploy()}},
		{name: "rollback failed", n: Notification{Kind: NotificationDeployFailed, Target: target, Deployment: rollback()}},
		{name: "lock acquired", n: Notification{Kind: NotificationLockChanged, Target: target, Lock: lock(), LockChange: LockAcquired}},
		{name: "lock released", n: Notification{Kind: NotificationLockChanged, Target: target, Lock: lock(), LockChange: LockReleased}},
		{name: "lock reminder", n: Notification{Kind: NotificationLockReminder, Target: target, Lock: lock()}},
		{
			name:    "unknown kind",
			n:       Notification{Kind: "deploy.exploded", Target: target, Deployment: deploy()},
			wantErr: []string{`"deploy.exploded"`},
			wantIs:  ErrUnknownNotificationKind,
		},
		{
			name:    "invalid target",
			n:       Notification{Kind: NotificationDeploySucceeded, Deployment: deploy()},
			wantErr: []string{"target app must not be empty", "does not match"},
		},
		{
			name:    "deploy kind without deployment",
			n:       Notification{Kind: NotificationDeployStarted, Target: target},
			wantErr: []string{"deploy.started notification needs a deployment"},
		},
		{
			name:    "deploy kind with lock",
			n:       Notification{Kind: NotificationDeployStarted, Target: target, Deployment: deploy(), Lock: lock(), LockChange: LockAcquired},
			wantErr: []string{"must not carry a lock"},
		},
		{
			name:    "deployment for another target",
			n:       Notification{Kind: NotificationDeployStarted, Target: other, Deployment: deploy()},
			wantErr: []string{"deployment target blog/prod does not match notification target api/prod"},
		},
		{
			name:    "rolled back without original",
			n:       Notification{Kind: NotificationDeployRolledBack, Target: target, Deployment: rollback()},
			wantErr: []string{"needs the deployment that was rolled back"},
		},
		{
			name:    "rolled back with a deploy",
			n:       Notification{Kind: NotificationDeployRolledBack, Target: target, Deployment: deploy(), RollbackOf: deploy()},
			wantErr: []string{"needs a deployment of kind rollback", "only a rollback deployment may name"},
		},
		{
			name: "rolled back deployment for another target",
			n: Notification{
				Kind: NotificationDeployRolledBack, Target: target, Deployment: rollback(),
				RollbackOf: &Deployment{ID: 41, Target: other, Kind: KindDeploy},
			},
			wantErr: []string{"rolled back deployment target api/prod does not match"},
		},
		{
			name:    "lock kind without lock",
			n:       Notification{Kind: NotificationLockReminder, Target: target},
			wantErr: []string{"lock.reminder notification needs a lock"},
		},
		{
			name:    "lock kind with deployment",
			n:       Notification{Kind: NotificationLockReminder, Target: target, Lock: lock(), Deployment: deploy()},
			wantErr: []string{"must not carry a deployment"},
		},
		{
			name:    "lock for another target",
			n:       Notification{Kind: NotificationLockReminder, Target: other, Lock: lock()},
			wantErr: []string{"lock target blog/prod does not match"},
		},
		{
			name:    "lock changed without change",
			n:       Notification{Kind: NotificationLockChanged, Target: target, Lock: lock()},
			wantErr: []string{`needs a lock change of acquired or released, got ""`},
		},
		{
			name:    "lock changed with unknown change",
			n:       Notification{Kind: NotificationLockChanged, Target: target, Lock: lock(), LockChange: "stolen"},
			wantErr: []string{`got "stolen"`},
		},
		{
			name:    "lock reminder with change",
			n:       Notification{Kind: NotificationLockReminder, Target: target, Lock: lock(), LockChange: LockReleased},
			wantErr: []string{"lock.reminder notification must not carry a lock change"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := tt.n.Validate()
			if len(tt.wantErr) == 0 {
				if err != nil {
					t.Fatalf("Validate() = %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate() = nil, want %q", tt.wantErr)
			}
			for _, want := range tt.wantErr {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("Validate() = %q, want it to contain %q", err, want)
				}
			}
			if tt.wantIs != nil && !errors.Is(err, tt.wantIs) {
				t.Errorf("Validate() = %v, want errors.Is %v", err, tt.wantIs)
			}
		})
	}
}

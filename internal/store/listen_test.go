package store_test

import (
	"context"
	"errors"
	"net/url"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/store"
)

const (
	notifyWait  = 5 * time.Second
	silenceWait = 300 * time.Millisecond
)

type notifyListener struct {
	t      *testing.T
	conn   *pgx.Conn
	schema string
}

func listenRaw(t *testing.T, databaseURL string) *notifyListener {
	t.Helper()
	conn := connectRaw(t, databaseURL)
	if _, err := conn.Exec(t.Context(), "LISTEN "+store.QueueChannel); err != nil {
		t.Fatalf("LISTEN: %v", err)
	}
	return &notifyListener{t: t, conn: conn, schema: schemaOf(t, databaseURL)}
}

func (l *notifyListener) received(wait time.Duration) bool {
	l.t.Helper()
	ctx, cancel := context.WithTimeout(l.t.Context(), wait)
	defer cancel()
	got := false
	for {
		n, err := l.conn.WaitForNotification(ctx)
		if err != nil {
			if !errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
				l.t.Fatalf("WaitForNotification: %v", err)
			}
			return got
		}
		if n.Payload == l.schema {
			got = true
			if wait == notifyWait {
				return true
			}
		}
	}
}

func (l *notifyListener) expect(step string, want bool) {
	l.t.Helper()
	wait := silenceWait
	if want {
		wait = notifyWait
	}
	if got := l.received(wait); got != want {
		l.t.Errorf("%s: notified = %v, want %v", step, got, want)
	}
	l.received(silenceWait)
}

func TestQueueNotifications(t *testing.T) {
	t.Parallel()

	st, databaseURL := openMigrated(t)
	l := listenRaw(t, databaseURL)
	ctx := t.Context()

	d := mustEnqueue(t, st, deploymentOf("web", randomSHA(t)), core.PriorityNormal)
	l.expect("enqueue", true)

	if _, _, ok, err := st.Claim(ctx, "w1", testLease); err != nil || !ok {
		t.Fatalf("Claim = %v, %v", ok, err)
	}
	l.expect("claim", false)

	if _, err := st.HeartbeatQueueEntry(ctx, d.ID, "w1", testLease); err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	l.expect("heartbeat", false)

	if err := st.ExpediteDeployment(ctx, d.ID, core.PriorityUrgent, "alice"); err != nil {
		t.Fatalf("Expedite: %v", err)
	}
	l.expect("expedite", true)

	if err := st.ReleaseQueueEntry(ctx, d.ID, "w1", 0); err != nil {
		t.Fatalf("Release: %v", err)
	}
	l.expect("release", true)

	if _, _, ok, err := st.Claim(ctx, "w1", testLease); err != nil || !ok {
		t.Fatalf("Claim = %v, %v", ok, err)
	}
	l.expect("second claim", false)

	if err := st.CompleteQueueEntry(ctx, d.ID, "w1"); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	l.expect("complete", true)
}

func TestListenQueue(t *testing.T) {
	t.Parallel()

	st, databaseURL := openMigrated(t)
	other, _ := openMigrated(t)

	appName := "doorstep-listen-" + randomSHA(t)[:12]
	u, err := url.Parse(databaseURL)
	if err != nil {
		t.Fatalf("parse URL: %v", err)
	}
	query := u.Query()
	query.Set("application_name", appName)
	u.RawQuery = query.Encode()
	listener := openUnmigrated(t, u.String())

	wakes := make(chan struct{}, 100)
	wake := func() {
		select {
		case wakes <- struct{}{}:
		default:
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- listener.ListenQueue(ctx, wake, nil) }()

	expectWake := func(step string, want bool) {
		t.Helper()
		wait := silenceWait
		if want {
			wait = notifyWait
		}
		select {
		case <-wakes:
			if !want {
				t.Errorf("%s: unexpected wake", step)
			}
		case <-time.After(wait):
			if want {
				t.Fatalf("%s: no wake within %s", step, wait)
			}
		}
		for len(wakes) > 0 {
			<-wakes
		}
	}

	expectWake("connect", true)
	mustEnqueue(t, other, deploymentOf("web", randomSHA(t)), core.PriorityNormal)
	expectWake("other schema", false)
	mustEnqueue(t, st, deploymentOf("web", randomSHA(t)), core.PriorityNormal)
	expectWake("enqueue", true)

	terminated := queryInt(t, st, `SELECT count(*) FROM (
		SELECT pg_terminate_backend(pid) FROM pg_stat_activity
		WHERE application_name = $1 AND query LIKE 'LISTEN%'
	) killed`, appName)
	if terminated != 1 {
		t.Fatalf("terminated %d listener backends, want 1", terminated)
	}
	expectWake("reconnect", true)
	mustEnqueue(t, st, deploymentOf("web", randomSHA(t)), core.PriorityNormal)
	expectWake("enqueue after reconnect", true)

	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("ListenQueue = %v, want context.Canceled", err)
		}
	case <-time.After(notifyWait):
		t.Fatal("ListenQueue did not return after cancellation")
	}
}

func TestListenQueueRejectsTransaction(t *testing.T) {
	t.Parallel()

	st, _ := openMigrated(t)
	err := st.InTx(t.Context(), func(tx *store.Store) error {
		return tx.ListenQueue(t.Context(), func() {}, nil)
	})
	if err == nil {
		t.Fatal("ListenQueue inside a transaction succeeded")
	}
}

package store_test

import (
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sharpintelligence/doorstep/internal/store"
	"github.com/sharpintelligence/doorstep/internal/store/storetest"
)

const deliveryStaleAfter = time.Minute

func TestClaimWebhookDelivery(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	steps := []struct {
		name     string
		release  string
		complete string
		age      string
		id       string
		event    string
		want     store.DeliveryClaim
	}{
		{name: "first delivery is claimed", id: "d-1", event: "push", want: store.DeliveryClaimed},
		{name: "redelivery while the first is handled is in progress", id: "d-1", event: "push", want: store.DeliveryInProgress},
		{name: "same id with another event is in progress", id: "d-1", event: "check_run", want: store.DeliveryInProgress},
		{name: "another delivery is claimed", id: "d-2", event: "push", want: store.DeliveryClaimed},
		{name: "released delivery is claimed again", release: "d-1", id: "d-1", event: "push", want: store.DeliveryClaimed},
		{name: "completed delivery is a duplicate", complete: "d-1", id: "d-1", event: "push", want: store.DeliveryCompleted},
		{name: "releasing a completed delivery keeps it", release: "d-1", id: "d-1", event: "push", want: store.DeliveryCompleted},
		{name: "old completed delivery is still a duplicate", age: "d-1", id: "d-1", event: "push", want: store.DeliveryCompleted},
		{name: "stranded claim is reclaimed", age: "d-2", id: "d-2", event: "push", want: store.DeliveryClaimed},
		{name: "reclaimed delivery is in progress again", id: "d-2", event: "push", want: store.DeliveryInProgress},
		{name: "releasing an unknown delivery is a no-op", release: "unknown", id: "d-3", event: "push", want: store.DeliveryClaimed},
	}
	for _, step := range steps {
		if step.release != "" {
			if err := st.ReleaseWebhookDelivery(t.Context(), step.release); err != nil {
				t.Fatalf("%s: ReleaseWebhookDelivery: %v", step.name, err)
			}
		}
		if step.complete != "" {
			if err := st.CompleteWebhookDelivery(t.Context(), step.complete); err != nil {
				t.Fatalf("%s: CompleteWebhookDelivery: %v", step.name, err)
			}
		}
		if step.age != "" {
			mustExec(t, st, "UPDATE webhook_deliveries SET claimed_at = claimed_at - interval '2 minutes', completed_at = completed_at - interval '1 minute' WHERE delivery_id = $1", step.age)
		}
		got, err := st.ClaimWebhookDelivery(t.Context(), step.id, step.event, deliveryStaleAfter)
		if err != nil {
			t.Fatalf("%s: ClaimWebhookDelivery: %v", step.name, err)
		}
		if got != step.want {
			t.Errorf("%s: claim = %s, want %s", step.name, got, step.want)
		}
	}
	if n := queryInt(t, st, "SELECT count(*) FROM webhook_deliveries"); n != 3 {
		t.Errorf("rows = %d, want 3", n)
	}
	if n := queryInt(t, st, "SELECT count(*) FROM webhook_deliveries WHERE completed_at IS NOT NULL"); n != 1 {
		t.Errorf("completed rows = %d, want 1", n)
	}
}

func TestClaimWebhookDeliveryRejectsInvalidInput(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	tests := []struct {
		name       string
		id         string
		event      string
		staleAfter time.Duration
		want       string
	}{
		{name: "empty id", id: "", event: "push", staleAfter: deliveryStaleAfter, want: "delivery id must be 1 to 128 characters"},
		{name: "id too long", id: strings.Repeat("x", store.MaxDeliveryIDLength+1), event: "push", staleAfter: deliveryStaleAfter, want: "got 129"},
		{name: "empty event", id: "d-1", event: "", staleAfter: deliveryStaleAfter, want: "event must not be empty"},
		{name: "zero stale age", id: "d-1", event: "push", want: "stale age must be positive"},
		{name: "negative stale age", id: "d-1", event: "push", staleAfter: -time.Second, want: "stale age must be positive"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := st.ClaimWebhookDelivery(t.Context(), tt.id, tt.event, tt.staleAfter)
			if err == nil || !strings.Contains(err.Error(), tt.want) || got != "" {
				t.Errorf("ClaimWebhookDelivery = %q, %v, want an error containing %q", got, err, tt.want)
			}
		})
	}
	if got, err := st.ClaimWebhookDelivery(t.Context(), strings.Repeat("x", store.MaxDeliveryIDLength), "push", deliveryStaleAfter); err != nil || got != store.DeliveryClaimed {
		t.Errorf("ClaimWebhookDelivery with a %d character id = %s, %v", store.MaxDeliveryIDLength, got, err)
	}
}

func TestClaimWebhookDeliveryConcurrentClaimsHaveOneWinner(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		setup string
	}{
		{name: "new delivery"},
		{name: "stranded claim", setup: "INSERT INTO webhook_deliveries (delivery_id, event, claimed_at) VALUES ('same', 'push', now() - interval '1 hour')"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			st := storetest.Open(t)
			if tt.setup != "" {
				mustExec(t, st, tt.setup)
			}
			const callers = 8
			var (
				wg      sync.WaitGroup
				winners atomic.Int32
				start   = make(chan struct{})
			)
			for range callers {
				wg.Go(func() {
					<-start
					got, err := st.ClaimWebhookDelivery(t.Context(), "same", "push", deliveryStaleAfter)
					if err != nil {
						t.Errorf("ClaimWebhookDelivery: %v", err)
					}
					switch got {
					case store.DeliveryClaimed:
						winners.Add(1)
					case store.DeliveryInProgress:
					case store.DeliveryCompleted:
						t.Errorf("ClaimWebhookDelivery = %s, want claimed or in progress", got)
					}
				})
			}
			close(start)
			wg.Wait()
			if got := winners.Load(); got != 1 {
				t.Errorf("winners = %d, want 1", got)
			}
		})
	}
}

func TestDeleteWebhookDeliveriesBefore(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	mustExec(t, st, `INSERT INTO webhook_deliveries (delivery_id, event, claimed_at, completed_at) VALUES
		('old', 'push', now() - interval '8 days', now() - interval '8 days'),
		('edge', 'push', now() - interval '6 days 23 hours', now() - interval '6 days 23 hours'),
		('new', 'push', now(), NULL)`)

	deleted, err := st.DeleteWebhookDeliveriesBefore(t.Context(), 7*24*time.Hour)
	if err != nil {
		t.Fatalf("DeleteWebhookDeliveriesBefore: %v", err)
	}
	if deleted != 1 {
		t.Errorf("deleted = %d, want 1", deleted)
	}
	if n := queryInt(t, st, "SELECT count(*) FROM webhook_deliveries WHERE delivery_id IN ('edge', 'new')"); n != 2 {
		t.Errorf("kept rows = %d, want 2", n)
	}
	if got, err := st.ClaimWebhookDelivery(t.Context(), "old", "push", deliveryStaleAfter); err != nil || got != store.DeliveryClaimed {
		t.Errorf("ClaimWebhookDelivery after retention = %s, %v, want claimed", got, err)
	}

	if _, err := st.DeleteWebhookDeliveriesBefore(t.Context(), 0); err == nil {
		t.Error("DeleteWebhookDeliveriesBefore(0) succeeded, want an error")
	}
}

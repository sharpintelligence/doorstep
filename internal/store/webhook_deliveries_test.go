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

func TestClaimWebhookDelivery(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	steps := []struct {
		name        string
		release     string
		id          string
		event       string
		wantClaimed bool
	}{
		{name: "first delivery is claimed", id: "d-1", event: "push", wantClaimed: true},
		{name: "redelivery is a duplicate", id: "d-1", event: "push"},
		{name: "same id with another event is a duplicate", id: "d-1", event: "check_run"},
		{name: "another delivery is claimed", id: "d-2", event: "status", wantClaimed: true},
		{name: "released delivery is claimed again", release: "d-1", id: "d-1", event: "push", wantClaimed: true},
		{name: "releasing an unknown delivery is a no-op", release: "unknown", id: "d-3", event: "push", wantClaimed: true},
	}
	for _, step := range steps {
		if step.release != "" {
			if err := st.ReleaseWebhookDelivery(t.Context(), step.release); err != nil {
				t.Fatalf("%s: ReleaseWebhookDelivery: %v", step.name, err)
			}
		}
		claimed, err := st.ClaimWebhookDelivery(t.Context(), step.id, step.event)
		if err != nil {
			t.Fatalf("%s: ClaimWebhookDelivery: %v", step.name, err)
		}
		if claimed != step.wantClaimed {
			t.Errorf("%s: claimed = %t, want %t", step.name, claimed, step.wantClaimed)
		}
	}
	if n := queryInt(t, st, "SELECT count(*) FROM webhook_deliveries"); n != 3 {
		t.Errorf("rows = %d, want 3", n)
	}
}

func TestClaimWebhookDeliveryRejectsInvalidInput(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	tests := []struct {
		name  string
		id    string
		event string
		want  string
	}{
		{name: "empty id", id: "", event: "push", want: "delivery id must be 1 to 128 characters"},
		{name: "id too long", id: strings.Repeat("x", store.MaxDeliveryIDLength+1), event: "push", want: "got 129"},
		{name: "empty event", id: "d-1", event: "", want: "event must not be empty"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			claimed, err := st.ClaimWebhookDelivery(t.Context(), tt.id, tt.event)
			if err == nil || !strings.Contains(err.Error(), tt.want) || claimed {
				t.Errorf("ClaimWebhookDelivery = %t, %v, want an error containing %q", claimed, err, tt.want)
			}
		})
	}
	if claimed, err := st.ClaimWebhookDelivery(t.Context(), strings.Repeat("x", store.MaxDeliveryIDLength), "push"); err != nil || !claimed {
		t.Errorf("ClaimWebhookDelivery with a %d character id = %t, %v", store.MaxDeliveryIDLength, claimed, err)
	}
}

func TestClaimWebhookDeliveryConcurrentClaimsHaveOneWinner(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	const callers = 8
	var (
		wg      sync.WaitGroup
		winners atomic.Int32
		start   = make(chan struct{})
	)
	for range callers {
		wg.Go(func() {
			<-start
			claimed, err := st.ClaimWebhookDelivery(t.Context(), "same", "push")
			if err != nil {
				t.Errorf("ClaimWebhookDelivery: %v", err)
			}
			if claimed {
				winners.Add(1)
			}
		})
	}
	close(start)
	wg.Wait()
	if got := winners.Load(); got != 1 {
		t.Errorf("winners = %d, want 1", got)
	}
}

func TestDeleteWebhookDeliveriesBefore(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	mustExec(t, st, `INSERT INTO webhook_deliveries (delivery_id, event, received_at) VALUES
		('old', 'push', now() - interval '8 days'),
		('edge', 'push', now() - interval '6 days 23 hours'),
		('new', 'status', now())`)

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
	if claimed, err := st.ClaimWebhookDelivery(t.Context(), "old", "push"); err != nil || !claimed {
		t.Errorf("ClaimWebhookDelivery after retention = %t, %v, want claimed", claimed, err)
	}

	if _, err := st.DeleteWebhookDeliveriesBefore(t.Context(), 0); err == nil {
		t.Error("DeleteWebhookDeliveriesBefore(0) succeeded, want an error")
	}
}

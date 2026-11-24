package api

import (
	"context"
	"net/http"
	"time"

	"github.com/sharpintelligence/doorstep/internal/core"
)

const queuePath = Prefix + "apps/{app}/queue"

type QueueReader interface {
	Entries(ctx context.Context, app string) ([]core.QueueEntry, error)
}

type AppCatalog interface {
	Known(app string) bool
}

type queueResponse struct {
	App     string       `json:"app"`
	Depth   int          `json:"depth"`
	Entries []queueEntry `json:"entries"`
}

type queueEntry struct {
	Position       int               `json:"position"`
	DeploymentID   core.DeploymentID `json:"deployment_id"`
	Environment    string            `json:"environment"`
	Kind           string            `json:"kind"`
	State          string            `json:"state"`
	Priority       string            `json:"priority"`
	CommitSHA      string            `json:"commit_sha"`
	Source         string            `json:"source"`
	RequestedBy    string            `json:"requested_by"`
	EnqueuedAt     time.Time         `json:"enqueued_at"`
	AvailableAt    time.Time         `json:"available_at"`
	Attempts       int               `json:"attempts"`
	InFlight       bool              `json:"in_flight"`
	LeaseExpiresAt *time.Time        `json:"lease_expires_at"`
}

func (a *api) registerQueue(mux *http.ServeMux) {
	mux.HandleFunc("GET "+queuePath, a.getQueue)
	mux.HandleFunc(queuePath, methodNotAllowed(http.MethodGet, http.MethodHead))
}

func (a *api) getQueue(w http.ResponseWriter, r *http.Request) {
	app := r.PathValue("app")
	entries, err := a.queue.Entries(r.Context(), app)
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	if len(entries) == 0 && !a.apps.Known(app) {
		writeError(w, http.StatusNotFound, codeNotFound, "app is not configured")
		return
	}

	resp := queueResponse{App: app, Entries: make([]queueEntry, 0, len(entries))}
	for _, e := range entries {
		if !e.InFlight() {
			resp.Depth++
		}
		d := e.Deployment
		resp.Entries = append(resp.Entries, queueEntry{
			Position:       e.Position,
			DeploymentID:   d.ID,
			Environment:    d.Target.Environment,
			Kind:           string(d.Kind),
			State:          string(d.State),
			Priority:       e.Priority.String(),
			CommitSHA:      d.Commit.SHA,
			Source:         string(d.Source),
			RequestedBy:    d.RequestedBy,
			EnqueuedAt:     e.EnqueuedAt.UTC(),
			AvailableAt:    e.AvailableAt.UTC(),
			Attempts:       e.Attempts,
			InFlight:       e.InFlight(),
			LeaseExpiresAt: utc(e.LeaseExpiresAt),
		})
	}
	writeJSON(w, http.StatusOK, resp)
}

func utc(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}

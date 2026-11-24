package api

import (
	"encoding/json"
	"strconv"
	"time"

	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/store"
)

type deploymentList struct {
	Items      []deployment `json:"items"`
	NextCursor string       `json:"next_cursor,omitempty"`
}

type deploymentDetail struct {
	Deployment      deployment   `json:"deployment"`
	Events          []event      `json:"events"`
	EventsTruncated bool         `json:"events_truncated"`
	Gates           []gate       `json:"gates"`
	Rollbacks       []deployment `json:"rollbacks"`
	Retries         []deployment `json:"retries"`
}

type deployment struct {
	ID              core.DeploymentID  `json:"id"`
	App             string             `json:"app"`
	Environment     string             `json:"environment"`
	Kind            core.Kind          `json:"kind"`
	State           core.State         `json:"state"`
	Trigger         trigger            `json:"trigger"`
	Commit          commit             `json:"commit"`
	Image           image              `json:"image"`
	GitOps          gitops             `json:"gitops"`
	ArgoCD          argocd             `json:"argocd"`
	ParentID        *core.DeploymentID `json:"parent_id,omitempty"`
	RollbackOfID    *core.DeploymentID `json:"rollback_of_id,omitempty"`
	SupersededByID  *core.DeploymentID `json:"superseded_by_id,omitempty"`
	FailureReason   string             `json:"failure_reason,omitempty"`
	Migration       bool               `json:"migration,omitempty"`
	MigrationReason string             `json:"migration_reason,omitempty"`
	CreatedAt       time.Time          `json:"created_at"`
	UpdatedAt       time.Time          `json:"updated_at"`
	StartedAt       *time.Time         `json:"started_at,omitempty"`
	FinishedAt      *time.Time         `json:"finished_at,omitempty"`
}

type trigger struct {
	Source      core.Source `json:"source"`
	RequestedBy string      `json:"requested_by,omitempty"`
}

type commit struct {
	Repository string `json:"repository"`
	SHA        string `json:"sha"`
}

type image struct {
	Repository     string      `json:"repository"`
	Tag            string      `json:"tag"`
	Digest         core.Digest `json:"digest,omitempty"`
	PreviousDigest core.Digest `json:"previous_digest,omitempty"`
}

type gitops struct {
	Repository string `json:"repository,omitempty"`
	CommitSHA  string `json:"commit_sha,omitempty"`
}

type argocd struct {
	Revision string `json:"revision,omitempty"`
}

type event struct {
	ID         int64           `json:"id"`
	Type       core.EventType  `json:"type"`
	From       core.State      `json:"from,omitempty"`
	To         core.State      `json:"to,omitempty"`
	Actor      string          `json:"actor,omitempty"`
	Message    string          `json:"message,omitempty"`
	Data       json.RawMessage `json:"data"`
	OccurredAt time.Time       `json:"occurred_at"`
}

type gate struct {
	Name       string          `json:"name"`
	Type       string          `json:"type"`
	Phase      core.GatePhase  `json:"phase"`
	Status     core.GateStatus `json:"status"`
	Message    string          `json:"message,omitempty"`
	Evidence   json.RawMessage `json:"evidence"`
	StartedAt  time.Time       `json:"started_at"`
	FinishedAt *time.Time      `json:"finished_at,omitempty"`
}

func newDeploymentList(page store.DeploymentPage) deploymentList {
	list := deploymentList{Items: newDeployments(page.Items)}
	if page.Next != 0 {
		list.NextCursor = strconv.FormatInt(int64(page.Next), 10)
	}
	return list
}

func newDeploymentDetail(h store.DeploymentHistory) deploymentDetail {
	detail := deploymentDetail{
		Deployment:      newDeployment(h.Deployment),
		Events:          make([]event, 0, len(h.Events)),
		EventsTruncated: h.EventsTruncated,
		Gates:           make([]gate, 0, len(h.Gates)),
		Rollbacks:       newDeployments(h.Rollbacks),
		Retries:         newDeployments(h.Retries),
	}
	for _, e := range h.Events {
		detail.Events = append(detail.Events, event{
			ID:         e.ID,
			Type:       e.Type,
			From:       e.From,
			To:         e.To,
			Actor:      e.Actor,
			Message:    e.Message,
			Data:       jsonObject(e.Data),
			OccurredAt: e.OccurredAt.UTC(),
		})
	}
	for _, g := range h.Gates {
		detail.Gates = append(detail.Gates, gate{
			Name:       g.Name,
			Type:       g.Type,
			Phase:      g.Phase,
			Status:     g.Status,
			Message:    g.Message,
			Evidence:   jsonObject(g.Evidence),
			StartedAt:  g.StartedAt.UTC(),
			FinishedAt: utc(g.FinishedAt),
		})
	}
	return detail
}

func newDeployments(ds []core.Deployment) []deployment {
	out := make([]deployment, 0, len(ds))
	for _, d := range ds {
		out = append(out, newDeployment(d))
	}
	return out
}

func newDeployment(d core.Deployment) deployment {
	return deployment{
		ID:              d.ID,
		App:             d.Target.App,
		Environment:     d.Target.Environment,
		Kind:            d.Kind,
		State:           d.State,
		Trigger:         trigger{Source: d.Source, RequestedBy: d.RequestedBy},
		Commit:          commit{Repository: d.Commit.Repository, SHA: d.Commit.SHA},
		Image:           image{Repository: d.Image.Repository, Tag: d.Image.Tag, Digest: d.Digest, PreviousDigest: d.PreviousDigest},
		GitOps:          gitops{Repository: d.GitOpsRepository, CommitSHA: d.PinCommitSHA},
		ArgoCD:          argocd{Revision: d.ArgoCDRevision},
		ParentID:        d.ParentID,
		RollbackOfID:    d.RollbackOfID,
		SupersededByID:  d.SupersededByID,
		FailureReason:   d.FailureReason,
		Migration:       d.Migration,
		MigrationReason: d.MigrationReason,
		CreatedAt:       d.CreatedAt.UTC(),
		UpdatedAt:       d.UpdatedAt.UTC(),
		StartedAt:       utc(d.StartedAt),
		FinishedAt:      utc(d.FinishedAt),
	}
}

func jsonObject(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage("{}")
	}
	return raw
}

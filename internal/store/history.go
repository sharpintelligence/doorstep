package store

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sharpintelligence/doorstep/internal/core"
)

const (
	DefaultPageSize  = 50
	MaxPageSize      = 200
	MaxHistoryEvents = 500
)

type DeploymentFilter struct {
	App         string
	Environment string
	States      []core.State
	Kinds       []core.Kind
	CommitSHA   string
	Digest      core.Digest
	Before      core.DeploymentID
	Limit       int
}

type DeploymentPage struct {
	Items []core.Deployment
	Next  core.DeploymentID
}

type DeploymentHistory struct {
	Deployment      core.Deployment
	Events          []core.Event
	EventsTruncated bool
	Gates           []core.GateResult
	Rollbacks       []core.Deployment
	Retries         []core.Deployment
}

const listDeploymentsSQL = `SELECT` + deploymentColumns + deploymentTables + `
WHERE ($1::text = '' OR a.name = $1)
  AND ($2::text = '' OR a.environment = $2)
  AND (cardinality($3::text[]) = 0 OR d.state = ANY($3))
  AND (cardinality($4::text[]) = 0 OR d.kind = ANY($4))
  AND ($5::text = '' OR d.commit_sha = $5)
  AND ($6::text = '' OR d.image_digest = $6)
  AND ($7::bigint = 0 OR d.id < $7)
ORDER BY d.id DESC
LIMIT $8`

const selectEventsSQL = `
SELECT id, type, COALESCE(from_state, ''), COALESCE(to_state, ''), actor, message, data, occurred_at
FROM deployment_events
WHERE deployment_id = $1
ORDER BY id DESC
LIMIT $2`

const selectRelatedDeploymentsSQL = `SELECT` + deploymentColumns + deploymentTables + `
WHERE d.rollback_of_id = $1 OR d.parent_id = $1
ORDER BY d.id DESC`

const lastSuccessfulDeploymentSQL = `SELECT` + deploymentColumns + deploymentTables + `
WHERE a.name = $1
  AND a.environment = $2
  AND d.state = 'succeeded'
  AND d.finished_at IS NOT NULL
  AND d.image_digest IS NOT NULL
  AND ($3::text = '' OR d.image_digest IS DISTINCT FROM $3)
  AND NOT EXISTS (
      SELECT 1 FROM blocks b
      WHERE b.app_id = d.app_id AND b.digest = d.image_digest AND b.cleared_at IS NULL
  )
ORDER BY d.finished_at DESC, d.id DESC
LIMIT 1`

const currentDeploymentSQL = `SELECT` + deploymentColumns + deploymentTables + `
WHERE a.name = $1
  AND a.environment = $2
  AND (
      d.state = ANY($3::text[])
      OR d.state = 'succeeded'
      OR (d.state IN ('failed', 'cancelled') AND d.pin_commit_sha IS NOT NULL)
  )
ORDER BY COALESCE(d.started_at, d.created_at) DESC, d.id DESC
LIMIT 1`

func (f DeploymentFilter) validate() error {
	var errs []error
	for _, s := range f.States {
		if !s.Valid() {
			errs = append(errs, fmt.Errorf("%w: %q", core.ErrUnknownState, s))
		}
	}
	for _, k := range f.Kinds {
		if !k.Valid() {
			errs = append(errs, fmt.Errorf("unknown deployment kind %q", k))
		}
	}
	if f.CommitSHA != "" {
		errs = append(errs, core.ValidateCommitSHA(f.CommitSHA))
	}
	if f.Digest != "" {
		errs = append(errs, f.Digest.Validate())
	}
	if f.Before < 0 {
		errs = append(errs, fmt.Errorf("cursor must not be negative, got %d", f.Before))
	}
	return errors.Join(errs...)
}

func pageSize(limit int) int {
	if limit <= 0 {
		return DefaultPageSize
	}
	return min(limit, MaxPageSize)
}

func (s *Store) ListDeployments(ctx context.Context, f DeploymentFilter) (DeploymentPage, error) {
	if err := f.validate(); err != nil {
		return DeploymentPage{}, fmt.Errorf("list deployments: %w", err)
	}
	limit := pageSize(f.Limit)
	states := make([]string, 0, len(f.States))
	for _, st := range f.States {
		states = append(states, string(st))
	}
	kinds := make([]string, 0, len(f.Kinds))
	for _, k := range f.Kinds {
		kinds = append(kinds, string(k))
	}

	items, err := s.queryDeployments(ctx, listDeploymentsSQL,
		f.App, f.Environment, states, kinds, f.CommitSHA, f.Digest, f.Before, limit+1)
	if err != nil {
		return DeploymentPage{}, fmt.Errorf("list deployments: %w", err)
	}
	page := DeploymentPage{Items: items}
	if len(items) > limit {
		page.Items = items[:limit]
		page.Next = page.Items[limit-1].ID
	}
	return page, nil
}

func (s *Store) GetDeploymentHistory(ctx context.Context, id core.DeploymentID) (DeploymentHistory, error) {
	if s.pool == nil {
		return s.readHistory(ctx, id)
	}
	var h DeploymentHistory
	err := pgx.BeginTxFunc(ctx, s.pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		var err error
		h, err = (&Store{db: tx}).readHistory(ctx, id)
		return err
	})
	if err != nil {
		return DeploymentHistory{}, err
	}
	return h, nil
}

func (s *Store) readHistory(ctx context.Context, id core.DeploymentID) (DeploymentHistory, error) {
	d, err := s.GetDeployment(ctx, id)
	if err != nil {
		return DeploymentHistory{}, err
	}
	h := DeploymentHistory{Deployment: d}

	h.Events, err = s.events(ctx, d, MaxHistoryEvents+1)
	if err != nil {
		return DeploymentHistory{}, err
	}
	if len(h.Events) > MaxHistoryEvents {
		h.Events = h.Events[:MaxHistoryEvents]
		h.EventsTruncated = true
	}
	slices.Reverse(h.Events)

	h.Gates, err = s.gateResults(ctx, id)
	if err != nil {
		return DeploymentHistory{}, err
	}

	related, err := s.queryDeployments(ctx, selectRelatedDeploymentsSQL, id)
	if err != nil {
		return DeploymentHistory{}, fmt.Errorf("list deployments related to %d: %w", id, err)
	}
	for _, r := range related {
		if r.RollbackOfID != nil && *r.RollbackOfID == id {
			h.Rollbacks = append(h.Rollbacks, r)
		}
		if r.ParentID != nil && *r.ParentID == id {
			h.Retries = append(h.Retries, r)
		}
	}
	return h, nil
}

func (s *Store) events(ctx context.Context, d core.Deployment, limit int) ([]core.Event, error) {
	rows, err := s.db.Query(ctx, selectEventsSQL, d.ID, limit)
	if err != nil {
		return nil, fmt.Errorf("list deployment %d events: %w", d.ID, err)
	}
	events, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (core.Event, error) {
		e := core.Event{DeploymentID: d.ID, Target: d.Target}
		err := row.Scan(&e.ID, &e.Type, &e.From, &e.To, &e.Actor, &e.Message, &e.Data, &e.OccurredAt)
		return e, err
	})
	if err != nil {
		return nil, fmt.Errorf("list deployment %d events: %w", d.ID, err)
	}
	return events, nil
}

func (s *Store) gateResults(ctx context.Context, id core.DeploymentID) ([]core.GateResult, error) {
	gates, err := s.ListGateResults(ctx, id)
	if err != nil {
		return nil, err
	}
	slices.SortFunc(gates, func(a, b core.GateResult) int { return strings.Compare(a.Name, b.Name) })
	return gates, nil
}

func (s *Store) LastSuccessfulDeployment(ctx context.Context, t core.Target, excludeDigest core.Digest) (core.Deployment, error) {
	if err := t.Validate(); err != nil {
		return core.Deployment{}, fmt.Errorf("last successful deployment: %w", err)
	}
	if excludeDigest != "" {
		if err := excludeDigest.Validate(); err != nil {
			return core.Deployment{}, fmt.Errorf("last successful deployment: %w", err)
		}
	}
	d, err := scanDeployment(s.db.QueryRow(ctx, lastSuccessfulDeploymentSQL, t.App, t.Environment, excludeDigest))
	if errors.Is(err, pgx.ErrNoRows) {
		return core.Deployment{}, fmt.Errorf("last successful deployment of %s: %w", t, core.ErrNotFound)
	}
	if err != nil {
		return core.Deployment{}, fmt.Errorf("last successful deployment of %s: %w", t, err)
	}
	return d, nil
}

func (s *Store) CurrentDeployment(ctx context.Context, t core.Target) (core.Deployment, error) {
	if err := t.Validate(); err != nil {
		return core.Deployment{}, fmt.Errorf("current deployment: %w", err)
	}
	d, err := scanDeployment(s.db.QueryRow(ctx, currentDeploymentSQL, t.App, t.Environment, core.ActiveStates()))
	if errors.Is(err, pgx.ErrNoRows) {
		return core.Deployment{}, fmt.Errorf("current deployment of %s: %w", t, core.ErrNotFound)
	}
	if err != nil {
		return core.Deployment{}, fmt.Errorf("current deployment of %s: %w", t, err)
	}
	return d, nil
}

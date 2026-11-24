package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sharpintelligence/doorstep/internal/core"
)

type Transition struct {
	ID             core.DeploymentID
	From           core.State
	To             core.State
	Actor          string
	Message        string
	Data           json.RawMessage
	FailureReason  string
	SupersededByID *core.DeploymentID
	Lineage        core.Lineage
	ExpectRetries  *int
}

const transitionDeploymentSQL = `
UPDATE deployments
SET state = $7,
    started_at = CASE WHEN $8::boolean THEN COALESCE(started_at, now()) ELSE started_at END,
    finished_at = CASE WHEN $9::boolean THEN now() ELSE finished_at END,
    failure_reason = COALESCE(NULLIF($10::text, ''), failure_reason),
    superseded_by_id = COALESCE($11::bigint, superseded_by_id),
    ` + lineagePatchSQL + `
WHERE id = $1 AND state = $12
RETURNING app_id`

const deleteQueueEntrySQL = `DELETE FROM queue_entries WHERE deployment_id = $1`

const selectDeploymentStateSQL = `SELECT state FROM deployments WHERE id = $1`

const lockDeploymentSQL = `SELECT id FROM deployments WHERE id = $1 FOR UPDATE`

var (
	errMisplacedFailureReason = errors.New("a failure reason is only recorded on a transition to failed")
	errMisplacedSupersededBy  = errors.New("superseded by is only recorded on a transition to superseded")
	errMissingFailureReason   = errors.New("a transition to failed needs a failure reason")
	errMissingSupersededBy    = errors.New("a transition to superseded needs the deployment that supersedes it")
	errSupersededBySelf       = errors.New("a deployment cannot supersede itself")
	errEventDataNotObject     = errors.New("event data must be a JSON object")
	errEventDataLineageKey    = errors.New("event data must not set lineage keys; pass them as lineage")
)

func (s *Store) TransitionDeployment(ctx context.Context, t Transition) (core.Deployment, error) {
	data, err := t.validate()
	if err != nil {
		return core.Deployment{}, fmt.Errorf("transition deployment %d: %w", t.ID, err)
	}

	var updated core.Deployment
	err = s.InTx(ctx, func(tx *Store) error {
		if err := tx.checkRetries(ctx, t); err != nil {
			return err
		}
		var appID AppID
		err := tx.db.QueryRow(ctx, transitionDeploymentSQL,
			t.ID,
			t.Lineage.Digest, t.Lineage.PreviousDigest, t.Lineage.GitOpsRepository, t.Lineage.PinCommitSHA, t.Lineage.ArgoCDRevision,
			t.To, t.To.Active(), t.To.Terminal(), t.FailureReason, t.SupersededByID, t.From,
		).Scan(&appID)
		if errors.Is(err, pgx.ErrNoRows) {
			return tx.missedUpdate(ctx, t.ID, func(current core.State) error {
				return fmt.Errorf("%w: deployment %d is %s, expected %s", core.ErrStateConflict, t.ID, current, t.From)
			})
		}
		if err != nil {
			return fmt.Errorf("update deployment %d: %w", t.ID, err)
		}
		err = tx.insertEvent(ctx, event{
			deploymentID: t.ID,
			appID:        appID,
			typ:          core.EventTransition,
			from:         t.From,
			to:           t.To,
			actor:        t.Actor,
			message:      t.Message,
			data:         data,
		})
		if err != nil {
			return err
		}
		if t.To.Terminal() {
			if _, err := tx.db.Exec(ctx, deleteQueueEntrySQL, t.ID); err != nil {
				return fmt.Errorf("dequeue deployment %d: %w", t.ID, err)
			}
		}
		updated, err = tx.GetDeployment(ctx, t.ID)
		return err
	})
	if err != nil {
		return core.Deployment{}, fmt.Errorf("transition deployment %d %s -> %s: %w", t.ID, t.From, t.To, err)
	}
	return updated, nil
}

func (s *Store) checkRetries(ctx context.Context, t Transition) error {
	if t.ExpectRetries == nil {
		return nil
	}
	var id core.DeploymentID
	err := s.db.QueryRow(ctx, lockDeploymentSQL, t.ID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("lock deployment %d: %w", t.ID, err)
	}
	state, err := s.RetryState(ctx, t.ID)
	if err != nil {
		return err
	}
	if state.Retries != *t.ExpectRetries {
		return fmt.Errorf("%w: deployment %d has recorded %d retries, expected %d", core.ErrStateConflict, t.ID, state.Retries, *t.ExpectRetries)
	}
	return nil
}

func (t Transition) validate() ([]byte, error) {
	if err := t.From.Transition(t.To); err != nil {
		return nil, err
	}
	if t.FailureReason != "" && t.To != core.StateFailed {
		return nil, errMisplacedFailureReason
	}
	if t.SupersededByID != nil && t.To != core.StateSuperseded {
		return nil, errMisplacedSupersededBy
	}
	if t.To == core.StateFailed && strings.TrimSpace(t.FailureReason) == "" {
		return nil, errMissingFailureReason
	}
	if t.To == core.StateSuperseded {
		if t.SupersededByID == nil {
			return nil, errMissingSupersededBy
		}
		if *t.SupersededByID == t.ID {
			return nil, errSupersededBySelf
		}
	}
	if err := t.Lineage.Validate(); err != nil {
		return nil, err
	}
	return eventData(t.Data, t.Lineage)
}

func (s *Store) missedUpdate(ctx context.Context, id core.DeploymentID, conflict func(current core.State) error) error {
	var current core.State
	err := s.db.QueryRow(ctx, selectDeploymentStateSQL, id).Scan(&current)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("deployment %d: %w", id, core.ErrNotFound)
	}
	if err != nil {
		return fmt.Errorf("read deployment %d state: %w", id, err)
	}
	return conflict(current)
}

func eventData(data json.RawMessage, l core.Lineage) ([]byte, error) {
	fields := map[string]json.RawMessage{}
	if len(data) > 0 {
		if err := json.Unmarshal(data, &fields); err != nil || fields == nil {
			return nil, errEventDataNotObject
		}
	}
	var reserved []string
	for _, key := range lineageKeys {
		if _, ok := fields[key]; ok {
			reserved = append(reserved, key)
		}
	}
	if len(reserved) > 0 {
		return nil, fmt.Errorf("%w: %s", errEventDataLineageKey, strings.Join(reserved, ", "))
	}
	for key, value := range lineageFields(l) {
		encoded, err := json.Marshal(value)
		if err != nil {
			return nil, err
		}
		fields[key] = encoded
	}
	return json.Marshal(fields)
}

package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sharpintelligence/doorstep/internal/core"
)

const foreignKeyViolation = "23503"

const gateResultColumns = `deployment_id, name, type, phase, status, message, evidence, started_at, finished_at, updated_at`

const insertGateResultSQL = `
INSERT INTO gate_results (deployment_id, name, type, phase, status, message, evidence, finished_at)
VALUES ($1, $2, $3, $4, $5::text, $6, $7::jsonb, CASE WHEN $5::text = 'pending' THEN NULL ELSE now() END)
ON CONFLICT (deployment_id, name) DO NOTHING
RETURNING ` + gateResultColumns

const lockGateResultSQL = `
SELECT status FROM gate_results WHERE deployment_id = $1 AND name = $2 FOR UPDATE`

const updateGateResultSQL = `
UPDATE gate_results AS g SET
    type = $3,
    phase = $4,
    status = $5::text,
    message = $6,
    evidence = $7::jsonb,
    started_at = CASE
        WHEN g.status <> 'pending' AND $5::text = 'pending' THEN now()
        ELSE g.started_at
    END,
    finished_at = CASE
        WHEN $5::text = 'pending' THEN NULL
        WHEN g.status = $5::text THEN g.finished_at
        ELSE now()
    END
WHERE deployment_id = $1 AND name = $2
RETURNING ` + gateResultColumns

const insertGateEventSQL = `
INSERT INTO deployment_events (deployment_id, app_id, type, message, data)
SELECT d.id, d.app_id, 'gate', $2, $3::jsonb FROM deployments d WHERE d.id = $1`

const selectGateResultsSQL = `
SELECT ` + gateResultColumns + `
FROM gate_results
WHERE deployment_id = $1
ORDER BY id`

type gateEventData struct {
	Gate   string          `json:"gate"`
	Type   string          `json:"type"`
	Phase  core.GatePhase  `json:"phase"`
	Status core.GateStatus `json:"status"`
}

func (s *Store) RecordGateResult(ctx context.Context, r core.GateResult) (core.GateResult, bool, error) {
	evidence, err := validateGateResult(r)
	if err != nil {
		return core.GateResult{}, false, fmt.Errorf("record gate result: %w", err)
	}
	r.Message = core.CleanGateMessage(r.Message)

	var (
		stored  core.GateResult
		changed bool
	)
	err = s.InTx(ctx, func(tx *Store) error {
		var upsertErr error
		stored, changed, upsertErr = tx.upsertGateResult(ctx, r, evidence)
		if upsertErr != nil || !changed {
			return upsertErr
		}
		data, err := json.Marshal(gateEventData{Gate: stored.Name, Type: stored.Type, Phase: stored.Phase, Status: stored.Status})
		if err != nil {
			return fmt.Errorf("encode gate event: %w", err)
		}
		if _, err := tx.db.Exec(ctx, insertGateEventSQL, r.DeploymentID, stored.Name+" "+string(stored.Status), string(data)); err != nil {
			return fmt.Errorf("record gate event: %w", err)
		}
		return nil
	})
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == foreignKeyViolation {
		return core.GateResult{}, false, fmt.Errorf("record gate %s: deployment %d: %w", r.Name, r.DeploymentID, core.ErrNotFound)
	}
	if err != nil {
		return core.GateResult{}, false, fmt.Errorf("record gate %s for deployment %d: %w", r.Name, r.DeploymentID, err)
	}
	return stored, changed, nil
}

func (s *Store) upsertGateResult(ctx context.Context, r core.GateResult, evidence json.RawMessage) (core.GateResult, bool, error) {
	args := []any{r.DeploymentID, r.Name, r.Type, r.Phase, r.Status, r.Message, string(evidence)}
	inserted, err := scanGateResult(s.db.QueryRow(ctx, insertGateResultSQL, args...))
	if err == nil {
		return inserted, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return core.GateResult{}, false, fmt.Errorf("insert: %w", err)
	}
	var previous core.GateStatus
	if err := s.db.QueryRow(ctx, lockGateResultSQL, r.DeploymentID, r.Name).Scan(&previous); err != nil {
		return core.GateResult{}, false, fmt.Errorf("lock: %w", err)
	}
	updated, err := scanGateResult(s.db.QueryRow(ctx, updateGateResultSQL, args...))
	if err != nil {
		return core.GateResult{}, false, fmt.Errorf("update: %w", err)
	}
	return updated, previous != updated.Status, nil
}

func (s *Store) ListGateResults(ctx context.Context, id core.DeploymentID) ([]core.GateResult, error) {
	rows, err := s.db.Query(ctx, selectGateResultsSQL, id)
	if err != nil {
		return nil, fmt.Errorf("list gate results for deployment %d: %w", id, err)
	}
	results, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (core.GateResult, error) {
		return scanGateResult(row)
	})
	if err != nil {
		return nil, fmt.Errorf("list gate results for deployment %d: %w", id, err)
	}
	return results, nil
}

func validateGateResult(r core.GateResult) (json.RawMessage, error) {
	var errs []error
	if r.DeploymentID <= 0 {
		errs = append(errs, fmt.Errorf("invalid deployment id %d", r.DeploymentID))
	}
	if strings.TrimSpace(r.Name) == "" {
		errs = append(errs, errors.New("gate name is empty"))
	}
	if strings.TrimSpace(r.Type) == "" {
		errs = append(errs, errors.New("gate type is empty"))
	}
	if !r.Phase.Valid() {
		errs = append(errs, fmt.Errorf("unknown gate phase %q", r.Phase))
	}
	if !r.Status.Valid() {
		errs = append(errs, fmt.Errorf("unknown gate status %q", r.Status))
	}
	if err := core.ValidateGateEvidence(r.Evidence); err != nil {
		errs = append(errs, err)
	}
	evidence := r.Evidence
	if len(evidence) == 0 {
		evidence = json.RawMessage(`{}`)
	}
	return evidence, errors.Join(errs...)
}

func scanGateResult(row pgx.Row) (core.GateResult, error) {
	var (
		r        core.GateResult
		evidence []byte
	)
	if err := row.Scan(
		&r.DeploymentID, &r.Name, &r.Type, &r.Phase, &r.Status, &r.Message, &evidence,
		&r.StartedAt, &r.FinishedAt, &r.UpdatedAt,
	); err != nil {
		return core.GateResult{}, err
	}
	r.Evidence = json.RawMessage(evidence)
	return r, nil
}

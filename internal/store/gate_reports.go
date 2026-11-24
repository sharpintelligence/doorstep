package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sharpintelligence/doorstep/internal/core"
)

const gateReportColumns = `deployment_id, gate, status, message, url, details, reported_by, created_at, updated_at`

const insertGateReportSQL = `
INSERT INTO gate_reports (deployment_id, gate, status, message, url, details, reported_by)
VALUES ($1, $2, $3, $4, $5, $6::jsonb, $7)
ON CONFLICT (deployment_id, gate) DO NOTHING
RETURNING ` + gateReportColumns

const lockGateReportSQL = `
SELECT ` + gateReportColumns + `
FROM gate_reports
WHERE deployment_id = $1 AND gate = $2
FOR UPDATE`

const updateGateReportSQL = `
UPDATE gate_reports SET
    status = $3,
    message = $4,
    url = $5,
    details = $6::jsonb,
    reported_by = $7
WHERE deployment_id = $1 AND gate = $2
  AND (status, message, url, details, reported_by) IS DISTINCT FROM ($3, $4, $5, $6::jsonb, $7)
RETURNING ` + gateReportColumns

const selectGateReportSQL = `
SELECT ` + gateReportColumns + `
FROM gate_reports
WHERE deployment_id = $1 AND gate = $2`

const lockGateResultStatusSQL = `
SELECT status FROM gate_results
WHERE deployment_id = $1 AND name = $2
FOR UPDATE`

const selectGateReportStatusSQL = `
SELECT status FROM gate_reports
WHERE deployment_id = $1 AND gate = $2`

const activeDeploymentForCommitSQL = `SELECT` + deploymentColumns + deploymentTables + `
WHERE a.name = $1
  AND a.environment = $2
  AND d.commit_sha = $3
  AND d.state <> ALL($4::text[])
ORDER BY d.id DESC
LIMIT 1`

func (s *Store) GetGateReport(ctx context.Context, id core.DeploymentID, gate string) (core.GateReport, error) {
	r, err := scanGateReport(s.db.QueryRow(ctx, selectGateReportSQL, id, gate))
	if errors.Is(err, pgx.ErrNoRows) {
		return core.GateReport{}, fmt.Errorf("gate report %s of deployment %d: %w", gate, id, core.ErrNotFound)
	}
	if err != nil {
		return core.GateReport{}, fmt.Errorf("get gate report %s of deployment %d: %w", gate, id, err)
	}
	return r, nil
}

func (s *Store) SaveGateReport(ctx context.Context, r core.GateReport, audit core.AuditEntry) (core.GateReport, bool, error) {
	r.Message = core.CleanGateMessage(r.Message)
	if err := r.Validate(); err != nil {
		return core.GateReport{}, false, fmt.Errorf("save gate report: %w", err)
	}
	if err := audit.Validate(); err != nil {
		return core.GateReport{}, false, fmt.Errorf("save gate report: audit: %w", err)
	}
	details := r.Details
	if len(details) == 0 {
		details = json.RawMessage(`{}`)
	}
	args := []any{r.DeploymentID, r.Gate, r.Status, r.Message, r.URL, string(details), r.ReportedBy}

	var (
		stored  core.GateReport
		changed bool
	)
	err := s.InTx(ctx, func(tx *Store) error {
		var saveErr error
		stored, changed, saveErr = tx.upsertGateReport(ctx, r, args)
		if saveErr != nil || !changed {
			return saveErr
		}
		return tx.RecordAudit(ctx, audit)
	})
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == foreignKeyViolation {
		return core.GateReport{}, false, fmt.Errorf("save gate report %s: deployment %d: %w", r.Gate, r.DeploymentID, core.ErrNotFound)
	}
	if err != nil {
		return core.GateReport{}, false, fmt.Errorf("save gate report %s for deployment %d: %w", r.Gate, r.DeploymentID, err)
	}
	return stored, changed, nil
}

func (s *Store) upsertGateReport(ctx context.Context, r core.GateReport, args []any) (core.GateReport, bool, error) {
	inserted, err := scanGateReport(s.db.QueryRow(ctx, insertGateReportSQL, args...))
	if err == nil {
		if err := s.checkGateUndecided(ctx, r); err != nil {
			return core.GateReport{}, false, err
		}
		return inserted, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return core.GateReport{}, false, fmt.Errorf("insert: %w", err)
	}
	existing, err := scanGateReport(s.db.QueryRow(ctx, lockGateReportSQL, r.DeploymentID, r.Gate))
	if err != nil {
		return core.GateReport{}, false, fmt.Errorf("lock: %w", err)
	}
	if existing.Status.Terminal() {
		if existing.Status == r.Status {
			return existing, false, nil
		}
		return core.GateReport{}, false, fmt.Errorf("%w: gate %s was reported %s", core.ErrGateDecided, r.Gate, existing.Status)
	}
	if err := s.checkGateUndecided(ctx, r); err != nil {
		return core.GateReport{}, false, err
	}
	updated, err := scanGateReport(s.db.QueryRow(ctx, updateGateReportSQL, args...))
	if errors.Is(err, pgx.ErrNoRows) {
		return existing, false, nil
	}
	if err != nil {
		return core.GateReport{}, false, fmt.Errorf("update: %w", err)
	}
	return updated, true, nil
}

func (s *Store) checkGateUndecided(ctx context.Context, r core.GateReport) error {
	var status core.GateStatus
	err := s.db.QueryRow(ctx, lockGateResultStatusSQL, r.DeploymentID, r.Gate).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("lock gate result: %w", err)
	}
	if status.Terminal() {
		return fmt.Errorf("%w: gate %s already has a final result", core.ErrGateDecided, r.Gate)
	}
	return nil
}

func (s *Store) checkReportAgrees(ctx context.Context, r core.GateResult) error {
	if !r.Status.Terminal() {
		return nil
	}
	var reported core.GateStatus
	err := s.db.QueryRow(ctx, selectGateReportStatusSQL, r.DeploymentID, r.Name).Scan(&reported)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read gate report: %w", err)
	}
	if reported.Terminal() && reported != r.Status {
		return fmt.Errorf("%w: gate %s was reported %s, not %s", core.ErrGateReported, r.Name, reported, r.Status)
	}
	return nil
}

func (s *Store) ActiveDeploymentForCommit(ctx context.Context, t core.Target, sha string) (core.Deployment, error) {
	if err := t.Validate(); err != nil {
		return core.Deployment{}, fmt.Errorf("active deployment for commit: %w", err)
	}
	if err := core.ValidateCommitSHA(sha); err != nil {
		return core.Deployment{}, fmt.Errorf("active deployment for commit: %w", err)
	}
	ds, err := s.queryDeployments(ctx, activeDeploymentForCommitSQL, t.App, t.Environment, sha, core.TerminalStates())
	if err != nil {
		return core.Deployment{}, fmt.Errorf("active deployment of %s for commit %s: %w", t, sha, err)
	}
	if len(ds) == 0 {
		return core.Deployment{}, fmt.Errorf("active deployment of %s for commit %s: %w", t, sha, core.ErrNotFound)
	}
	return ds[0], nil
}

func scanGateReport(row pgx.Row) (core.GateReport, error) {
	var (
		r       core.GateReport
		details []byte
	)
	if err := row.Scan(
		&r.DeploymentID, &r.Gate, &r.Status, &r.Message, &r.URL, &details, &r.ReportedBy, &r.CreatedAt, &r.UpdatedAt,
	); err != nil {
		return core.GateReport{}, err
	}
	r.Details = json.RawMessage(details)
	return r, nil
}

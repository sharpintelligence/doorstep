package store

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sharpintelligence/doorstep/internal/core"
)

const (
	DefaultAuditLimit = 50
	MaxAuditLimit     = 200
)

type AuditFilter struct {
	Target   *core.Target
	Actor    *core.Actor
	Action   core.AuditAction
	BeforeID int64
	Limit    int
}

const insertAuditSQL = `
INSERT INTO audit_log (actor_kind, actor_id, actor_name, actor_role, action, app, environment, deployment_id, detail)
VALUES ($1, $2, $3, NULLIF($4, ''), $5, $6, $7, $8, COALESCE($9::jsonb, '{}'))`

const selectAuditSQL = `
SELECT id, occurred_at, actor_kind, actor_id, actor_name, COALESCE(actor_role, ''), action, app, environment, deployment_id, detail
FROM audit_log`

func (s *Store) RecordAudit(ctx context.Context, e core.AuditEntry) error {
	if err := e.Validate(); err != nil {
		return fmt.Errorf("record audit %s: %w", e.Action, err)
	}
	var app, environment *string
	if e.Target != nil {
		app, environment = &e.Target.App, &e.Target.Environment
	}
	var detail *string
	if len(e.Detail) > 0 {
		d := string(e.Detail)
		detail = &d
	}
	_, err := s.db.Exec(ctx, insertAuditSQL,
		e.Actor.Kind, e.Actor.ID, e.Actor.Name, e.Role, e.Action, app, environment, e.DeploymentID, detail)
	if err != nil {
		return fmt.Errorf("record audit %s: %w", e.Action, err)
	}
	return nil
}

func (s *Store) ListAudit(ctx context.Context, f AuditFilter) ([]core.AuditEntry, error) {
	var conditions []string
	var args []any
	arg := func(v any) string {
		args = append(args, v)
		return "$" + strconv.Itoa(len(args))
	}
	if f.Target != nil {
		conditions = append(conditions, "app = "+arg(f.Target.App)+" AND environment = "+arg(f.Target.Environment))
	}
	if f.Actor != nil {
		conditions = append(conditions, "actor_kind = "+arg(f.Actor.Kind)+" AND actor_id = "+arg(f.Actor.ID))
	}
	if f.Action != "" {
		conditions = append(conditions, "action = "+arg(f.Action))
	}
	if f.BeforeID > 0 {
		conditions = append(conditions, "id < "+arg(f.BeforeID))
	}
	query := selectAuditSQL
	if len(conditions) > 0 {
		query += "\nWHERE " + strings.Join(conditions, " AND ")
	}
	query += "\nORDER BY id DESC\nLIMIT " + arg(clampAuditLimit(f.Limit))

	rows, err := s.db.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list audit: %w", err)
	}
	entries, err := pgx.CollectRows(rows, scanAuditEntry)
	if err != nil {
		return nil, fmt.Errorf("list audit: %w", err)
	}
	return entries, nil
}

func clampAuditLimit(limit int) int {
	switch {
	case limit <= 0:
		return DefaultAuditLimit
	case limit > MaxAuditLimit:
		return MaxAuditLimit
	default:
		return limit
	}
}

func scanAuditEntry(row pgx.CollectableRow) (core.AuditEntry, error) {
	var e core.AuditEntry
	var app, environment *string
	err := row.Scan(&e.ID, &e.OccurredAt, &e.Actor.Kind, &e.Actor.ID, &e.Actor.Name, &e.Role, &e.Action,
		&app, &environment, &e.DeploymentID, &e.Detail)
	if err != nil {
		return core.AuditEntry{}, err
	}
	if app != nil && environment != nil {
		e.Target = &core.Target{App: *app, Environment: *environment}
	}
	return e, nil
}

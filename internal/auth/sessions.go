package auth

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/store"
)

type Sessions struct {
	repo Repository
}

type RevokeSessionsInput struct {
	User string
	All  bool
	By   Requester
}

func NewSessions(repo Repository) *Sessions {
	return &Sessions{repo: repo}
}

func (s *Sessions) List(ctx context.Context) ([]store.Session, error) {
	return s.repo.ListSessions(ctx)
}

func (s *Sessions) Revoke(ctx context.Context, in RevokeSessionsInput) (int64, error) {
	var errs []error
	user := strings.TrimSpace(in.User)
	if (user == "") == !in.All {
		errs = append(errs, errors.New("name exactly one user, or all sessions"))
	}
	if err := in.By.Actor.Validate(); err != nil {
		errs = append(errs, fmt.Errorf("revoker: %w", err))
	}
	if err := errors.Join(errs...); err != nil {
		return 0, err
	}

	var revoked int64
	err := s.repo.InTx(ctx, func(tx Repository) error {
		var err error
		detail := map[string]any{}
		if in.All {
			revoked, err = tx.DeleteAllSessions(ctx)
			detail["all"] = true
		} else {
			revoked, err = tx.DeleteUserSessions(ctx, user)
			detail["user"] = user
		}
		if err != nil {
			return err
		}
		detail["sessions"] = revoked
		return tx.RecordAudit(ctx, core.AuditEntry{Actor: in.By.Actor, Action: core.AuditSessionRevoke, Detail: in.By.auditDetail(detail)})
	})
	if err != nil {
		return 0, err
	}
	return revoked, nil
}

package core

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

type AuditAction string

const (
	AuditDeployRequest   AuditAction = "deploy.request"
	AuditDeployCancel    AuditAction = "deploy.cancel"
	AuditRollbackRequest AuditAction = "rollback.request"
	AuditLockAcquire     AuditAction = "lock.acquire"
	AuditLockRelease     AuditAction = "lock.release"
	AuditTokenCreate     AuditAction = "token.create"
	AuditTokenRevoke     AuditAction = "token.revoke"
	AuditLogin           AuditAction = "auth.login"
	AuditLogout          AuditAction = "auth.logout"
)

func AuditActions() []AuditAction {
	return []AuditAction{
		AuditDeployRequest,
		AuditDeployCancel,
		AuditRollbackRequest,
		AuditLockAcquire,
		AuditLockRelease,
		AuditTokenCreate,
		AuditTokenRevoke,
		AuditLogin,
		AuditLogout,
	}
}

func (a AuditAction) Valid() bool {
	segments := strings.Split(string(a), ".")
	if len(segments) < 2 {
		return false
	}
	for _, segment := range segments {
		if segment == "" || strings.ContainsFunc(segment, isNotActionRune) {
			return false
		}
	}
	return true
}

func isNotActionRune(r rune) bool {
	return (r < 'a' || r > 'z') && r != '_'
}

type AuditEntry struct {
	ID           int64
	OccurredAt   time.Time
	Actor        Actor
	Role         Role
	Action       AuditAction
	Target       *Target
	DeploymentID *DeploymentID
	Detail       json.RawMessage
}

func (e AuditEntry) Validate() error {
	errs := []error{prefixError("actor", e.Actor.Validate())}
	if e.Role != "" && !e.Role.Valid() {
		errs = append(errs, fmt.Errorf("%w %q", ErrUnknownRole, e.Role))
	}
	if !e.Action.Valid() {
		errs = append(errs, fmt.Errorf("audit action %q must be dot-separated lowercase words, such as deploy.request", e.Action))
	}
	if e.Target != nil {
		errs = append(errs, e.Target.Validate())
	}
	if len(e.Detail) > 0 && !isJSONObject(e.Detail) {
		errs = append(errs, errors.New("audit detail must be a JSON object"))
	}
	return errors.Join(errs...)
}

func isJSONObject(data []byte) bool {
	return json.Valid(data) && bytes.HasPrefix(bytes.TrimSpace(data), []byte("{"))
}

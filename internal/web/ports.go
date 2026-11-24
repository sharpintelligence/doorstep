package web

import (
	"context"

	"github.com/sharpintelligence/doorstep/internal/auth"
	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/deploys"
	"github.com/sharpintelligence/doorstep/internal/rollback"
	"github.com/sharpintelligence/doorstep/internal/store"
)

type History interface {
	ListDeployments(ctx context.Context, f store.DeploymentFilter) (store.DeploymentPage, error)
	GetDeploymentHistory(ctx context.Context, id core.DeploymentID) (store.DeploymentHistory, error)
	CurrentDeployment(ctx context.Context, t core.Target) (core.Deployment, error)
	CurrentDeployments(ctx context.Context) ([]core.Deployment, error)
	LatestDeployments(ctx context.Context) ([]core.Deployment, error)
}

type Queue interface {
	Entries(ctx context.Context, app string) ([]core.QueueEntry, error)
}

type Locks interface {
	Lock(ctx context.Context, req core.LockRequest) (core.Lock, error)
	Unlock(ctx context.Context, target *core.Target, actor string) (core.Lock, error)
	List(ctx context.Context) ([]core.Lock, error)
}

type LockEvents interface {
	ListLockEvents(ctx context.Context, limit int) ([]core.LockEvent, error)
}

type Rollbacks interface {
	Rollback(ctx context.Context, req rollback.Request) (rollback.Result, error)
}

type Blocks interface {
	ListBlocks(ctx context.Context, t core.Target) ([]core.Block, error)
	Unblock(ctx context.Context, t core.Target, ref core.BlockRef, actor core.Actor, role core.Role) ([]core.Block, error)
}

type Deploys interface {
	Deploy(ctx context.Context, req deploys.Request) (deploys.Result, error)
	Cancel(ctx context.Context, req deploys.CancelRequest) (core.Deployment, error)
}

type Audit interface {
	ListAudit(ctx context.Context, f store.AuditFilter) ([]core.AuditEntry, error)
}

type Tokens interface {
	Create(ctx context.Context, in auth.CreateTokenInput) (string, store.APIToken, error)
	List(ctx context.Context) ([]store.APIToken, error)
	Revoke(ctx context.Context, name string, by auth.Requester) error
}

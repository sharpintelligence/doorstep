package store

import (
	"context"
	"errors"
	"fmt"
	"maps"

	"github.com/jackc/pgx/v5"

	"github.com/sharpintelligence/doorstep/internal/core"
)

const lineagePatchSQL = `image_digest = COALESCE(NULLIF($2::text, ''), image_digest),
    previous_digest = COALESCE(NULLIF($3::text, ''), previous_digest),
    gitops_repository = COALESCE(NULLIF($4::text, ''), gitops_repository),
    pin_commit_sha = COALESCE(NULLIF($5::text, ''), pin_commit_sha),
    argocd_revision = COALESCE(NULLIF($6::text, ''), argocd_revision)`

const setLineageSQL = `
UPDATE deployments
SET ` + lineagePatchSQL + `
WHERE id = $1 AND state <> ALL($7::text[])
RETURNING app_id`

const lineageRecordedMessage = "lineage recorded"

var errEmptyLineage = errors.New("lineage patch sets no fields")

func (s *Store) SetLineage(ctx context.Context, id core.DeploymentID, l core.Lineage, actor string) (core.Deployment, error) {
	if l.IsZero() {
		return core.Deployment{}, fmt.Errorf("set deployment %d lineage: %w", id, errEmptyLineage)
	}
	if err := l.Validate(); err != nil {
		return core.Deployment{}, fmt.Errorf("set deployment %d lineage: %w", id, err)
	}
	data, err := eventData(nil, l)
	if err != nil {
		return core.Deployment{}, fmt.Errorf("set deployment %d lineage: %w", id, err)
	}

	var updated core.Deployment
	err = s.InTx(ctx, func(tx *Store) error {
		var appID AppID
		err := tx.db.QueryRow(ctx, setLineageSQL,
			id, l.Digest, l.PreviousDigest, l.GitOpsRepository, l.PinCommitSHA, l.ArgoCDRevision, core.TerminalStates(),
		).Scan(&appID)
		if errors.Is(err, pgx.ErrNoRows) {
			return tx.missedUpdate(ctx, id, func(current core.State) error {
				return fmt.Errorf("%w: deployment %d is %s, lineage of a finished deployment is final", core.ErrStateConflict, id, current)
			})
		}
		if err != nil {
			return fmt.Errorf("update deployment %d: %w", id, err)
		}
		err = tx.insertEvent(ctx, event{
			deploymentID: id,
			appID:        appID,
			typ:          core.EventNote,
			actor:        actor,
			message:      lineageRecordedMessage,
			data:         data,
		})
		if err != nil {
			return err
		}
		updated, err = tx.GetDeployment(ctx, id)
		return err
	})
	if err != nil {
		return core.Deployment{}, fmt.Errorf("set deployment %d lineage: %w", id, err)
	}
	return updated, nil
}

func lineageFields(l core.Lineage) map[string]string {
	fields := map[string]string{
		"image_digest":      l.Digest.String(),
		"previous_digest":   l.PreviousDigest.String(),
		"gitops_repository": l.GitOpsRepository,
		"pin_commit_sha":    l.PinCommitSHA,
		"argocd_revision":   l.ArgoCDRevision,
	}
	maps.DeleteFunc(fields, func(_, value string) bool { return value == "" })
	return fields
}

package core

import (
	"errors"
	"fmt"
	"slices"
	"time"
)

type DeploymentID int64

type Kind string

const (
	KindDeploy   Kind = "deploy"
	KindRollback Kind = "rollback"
)

func Kinds() []Kind {
	return []Kind{KindDeploy, KindRollback}
}

func (k Kind) Valid() bool {
	return slices.Contains(Kinds(), k)
}

type Source string

const (
	SourceWebhook Source = "webhook"
	SourcePoll    Source = "poll"
	SourceAPI     Source = "api"
	SourceCLI     Source = "cli"
	SourceSystem  Source = "system"
)

func Sources() []Source {
	return []Source{SourceWebhook, SourcePoll, SourceAPI, SourceCLI, SourceSystem}
}

func (s Source) Valid() bool {
	return slices.Contains(Sources(), s)
}

type Deployment struct {
	ID               DeploymentID
	Target           Target
	Kind             Kind
	State            State
	Source           Source
	RequestedBy      string
	Commit           Commit
	Image            ImageRef
	Digest           Digest
	PreviousDigest   Digest
	PinCommitSHA     string
	GitOpsRepository string
	ArgoCDRevision   string
	ParentID         *DeploymentID
	RollbackOfID     *DeploymentID
	SupersededByID   *DeploymentID
	FailureReason    string
	CreatedAt        time.Time
	UpdatedAt        time.Time
	StartedAt        *time.Time
	FinishedAt       *time.Time
}

func (d Deployment) Validate() error {
	errs := []error{d.Target.Validate(), d.Commit.Validate(), d.Image.Validate()}
	if !d.Kind.Valid() {
		errs = append(errs, fmt.Errorf("unknown deployment kind %q", d.Kind))
	}
	if !d.State.Valid() {
		errs = append(errs, fmt.Errorf("%w: %q", ErrUnknownState, d.State))
	}
	if !d.Source.Valid() {
		errs = append(errs, fmt.Errorf("unknown deployment source %q", d.Source))
	}
	if d.Digest != "" {
		errs = append(errs, d.Digest.Validate())
	}
	if d.PreviousDigest != "" {
		errs = append(errs, prefixError("previous digest", d.PreviousDigest.Validate()))
	}
	if d.PinCommitSHA != "" {
		errs = append(errs, prefixError("pin commit", ValidateCommitSHA(d.PinCommitSHA)))
	}
	switch d.Kind {
	case KindRollback:
		if d.RollbackOfID == nil {
			errs = append(errs, errors.New("rollback deployment must name the deployment it rolls back"))
		}
	case KindDeploy:
		if d.RollbackOfID != nil {
			errs = append(errs, errors.New("only rollback deployments may name a deployment they roll back"))
		}
	}
	return errors.Join(errs...)
}

func prefixError(prefix string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", prefix, err)
}

package cli

import (
	"context"
	"errors"
	"fmt"

	"github.com/sharpintelligence/doorstep/internal/gates"
	"github.com/sharpintelligence/doorstep/internal/jobs"
	"github.com/sharpintelligence/doorstep/internal/kube"
)

const conditionTrue = "True"

type jobReader struct {
	reader *jobs.Reader
}

func (r jobReader) Job(ctx context.Context, namespace, name string) (gates.Job, error) {
	job, err := r.reader.Job(ctx, namespace, name)
	if errors.Is(err, jobs.ErrNotFound) {
		return gates.Job{}, fmt.Errorf("%w: %w", gates.ErrJobNotFound, err)
	}
	if err != nil {
		return gates.Job{}, err
	}
	return gatesJob(job), nil
}

func gatesJob(job jobs.Job) gates.Job {
	out := gates.Job{
		UID:          job.UID,
		CreatedAt:    job.CreatedAt,
		Deleting:     job.Deleting,
		Annotations:  job.Annotations,
		Suspended:    job.Suspended,
		BackoffLimit: job.BackoffLimit,
		Active:       job.Active,
		Succeeded:    job.Succeeded,
		Failed:       job.Failed,
		StartedAt:    job.StartedAt,
		CompletedAt:  job.CompletedAt,
	}
	for _, c := range job.Conditions {
		if c.Status != conditionTrue {
			continue
		}
		switch c.Type {
		case "Complete":
			out.Complete = true
		case "Failed":
			out.FailedCondition = true
			out.Reason = c.Reason
			out.Message = c.Message
		}
	}
	return out
}

func registerJobGate(registry *gates.Registry, client *kube.Client) error {
	if client == nil {
		return errors.New("register job gate: no kubernetes client")
	}
	reader, err := jobs.New(client)
	if err != nil {
		return fmt.Errorf("register job gate: %w", err)
	}
	return gates.RegisterJobGate(registry, gates.JobGateDeps{Jobs: jobReader{reader: reader}})
}

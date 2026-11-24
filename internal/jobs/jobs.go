package jobs

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"time"

	"github.com/sharpintelligence/doorstep/internal/kube"
)

const DefaultBackoffLimit int32 = 6

var ErrNotFound = errors.New("kubernetes job not found")

type Getter interface {
	Get(ctx context.Context, path string, query url.Values, out any) error
}

type Reader struct {
	client Getter
}

type Job struct {
	UID          string
	CreatedAt    time.Time
	Deleting     bool
	Annotations  map[string]string
	Suspended    bool
	BackoffLimit int32
	Active       int32
	Succeeded    int32
	Failed       int32
	StartedAt    time.Time
	CompletedAt  time.Time
	Conditions   []Condition
}

type Condition struct {
	Type    string
	Status  string
	Reason  string
	Message string
}

func New(client Getter) (*Reader, error) {
	if client == nil {
		return nil, errors.New("jobs: a kubernetes client is required")
	}
	return &Reader{client: client}, nil
}

func (r *Reader) Job(ctx context.Context, namespace, name string) (Job, error) {
	if !validSegment(namespace) || !validSegment(name) {
		return Job{}, fmt.Errorf("jobs: invalid job reference %q/%q", namespace, name)
	}
	var raw jobObject
	err := r.client.Get(ctx, jobPath(namespace, name), nil, &raw)
	switch {
	case kube.IsNotFound(err):
		return Job{}, fmt.Errorf("job %s/%s: %w: %w", namespace, name, ErrNotFound, err)
	case err != nil:
		return Job{}, fmt.Errorf("job %s/%s: %w", namespace, name, err)
	}
	return raw.job(), nil
}

func validSegment(s string) bool {
	return s != "" && s != "." && s != ".."
}

func jobPath(namespace, name string) string {
	return "/apis/batch/v1/namespaces/" + url.PathEscape(namespace) + "/jobs/" + url.PathEscape(name)
}

type jobObject struct {
	Metadata struct {
		UID               string            `json:"uid"`
		CreationTimestamp string            `json:"creationTimestamp"`
		DeletionTimestamp string            `json:"deletionTimestamp"`
		Annotations       map[string]string `json:"annotations"`
	} `json:"metadata"`
	Spec struct {
		Suspend      *bool  `json:"suspend"`
		BackoffLimit *int32 `json:"backoffLimit"`
	} `json:"spec"`
	Status struct {
		Active         int32  `json:"active"`
		Succeeded      int32  `json:"succeeded"`
		Failed         int32  `json:"failed"`
		StartTime      string `json:"startTime"`
		CompletionTime string `json:"completionTime"`
		Conditions     []struct {
			Type    string `json:"type"`
			Status  string `json:"status"`
			Reason  string `json:"reason"`
			Message string `json:"message"`
		} `json:"conditions"`
	} `json:"status"`
}

func (o jobObject) job() Job {
	j := Job{
		UID:          o.Metadata.UID,
		CreatedAt:    parseTime(o.Metadata.CreationTimestamp),
		Deleting:     o.Metadata.DeletionTimestamp != "",
		Annotations:  o.Metadata.Annotations,
		Suspended:    o.Spec.Suspend != nil && *o.Spec.Suspend,
		BackoffLimit: DefaultBackoffLimit,
		Active:       o.Status.Active,
		Succeeded:    o.Status.Succeeded,
		Failed:       o.Status.Failed,
		StartedAt:    parseTime(o.Status.StartTime),
		CompletedAt:  parseTime(o.Status.CompletionTime),
	}
	if o.Spec.BackoffLimit != nil {
		j.BackoffLimit = *o.Spec.BackoffLimit
	}
	for _, c := range o.Status.Conditions {
		j.Conditions = append(j.Conditions, Condition{Type: c.Type, Status: c.Status, Reason: c.Reason, Message: c.Message})
	}
	return j
}

func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}

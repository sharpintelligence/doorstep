package gitops

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net/http"
	"strings"
	"time"

	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/github"
)

const (
	DefaultPollInterval = 10 * time.Second
	DefaultMergeTimeout = 30 * time.Minute
	DefaultMaxAttempts  = 5

	initialBackoff = 200 * time.Millisecond
	historyDepth   = 20
)

type RepoOpener interface {
	Repo(ctx context.Context, owner, name string) (*github.Repo, error)
}

type Options struct {
	PollInterval  time.Duration
	MergeTimeout  time.Duration
	MaxAttempts   int
	Wait          func(ctx context.Context, d time.Duration) error
	DeploymentURL func(core.DeploymentID) string
	Logger        *slog.Logger
}

type Writer struct {
	repos  RepoOpener
	opts   Options
	logger *slog.Logger
}

func New(repos RepoOpener, opts Options) *Writer {
	if opts.PollInterval <= 0 {
		opts.PollInterval = DefaultPollInterval
	}
	if opts.MergeTimeout <= 0 {
		opts.MergeTimeout = DefaultMergeTimeout
	}
	if opts.MaxAttempts <= 0 {
		opts.MaxAttempts = DefaultMaxAttempts
	}
	if opts.Wait == nil {
		opts.Wait = sleep
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Writer{repos: repos, opts: opts, logger: logger}
}

func (w *Writer) Pin(ctx context.Context, req Request) (Result, error) {
	if err := req.Validate(); err != nil {
		return Result{}, fmt.Errorf("%w: %w", ErrInvalidRequest, err)
	}
	repo, err := w.repos.Repo(ctx, req.Target.Owner, req.Target.Repo)
	if err != nil {
		return Result{}, err
	}
	var result Result
	if req.Target.Mode == ModePullRequest {
		result, err = w.pinPullRequest(ctx, repo, req)
	} else {
		result, err = w.pinCommit(ctx, repo, req)
	}
	if err != nil {
		return Result{}, fmt.Errorf("pin %s for deployment %d: %w", req.Target.App, req.Deployment, err)
	}
	w.logger.InfoContext(ctx, "pin written",
		slog.String("app", req.Target.App),
		slog.Int64("deployment", int64(req.Deployment)),
		slog.String("digest", string(req.Digest)),
		slog.String("commit", result.Commit.SHA),
		slog.Bool("changed", result.Changed),
		slog.Int("pull_request", result.PullRequest))
	return result, nil
}

func (w *Writer) Current(ctx context.Context, t Target) (Pinned, error) {
	if err := t.Validate(); err != nil {
		return Pinned{}, fmt.Errorf("%w: %w", ErrInvalidRequest, err)
	}
	repo, err := w.repos.Repo(ctx, t.Owner, t.Repo)
	if err != nil {
		return Pinned{}, err
	}
	file, value, err := w.load(ctx, repo, t, t.Branch)
	if err != nil {
		return Pinned{}, fmt.Errorf("read pin of %s: %w", t.App, err)
	}
	return Pinned{Digest: t.digestOf(value), Value: value, File: file.Path}, nil
}

func (w *Writer) load(ctx context.Context, repo *github.Repo, t Target, ref string) (github.File, string, error) {
	var lastErr error
	for _, path := range t.candidateFiles() {
		file, value, err := w.loadAt(ctx, repo, t, path, ref)
		if github.HasStatus(err, http.StatusNotFound) {
			lastErr = err
			continue
		}
		return file, value, err
	}
	return github.File{}, "", fmt.Errorf("%w: no %s on %s in %s: %w",
		ErrPinNotFound, strings.Join(t.candidateFiles(), " or "), ref, repo.FullName(), lastErr)
}

func (w *Writer) loadAt(ctx context.Context, repo *github.Repo, t Target, path, ref string) (github.File, string, error) {
	file, err := repo.GetFile(ctx, path, ref)
	if err != nil {
		return github.File{}, "", err
	}
	value, err := readValue(editorFor(t), file.Content)
	if err != nil {
		return github.File{}, "", fmt.Errorf("%s on %s: %w", path, ref, err)
	}
	return file, value, nil
}

func (w *Writer) alreadyPinned(ctx context.Context, repo *github.Repo, req Request, path string) (Result, error) {
	commits, err := repo.ListCommits(ctx, req.Target.Branch, path, historyDepth)
	if err != nil {
		return Result{}, err
	}
	if len(commits) == 0 {
		return Result{}, fmt.Errorf("no commit on %s touches %s", req.Target.Branch, path)
	}
	for _, c := range commits {
		t := ParseMessage(c.Message)
		if t.Deployment == req.Deployment && t.App == req.Target.App && t.Digest == req.Digest {
			return Result{Commit: core.Commit{Repository: repo.FullName(), SHA: c.SHA}, Previous: t.PreviousDigest}, nil
		}
	}
	return Result{Commit: core.Commit{Repository: repo.FullName(), SHA: commits[0].SHA}}, nil
}

func (w *Writer) message(req Request, previous core.Digest) (string, string, error) {
	url := ""
	if w.opts.DeploymentURL != nil {
		url = w.opts.DeploymentURL(req.Deployment)
	}
	return FormatMessage(req, previous, url)
}

func (w *Writer) retryConflicts(ctx context.Context, what string, attempt func() error) error {
	for n := 1; ; n++ {
		err := attempt()
		if err == nil || !isWriteConflict(err) {
			return err
		}
		if n >= w.opts.MaxAttempts {
			return fmt.Errorf("%w: %s after %d attempts: %w", ErrConflict, what, n, err)
		}
		delay := backoff(n)
		w.logger.DebugContext(ctx, "pin write conflicted, retrying",
			slog.String("write", what), slog.Int("attempt", n), slog.Duration("delay", delay), slog.Any("error", err))
		if err := w.opts.Wait(ctx, delay); err != nil {
			return err
		}
	}
}

func isWriteConflict(err error) bool {
	var apiErr *github.APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	switch apiErr.StatusCode {
	case http.StatusConflict:
		return true
	case http.StatusUnprocessableEntity:
		msg := strings.ToLower(apiErr.Message)
		return strings.Contains(msg, "sha") && (strings.Contains(msg, "match") || strings.Contains(msg, "supplied"))
	default:
		return false
	}
}

func backoff(attempt int) time.Duration {
	d := initialBackoff << (attempt - 1)
	jitter, err := rand.Int(rand.Reader, big.NewInt(int64(d/2)+1))
	if err != nil {
		return d
	}
	return d/2 + time.Duration(jitter.Int64())
}

func sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

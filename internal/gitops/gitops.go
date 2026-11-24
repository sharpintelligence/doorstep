package gitops

import (
	"context"
	"crypto/rand"
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
	maxBackoff     = 10 * time.Second
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
	if _, err := repo.BranchSHA(ctx, ref); err != nil {
		return github.File{}, "", err
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
	commits, err := w.pinCommits(ctx, repo, req, path)
	if err != nil {
		return Result{}, err
	}
	var result Result
	for _, c := range commits {
		if c.trailers.Deployment == req.Deployment {
			return Result{Commit: core.Commit{Repository: repo.FullName(), SHA: c.sha}, Previous: c.trailers.PreviousDigest}, nil
		}
		if result.Commit.SHA == "" {
			result.Commit = core.Commit{Repository: repo.FullName(), SHA: c.sha}
		}
	}
	return result, nil
}

func (w *Writer) checkNotOverwritten(ctx context.Context, repo *github.Repo, req Request, path string) error {
	commits, err := w.pinCommits(ctx, repo, req, path)
	if err != nil {
		return err
	}
	for _, c := range commits {
		if c.trailers.Deployment == req.Deployment {
			return fmt.Errorf("%w: %s pinned %s on %s, and a later commit changed it", ErrPinOverwritten, c.sha, req.Digest, req.Target.Branch)
		}
	}
	return nil
}

type pinCommit struct {
	sha      string
	trailers Trailers
}

func (w *Writer) pinCommits(ctx context.Context, repo *github.Repo, req Request, path string) ([]pinCommit, error) {
	commits, err := repo.ListCommits(ctx, req.Target.Branch, path, historyDepth)
	if err != nil {
		return nil, err
	}
	var out []pinCommit
	for _, c := range commits {
		t := ParseMessage(c.Message)
		if t.App == req.Target.App && t.Digest == req.Digest {
			out = append(out, pinCommit{sha: c.SHA, trailers: t})
		}
	}
	return out, nil
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
		switch {
		case err == nil:
			return nil
		case isBranchProtected(err):
			return fmt.Errorf("%w: %s: %w", ErrBranchProtected, what, err)
		case !isWriteConflict(err):
			return err
		case n >= w.opts.MaxAttempts:
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

var (
	conflictMessages  = []string{"does not match", "but expected", "wasn't supplied"}
	protectedMessages = []string{"protected branch", "branch is protected", "through a pull request", "rule violation", "gh006", "gh013"}
)

func isWriteConflict(err error) bool {
	return hasAnyMessage(err, conflictMessages, http.StatusConflict, http.StatusUnprocessableEntity)
}

func isBranchProtected(err error) bool {
	return hasAnyMessage(err, protectedMessages, http.StatusForbidden, http.StatusConflict, http.StatusUnprocessableEntity)
}

func hasAnyMessage(err error, fragments []string, statuses ...int) bool {
	for _, status := range statuses {
		for _, fragment := range fragments {
			if github.HasMessage(err, status, fragment) {
				return true
			}
		}
	}
	return false
}

func backoff(attempt int) time.Duration {
	d := maxBackoff
	if shift := attempt - 1; shift < 16 {
		d = min(initialBackoff<<max(shift, 0), maxBackoff)
	}
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

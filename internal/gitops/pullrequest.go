package gitops

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/github"
)

const (
	maxRestarts      = 3
	supersededMarker = "Closed by doorstep: the pin conflicted with the base branch, so doorstep opened it again from the current base."
)

var errRestart = errors.New("pull request must be opened again")

type pullRun struct {
	w       *Writer
	repo    *github.Repo
	req     Request
	head    string
	desired string
	path    string
}

func (w *Writer) pinPullRequest(ctx context.Context, repo *github.Repo, req Request) (Result, error) {
	runCtx, cancel := context.WithTimeoutCause(ctx, w.opts.MergeTimeout, ErrMergeTimeout)
	defer cancel()
	run := &pullRun{w: w, repo: repo, req: req, head: req.headBranch(), desired: req.Target.desiredValue(req.Digest)}
	for restarts := 0; ; restarts++ {
		result, err := run.once(runCtx)
		switch {
		case err == nil:
			return result, nil
		case ctx.Err() == nil && errors.Is(context.Cause(runCtx), ErrMergeTimeout):
			return Result{}, fmt.Errorf("%w: %s not merged within %s: %w", ErrMergeTimeout, run.head, w.opts.MergeTimeout, err)
		case errors.Is(err, errRestart) && restarts < maxRestarts:
			w.logger.InfoContext(ctx, "pin pull request restarted", slog.String("app", req.Target.App), slog.String("branch", run.head))
		case errors.Is(err, errRestart):
			return Result{}, fmt.Errorf("%w: pull request from %s opened %d times", ErrConflict, run.head, restarts+1)
		default:
			return Result{}, err
		}
	}
}

func (r *pullRun) once(ctx context.Context) (Result, error) {
	t := r.req.Target
	base, current, err := r.w.load(ctx, r.repo, t, t.Branch)
	if err != nil {
		return Result{}, err
	}
	r.path = base.Path
	pulls, err := r.repo.ListPullRequests(ctx, r.head, t.Branch)
	if err != nil {
		return Result{}, err
	}
	existing, err := pickPull(pulls)
	if err != nil {
		return Result{}, err
	}
	if current == r.desired {
		return r.alreadyPinned(ctx, existing)
	}
	if existing != nil && existing.Merged {
		if !r.req.Repin {
			return Result{}, fmt.Errorf("%w: #%d was merged as %s, but %s no longer holds %s",
				ErrPinOverwritten, existing.Number, existing.MergeCommitSHA, t.Branch, r.desired)
		}
		existing = nil
	}
	previous := t.digestOf(current)
	expected, err := writeValue(editorFor(t), base.Content, r.desired)
	if err != nil {
		return Result{}, err
	}
	subject, body, err := r.w.message(r.req, previous)
	if err != nil {
		return Result{}, err
	}
	if existing == nil {
		if err := r.ensureBranch(ctx); err != nil {
			return Result{}, err
		}
	}
	if err := r.syncHead(ctx, expected, subject+"\n\n"+body); err != nil {
		return Result{}, err
	}
	if existing != nil {
		return r.poll(ctx, existing.Number)
	}
	number, err := r.open(ctx, subject, body)
	if err != nil {
		return Result{}, err
	}
	return r.poll(ctx, number)
}

func (r *pullRun) open(ctx context.Context, subject, body string) (int, error) {
	t := r.req.Target
	pr, err := r.repo.CreatePullRequest(ctx, github.NewPullRequest{Title: subject, Body: body, Head: r.head, Base: t.Branch})
	if errors.Is(err, github.ErrPullRequestExists) {
		pulls, listErr := r.repo.ListPullRequests(ctx, r.head, t.Branch)
		if listErr != nil {
			return 0, listErr
		}
		for _, p := range pulls {
			if p.State == github.PullStateOpen {
				r.w.logger.InfoContext(ctx, "pin pull request adopted", slog.String("app", t.App), slog.Int("pull_request", p.Number))
				return p.Number, nil
			}
		}
	}
	if err != nil {
		return 0, err
	}
	r.w.logger.InfoContext(ctx, "pin pull request opened",
		slog.String("app", t.App), slog.Int("pull_request", pr.Number), slog.String("url", pr.HTMLURL))
	return pr.Number, nil
}

func (r *pullRun) alreadyPinned(ctx context.Context, existing *github.PullRequest) (Result, error) {
	result, err := r.w.alreadyPinned(ctx, r.repo, r.req, r.path)
	if err != nil {
		return Result{}, err
	}
	if existing != nil {
		if existing.Merged {
			result.PullRequest = existing.Number
		} else if existing.State == github.PullStateOpen {
			if err := r.repo.ClosePullRequest(ctx, existing.Number); err != nil {
				r.w.logger.WarnContext(ctx, "close redundant pin pull request", slog.Int("pull_request", existing.Number), slog.Any("error", err))
			}
		}
	}
	r.deleteBranch(ctx)
	return result, nil
}

func (r *pullRun) ensureBranch(ctx context.Context) error {
	sha, err := r.repo.BranchSHA(ctx, r.req.Target.Branch)
	if err != nil {
		return err
	}
	err = r.repo.CreateBranch(ctx, r.head, sha)
	if errors.Is(err, github.ErrRefExists) {
		return r.repo.ResetBranch(ctx, r.head, sha)
	}
	return err
}

func (r *pullRun) syncHead(ctx context.Context, expected []byte, message string) error {
	return r.w.retryConflicts(ctx, "commit to "+r.head, func() error {
		file, err := r.repo.GetFile(ctx, r.path, r.head)
		switch {
		case github.HasStatus(err, http.StatusNotFound):
			file = github.File{}
		case err != nil:
			return err
		case bytes.Equal(file.Content, expected):
			return nil
		}
		_, err = r.repo.PutFile(ctx, github.PutFileRequest{
			Path:    r.path,
			Branch:  r.head,
			Message: message,
			Content: expected,
			SHA:     file.SHA,
		})
		return err
	})
}

func (r *pullRun) poll(ctx context.Context, number int) (Result, error) {
	for {
		pr, err := r.repo.GetPullRequest(ctx, number)
		if err != nil {
			return Result{}, err
		}
		switch {
		case pr.Merged:
			r.deleteBranch(ctx)
			return Result{}, fmt.Errorf("pull request #%d was merged by someone else: %w", number, errRestart)
		case pr.State != github.PullStateOpen:
			return Result{}, fmt.Errorf("%w: #%d %s", ErrPullRequestClosed, number, pr.HTMLURL)
		case pr.MergeableState == github.MergeableDirty:
			r.supersede(ctx, pr)
			return Result{}, fmt.Errorf("pull request #%d conflicts with %s: %w", number, r.req.Target.Branch, errRestart)
		case pr.MergeableState == github.MergeableBehind:
			if err := r.updateBranch(ctx, pr); err != nil {
				return Result{}, err
			}
		default:
			result, merged, err := r.tryMerge(ctx, pr)
			if err != nil || merged {
				return result, err
			}
		}
		if err := r.w.opts.Wait(ctx, r.w.opts.PollInterval); err != nil {
			return Result{}, err
		}
	}
}

func (r *pullRun) updateBranch(ctx context.Context, pr github.PullRequest) error {
	err := r.repo.UpdatePullRequestBranch(ctx, pr.Number, pr.HeadSHA)
	switch {
	case err == nil:
		return nil
	case github.HasMessage(err, http.StatusUnprocessableEntity, "merge conflict"):
		r.supersede(ctx, pr)
		return fmt.Errorf("pull request #%d conflicts with %s: %w", pr.Number, r.req.Target.Branch, errRestart)
	case github.HasMessage(err, http.StatusUnprocessableEntity, "expected head sha"),
		github.HasMessage(err, http.StatusUnprocessableEntity, "no new commits"),
		github.HasMessage(err, http.StatusUnprocessableEntity, "up to date"):
		r.w.logger.DebugContext(ctx, "pin pull request branch already moved", slog.Int("pull_request", pr.Number), slog.Any("error", err))
		return nil
	default:
		return err
	}
}

func (r *pullRun) tryMerge(ctx context.Context, pr github.PullRequest) (Result, bool, error) {
	t := r.req.Target
	if !strings.EqualFold(pr.HeadRepo, r.repo.FullName()) {
		return Result{}, false, fmt.Errorf("%w: #%d comes from %q, not %s", ErrPullRequestTampered, pr.Number, pr.HeadRepo, r.repo.FullName())
	}
	if pr.ChangedFiles != 1 {
		return Result{}, false, fmt.Errorf("%w: #%d changes %d files; doorstep only changes %s", ErrPullRequestTampered, pr.Number, pr.ChangedFiles, r.path)
	}
	base, current, err := r.w.loadAt(ctx, r.repo, t, r.path, t.Branch)
	if err != nil {
		return Result{}, false, err
	}
	if current == r.desired {
		return Result{}, false, fmt.Errorf("%s already holds the pin: %w", t.Branch, errRestart)
	}
	expected, err := writeValue(editorFor(t), base.Content, r.desired)
	if err != nil {
		return Result{}, false, err
	}
	previous := t.digestOf(current)
	subject, body, err := r.w.message(r.req, previous)
	if err != nil {
		return Result{}, false, err
	}
	head, err := r.repo.GetFile(ctx, r.path, pr.HeadSHA)
	if err != nil {
		return Result{}, false, err
	}
	if !bytes.Equal(head.Content, expected) {
		r.w.logger.InfoContext(ctx, "pin pull request differs from the expected edit, rewriting it",
			slog.String("app", t.App), slog.Int("pull_request", pr.Number))
		return Result{}, false, r.syncHead(ctx, expected, subject+"\n\n"+body)
	}
	sha, err := r.repo.MergePullRequest(ctx, pr.Number, github.MergeRequest{
		SHA:     pr.HeadSHA,
		Title:   subject,
		Message: body,
		Method:  github.MergeMethodSquash,
	})
	if github.HasStatus(err, http.StatusMethodNotAllowed) || github.HasStatus(err, http.StatusConflict) {
		r.w.logger.DebugContext(ctx, "pin pull request not mergeable yet", slog.Int("pull_request", pr.Number), slog.Any("error", err))
		return Result{}, false, nil
	}
	if err != nil {
		return Result{}, false, err
	}
	r.deleteBranch(ctx)
	return Result{
		Commit:      core.Commit{Repository: r.repo.FullName(), SHA: sha},
		Previous:    previous,
		Changed:     true,
		PullRequest: pr.Number,
	}, true, nil
}

func (r *pullRun) supersede(ctx context.Context, pr github.PullRequest) {
	update := github.PullRequestUpdate{State: github.PullStateClosed, Body: strings.TrimSpace(pr.Body + "\n\n" + supersededMarker)}
	if _, err := r.repo.UpdatePullRequest(ctx, pr.Number, update); err != nil {
		r.w.logger.WarnContext(ctx, "close conflicting pin pull request", slog.Int("pull_request", pr.Number), slog.Any("error", err))
	}
	r.deleteBranch(ctx)
}

func (r *pullRun) deleteBranch(ctx context.Context) {
	if err := r.repo.DeleteBranch(ctx, r.head); err != nil && !errors.Is(err, github.ErrRefNotFound) {
		r.w.logger.WarnContext(ctx, "delete pin branch", slog.String("branch", r.head), slog.Any("error", err))
	}
}

func pickPull(pulls []github.PullRequest) (*github.PullRequest, error) {
	for i := range pulls {
		if pulls[i].State == github.PullStateOpen {
			return &pulls[i], nil
		}
	}
	for i := range pulls {
		if pulls[i].Merged {
			return &pulls[i], nil
		}
	}
	for _, p := range pulls {
		if !strings.Contains(p.Body, supersededMarker) {
			return nil, fmt.Errorf("%w: #%d %s", ErrPullRequestClosed, p.Number, p.HTMLURL)
		}
	}
	return nil, nil
}

func (w *Writer) Abandon(ctx context.Context, req Request) error {
	if err := req.Validate(); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidRequest, err)
	}
	if req.Target.Mode != ModePullRequest {
		return nil
	}
	repo, err := w.repos.Repo(ctx, req.Target.Owner, req.Target.Repo)
	if err != nil {
		return err
	}
	head := req.headBranch()
	pulls, err := repo.ListPullRequests(ctx, head, req.Target.Branch)
	if err != nil {
		return err
	}
	for _, p := range pulls {
		if p.State == github.PullStateOpen {
			if err := repo.ClosePullRequest(ctx, p.Number); err != nil {
				return err
			}
		}
	}
	if err := repo.DeleteBranch(ctx, head); err != nil && !errors.Is(err, github.ErrRefNotFound) {
		return err
	}
	return nil
}

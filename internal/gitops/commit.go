package gitops

import (
	"context"

	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/github"
)

func (w *Writer) pinCommit(ctx context.Context, repo *github.Repo, req Request) (Result, error) {
	t := req.Target
	desired := t.desiredValue(req.Digest)
	var result Result
	err := w.retryConflicts(ctx, "commit to "+t.Branch, func() error {
		file, current, err := w.load(ctx, repo, t, t.Branch)
		if err != nil {
			return err
		}
		if current == desired {
			result, err = w.alreadyPinned(ctx, repo, req, file.Path)
			return err
		}
		if err := w.checkNotOverwritten(ctx, repo, req, file.Path); err != nil {
			return err
		}
		previous := t.digestOf(current)
		edited, err := writeValue(editorFor(t), file.Content, desired)
		if err != nil {
			return err
		}
		subject, body, err := w.message(req, previous)
		if err != nil {
			return err
		}
		sha, err := repo.PutFile(ctx, github.PutFileRequest{
			Path:    file.Path,
			Branch:  t.Branch,
			Message: subject + "\n\n" + body,
			Content: edited,
			SHA:     file.SHA,
		})
		if err != nil {
			return err
		}
		result = Result{Commit: core.Commit{Repository: repo.FullName(), SHA: sha}, Previous: previous, Changed: true}
		return nil
	})
	return result, err
}

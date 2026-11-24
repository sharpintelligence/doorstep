package github

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"unicode"
)

type Repo struct {
	client *Client
	owner  string
	name   string
}

func (a *App) Repo(ctx context.Context, owner, name string) (*Repo, error) {
	client, err := a.RepoClient(ctx, owner, name)
	if err != nil {
		return nil, err
	}
	return &Repo{client: client, owner: owner, name: name}, nil
}

func (r *Repo) Owner() string {
	return r.owner
}

func (r *Repo) Name() string {
	return r.name
}

func (r *Repo) FullName() string {
	return r.owner + "/" + r.name
}

func (r *Repo) endpoint(parts ...string) string {
	segments := []string{"repos", url.PathEscape(r.owner), url.PathEscape(r.name)}
	for _, part := range parts {
		for segment := range strings.SplitSeq(part, "/") {
			segments = append(segments, url.PathEscape(segment))
		}
	}
	return "/" + strings.Join(segments, "/")
}

func (r *Repo) do(ctx context.Context, method, path string, query url.Values, in, out any) error {
	if len(query) > 0 {
		path += "?" + query.Encode()
	}
	return r.client.Do(ctx, method, path, in, out)
}

func validatePath(p string) error {
	if p == "" {
		return fmt.Errorf("%w: empty path", ErrInvalidPath)
	}
	if strings.ContainsFunc(p, unicode.IsControl) {
		return fmt.Errorf("%w: %q contains a control character", ErrInvalidPath, p)
	}
	for segment := range strings.SplitSeq(p, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return fmt.Errorf("%w: %q", ErrInvalidPath, p)
		}
	}
	return nil
}

func validateRef(ref string) error {
	if err := validatePath(ref); err != nil {
		return err
	}
	if strings.ContainsFunc(ref, unicode.IsSpace) {
		return fmt.Errorf("%w: %q contains whitespace", ErrInvalidPath, ref)
	}
	return nil
}

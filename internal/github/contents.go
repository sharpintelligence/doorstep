package github

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

const MaxFileSize = 1 << 20

type File struct {
	Path    string
	SHA     string
	Content []byte
}

type PutFileRequest struct {
	Path    string
	Branch  string
	Message string
	Content []byte
	SHA     string
}

type contentResponse struct {
	Type     string `json:"type"`
	Encoding string `json:"encoding"`
	Size     int64  `json:"size"`
	Path     string `json:"path"`
	SHA      string `json:"sha"`
	Content  string `json:"content"`
}

func (r *Repo) GetFile(ctx context.Context, path, ref string) (File, error) {
	if err := validatePath(path); err != nil {
		return File{}, err
	}
	query := url.Values{}
	if ref != "" {
		if err := validateRef(ref); err != nil {
			return File{}, err
		}
		query.Set("ref", ref)
	}
	var raw json.RawMessage
	if err := r.do(ctx, http.MethodGet, r.endpoint("contents", path), query, nil, &raw); err != nil {
		return File{}, fmt.Errorf("get %s at %q in %s: %w", path, ref, r.FullName(), err)
	}
	if trimmed := bytes.TrimSpace(raw); len(trimmed) > 0 && trimmed[0] == '[' {
		return File{}, fmt.Errorf("%w: %s in %s is a directory", ErrNotAFile, path, r.FullName())
	}
	var c contentResponse
	if err := json.Unmarshal(raw, &c); err != nil {
		return File{}, fmt.Errorf("decode contents of %s in %s: %w", path, r.FullName(), err)
	}
	switch {
	case c.Type != "file":
		return File{}, fmt.Errorf("%w: %s in %s is a %s", ErrNotAFile, path, r.FullName(), c.Type)
	case c.Path != path:
		return File{}, fmt.Errorf("%w: %s in %s resolves to %s", ErrNotAFile, path, r.FullName(), c.Path)
	case c.Size > MaxFileSize:
		return File{}, fmt.Errorf("%w: %s in %s is %d bytes, the limit is %d", ErrFileTooLarge, path, r.FullName(), c.Size, MaxFileSize)
	case c.Encoding != "base64":
		return File{}, fmt.Errorf("%w: %s in %s has unsupported encoding %q", ErrNotAFile, path, r.FullName(), c.Encoding)
	case c.SHA == "":
		return File{}, fmt.Errorf("contents of %s in %s have no blob sha", path, r.FullName())
	}
	content, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(c.Content, "\n", ""))
	if err != nil {
		return File{}, fmt.Errorf("decode contents of %s in %s: %w", path, r.FullName(), err)
	}
	if int64(len(content)) != c.Size {
		return File{}, fmt.Errorf("contents of %s in %s are %d bytes, expected %d", path, r.FullName(), len(content), c.Size)
	}
	return File{Path: c.Path, SHA: c.SHA, Content: content}, nil
}

func (r *Repo) PutFile(ctx context.Context, req PutFileRequest) (string, error) {
	if err := validatePath(req.Path); err != nil {
		return "", err
	}
	if err := validateRef(req.Branch); err != nil {
		return "", err
	}
	if strings.TrimSpace(req.Message) == "" {
		return "", errors.New("put file: commit message is required")
	}
	if len(req.Content) > MaxFileSize {
		return "", fmt.Errorf("%w: new content of %s is %d bytes, the limit is %d", ErrFileTooLarge, req.Path, len(req.Content), MaxFileSize)
	}
	body := struct {
		Message string `json:"message"`
		Content string `json:"content"`
		SHA     string `json:"sha,omitempty"`
		Branch  string `json:"branch"`
	}{
		Message: req.Message,
		Content: base64.StdEncoding.EncodeToString(req.Content),
		SHA:     req.SHA,
		Branch:  req.Branch,
	}
	var out struct {
		Commit struct {
			SHA string `json:"sha"`
		} `json:"commit"`
	}
	if err := r.do(ctx, http.MethodPut, r.endpoint("contents", req.Path), nil, body, &out); err != nil {
		return "", fmt.Errorf("put %s on %s in %s: %w", req.Path, req.Branch, r.FullName(), err)
	}
	if out.Commit.SHA == "" {
		return "", fmt.Errorf("put %s on %s in %s: response has no commit sha", req.Path, req.Branch, r.FullName())
	}
	return out.Commit.SHA, nil
}

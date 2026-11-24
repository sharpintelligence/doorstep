package github

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
)

const (
	maxErrorBodyBytes    = 64 << 10
	maxErrorMessageBytes = 512
)

var (
	ErrNotInstalled          = errors.New("github app is not installed for the repository")
	ErrInstallationSuspended = errors.New("github app installation is suspended")
	ErrInvalidRepository     = errors.New("invalid repository name")
	ErrInvalidPrivateKey     = errors.New("invalid github app private key")
	ErrInvalidBaseURL        = errors.New("invalid github api url")
	ErrForeignHost           = errors.New("request host does not match the github api url")
	ErrMissingSignature      = errors.New("missing webhook signature")
	ErrInvalidSignature      = errors.New("invalid webhook signature")
	ErrInvalidCommitSHA      = errors.New("invalid commit SHA")
	ErrTooManyChecks         = errors.New("too many checks reported for the commit")
	ErrInvalidBranch         = errors.New("invalid branch name")
	ErrBranchNotFound        = errors.New("branch not found")
	ErrInvalidPath           = errors.New("invalid repository path or ref")
	ErrNotAFile              = errors.New("path is not a regular file")
	ErrFileTooLarge          = errors.New("file is too large")
	ErrRefNotFound           = errors.New("git ref not found")
	ErrRefExists             = errors.New("git ref already exists")
)

func HasStatus(err error, status int) bool {
	return hasStatus(err, status)
}

type APIError struct {
	StatusCode int
	Message    string
	RequestID  string
}

func (e *APIError) Error() string {
	msg := "github api: " + strconv.Itoa(e.StatusCode) + " " + http.StatusText(e.StatusCode)
	if e.Message != "" {
		msg += ": " + e.Message
	}
	if e.RequestID != "" {
		msg += " (request id " + e.RequestID + ")"
	}
	return msg
}

func newAPIError(resp *http.Response, body io.Reader) *APIError {
	apiErr := &APIError{
		StatusCode: resp.StatusCode,
		RequestID:  resp.Header.Get("X-GitHub-Request-Id"),
	}
	var payload struct {
		Message string `json:"message"`
	}
	if err := json.NewDecoder(io.LimitReader(body, maxErrorBodyBytes)).Decode(&payload); err == nil {
		apiErr.Message = truncate(payload.Message, maxErrorMessageBytes)
	}
	return apiErr
}

func truncate(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	cut := 0
	for i := range s {
		if i > limit {
			break
		}
		cut = i
	}
	return s[:cut] + "…"
}

func hasStatus(err error, status int) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.StatusCode == status
}

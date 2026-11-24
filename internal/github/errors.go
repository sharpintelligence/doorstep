package github

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
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
	ErrPullRequestExists     = errors.New("pull request already exists")
)

func HasStatus(err error, status int) bool {
	return hasStatus(err, status)
}

func HasMessage(err error, status int, fragment string) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.StatusCode == status &&
		strings.Contains(strings.ToLower(apiErr.Message), strings.ToLower(fragment))
}

type APIError struct {
	StatusCode         int
	Message            string
	RequestID          string
	RetryAfter         time.Duration
	RateLimitExhausted bool
	RateLimitReset     time.Time
}

func (e *APIError) RateLimited() bool {
	switch e.StatusCode {
	case http.StatusTooManyRequests:
		return true
	case http.StatusForbidden:
		return e.RateLimitExhausted || e.RetryAfter > 0 || strings.Contains(strings.ToLower(e.Message), "rate limit")
	default:
		return false
	}
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
	apiErr.RetryAfter = parseRetryAfterSeconds(resp.Header.Get("Retry-After"))
	apiErr.RateLimitExhausted = strings.TrimSpace(resp.Header.Get("X-RateLimit-Remaining")) == "0"
	apiErr.RateLimitReset = parseUnixTime(resp.Header.Get("X-RateLimit-Reset"))
	var payload struct {
		Message string `json:"message"`
		Errors  []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.NewDecoder(io.LimitReader(body, maxErrorBodyBytes)).Decode(&payload); err == nil {
		parts := []string{payload.Message}
		for _, detail := range payload.Errors {
			if detail.Message != "" {
				parts = append(parts, detail.Message)
			}
		}
		apiErr.Message = truncate(strings.TrimPrefix(strings.Join(parts, ": "), ": "), maxErrorMessageBytes)
	}
	return apiErr
}

func parseRetryAfterSeconds(value string) time.Duration {
	seconds, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || seconds <= 0 {
		return 0
	}
	return time.Duration(seconds) * time.Second
}

func parseUnixTime(value string) time.Time {
	seconds, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if err != nil || seconds <= 0 {
		return time.Time{}
	}
	return time.Unix(seconds, 0).UTC()
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

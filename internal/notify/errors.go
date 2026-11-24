package notify

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/sharpintelligence/doorstep/internal/github"
)

type StatusError struct {
	StatusCode int
	Excerpt    string
	RetryAfter time.Duration
}

func (e *StatusError) Error() string {
	msg := "unexpected response " + strconv.Itoa(e.StatusCode) + " " + http.StatusText(e.StatusCode)
	if e.Excerpt != "" {
		msg += ": " + e.Excerpt
	}
	return msg
}

type permanentError struct {
	err error
}

func (e permanentError) Error() string { return e.err.Error() }

func (e permanentError) Unwrap() error { return e.err }

func permanent(err error) error {
	if err == nil {
		return nil
	}
	return permanentError{err: err}
}

type reportedError struct {
	err error
}

func (e reportedError) Error() string { return e.err.Error() }

func (e reportedError) Unwrap() error { return e.err }

func reported(err error) error {
	if err == nil {
		return nil
	}
	return reportedError{err: err}
}

func isReported(err error) bool {
	_, ok := errors.AsType[reportedError](err)
	return ok
}

func retryable(err error) bool {
	if _, ok := errors.AsType[permanentError](err); ok {
		return false
	}
	if errors.Is(err, context.Canceled) {
		return false
	}
	if statusErr, ok := errors.AsType[*StatusError](err); ok {
		return retryableStatus(statusErr.StatusCode)
	}
	if apiErr, ok := errors.AsType[*github.APIError](err); ok {
		return retryableStatus(apiErr.StatusCode)
	}
	return true
}

func retryableStatus(code int) bool {
	return code == http.StatusRequestTimeout || code == http.StatusTooManyRequests || code >= http.StatusInternalServerError
}

func retryAfter(err error) (time.Duration, bool) {
	statusErr, ok := errors.AsType[*StatusError](err)
	if !ok || statusErr.RetryAfter <= 0 {
		return 0, false
	}
	return statusErr.RetryAfter, true
}

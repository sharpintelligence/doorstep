package github

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
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
)

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
	if err := json.NewDecoder(body).Decode(&payload); err == nil {
		apiErr.Message = payload.Message
	}
	return apiErr
}

func hasStatus(err error, status int) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.StatusCode == status
}

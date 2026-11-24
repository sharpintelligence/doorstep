package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strings"
)

const (
	MaxRequestBytes = 64 << 10

	codeInvalidRequest       = "invalid_request"
	codeUnauthorized         = "unauthorized"
	codeNotFound             = "not_found"
	codeMethodNotAllowed     = "method_not_allowed"
	codeLocked               = "locked"
	codeUnsupportedMediaType = "unsupported_media_type"
	codeInternal             = "internal"
)

type errorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type errorEnvelope struct {
	Error errorBody `json:"error"`
}

type requestError struct {
	status  int
	code    string
	message string
}

func (e *requestError) Error() string {
	return e.message
}

func badRequest(format string, args ...any) *requestError {
	return &requestError{status: http.StatusBadRequest, code: codeInvalidRequest, message: fmt.Sprintf(format, args...)}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, errorEnvelope{Error: errorBody{Code: code, Message: message}})
}

func (a *api) internalError(w http.ResponseWriter, r *http.Request, err error) {
	a.logger.ErrorContext(r.Context(), "api request failed",
		slog.String("method", r.Method),
		slog.String("path", r.URL.Path),
		slog.String("error", err.Error()),
	)
	writeError(w, http.StatusInternalServerError, codeInternal, "internal error")
}

func methodNotAllowed(allowed ...string) http.HandlerFunc {
	allow := strings.Join(allowed, ", ")
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Allow", allow)
		writeError(w, http.StatusMethodNotAllowed, codeMethodNotAllowed, "method "+r.Method+" is not allowed; use "+allow)
	}
}

func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) *requestError {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return &requestError{status: http.StatusUnsupportedMediaType, code: codeUnsupportedMediaType, message: "Content-Type must be application/json"}
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, MaxRequestBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return decodeError(err)
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if _, tooLarge := errors.AsType[*http.MaxBytesError](err); tooLarge {
			return decodeError(err)
		}
		return badRequest("request body must contain a single JSON object")
	}
	return nil
}

func decodeError(err error) *requestError {
	if maxErr, ok := errors.AsType[*http.MaxBytesError](err); ok {
		return &requestError{status: http.StatusRequestEntityTooLarge, code: codeInvalidRequest, message: fmt.Sprintf("request body must be at most %d bytes", maxErr.Limit)}
	}
	if typeErr, ok := errors.AsType[*json.UnmarshalTypeError](err); ok {
		return badRequest("field %q has the wrong type", typeErr.Field)
	}
	if errors.Is(err, io.EOF) {
		return badRequest("request body must not be empty")
	}
	return badRequest("malformed JSON request body: %v", err)
}

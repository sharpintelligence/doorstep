package github

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestTruncate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		in    string
		limit int
		want  string
	}{
		{name: "empty", in: "", limit: 4, want: ""},
		{name: "shorter", in: "abc", limit: 4, want: "abc"},
		{name: "exact", in: "abcd", limit: 4, want: "abcd"},
		{name: "longer", in: "abcdef", limit: 4, want: "abcd…"},
		{name: "keeps whole runes", in: "abcéf", limit: 4, want: "abc…"},
		{name: "rune ends at limit", in: "abécd", limit: 4, want: "abé…"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := truncate(tt.in, tt.limit); got != tt.want {
				t.Errorf("truncate(%q, %d) = %q, want %q", tt.in, tt.limit, got, tt.want)
			}
		})
	}
}

func TestNewAPIErrorBoundsMessage(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		messageSize int
		wantMessage bool
	}{
		{name: "small message kept", messageSize: 100, wantMessage: true},
		{name: "long message truncated", messageSize: 4 << 10, wantMessage: true},
		{name: "oversized body dropped", messageSize: maxErrorBodyBytes, wantMessage: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			body, err := json.Marshal(map[string]string{"message": strings.Repeat("é", tt.messageSize/2)})
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			resp := &http.Response{StatusCode: http.StatusBadRequest, Header: http.Header{}}
			apiErr := newAPIError(resp, strings.NewReader(string(body)))
			if got := apiErr.Message != ""; got != tt.wantMessage {
				t.Fatalf("message present = %t, want %t", got, tt.wantMessage)
			}
			if len(apiErr.Message) > maxErrorMessageBytes+len("…") || !utf8.ValidString(apiErr.Message) {
				t.Errorf("message of %d bytes is not bounded valid UTF-8", len(apiErr.Message))
			}
			if len(apiErr.Error()) > maxErrorMessageBytes+100 {
				t.Errorf("error string is %d bytes", len(apiErr.Error()))
			}
		})
	}
}

func TestNewAPIErrorIncludesDetails(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		body string
		want string
	}{
		{name: "message only", body: `{"message":"Not Found"}`, want: "Not Found"},
		{
			name: "validation details",
			body: `{"message":"Validation Failed","errors":[{"resource":"PullRequest","code":"custom","message":"A pull request already exists for acme:doorstep/blog-1."}]}`,
			want: "Validation Failed: A pull request already exists for acme:doorstep/blog-1.",
		},
		{name: "details without message text", body: `{"message":"Validation Failed","errors":[{"code":"missing"}]}`, want: "Validation Failed"},
		{name: "details only", body: `{"errors":[{"message":"bad"}]}`, want: "bad"},
		{name: "not json", body: `<html>`, want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			resp := &http.Response{StatusCode: http.StatusUnprocessableEntity, Header: http.Header{}}
			if got := newAPIError(resp, strings.NewReader(tt.body)).Message; got != tt.want {
				t.Fatalf("Message = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestHasMessage(t *testing.T) {
	t.Parallel()

	err := fmt.Errorf("wrapped: %w", &APIError{StatusCode: http.StatusConflict, Message: "Repository Rule Violations found"})
	tests := []struct {
		name     string
		err      error
		status   int
		fragment string
		want     bool
	}{
		{name: "matches ignoring case", err: err, status: http.StatusConflict, fragment: "rule violations", want: true},
		{name: "other status", err: err, status: http.StatusUnprocessableEntity, fragment: "rule violations"},
		{name: "other text", err: err, status: http.StatusConflict, fragment: "does not match"},
		{name: "not an api error", err: errors.New("rule violations"), status: http.StatusConflict, fragment: "rule violations"},
		{name: "nil", status: http.StatusConflict, fragment: "x"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := HasMessage(tt.err, tt.status, tt.fragment); got != tt.want {
				t.Fatalf("HasMessage() = %t, want %t", got, tt.want)
			}
		})
	}
}

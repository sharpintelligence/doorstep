package github

import (
	"encoding/json"
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

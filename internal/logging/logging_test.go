package logging

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

func TestParseLevel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in      string
		want    slog.Level
		wantErr error
	}{
		{in: "debug", want: slog.LevelDebug},
		{in: "info", want: slog.LevelInfo},
		{in: "warn", want: slog.LevelWarn},
		{in: "error", want: slog.LevelError},
		{in: "DEBUG", want: slog.LevelDebug},
		{in: "Warn", want: slog.LevelWarn},
		{in: "", wantErr: ErrUnknownLevel},
		{in: "warning", wantErr: ErrUnknownLevel},
		{in: "trace", wantErr: ErrUnknownLevel},
	}

	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			t.Parallel()
			got, err := ParseLevel(tt.in)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("ParseLevel(%q) error = %v, want %v", tt.in, err, tt.wantErr)
			}
			if err == nil && got != tt.want {
				t.Errorf("ParseLevel(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestParseFormat(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in      string
		want    string
		wantErr error
	}{
		{in: "json", want: FormatJSON},
		{in: "text", want: FormatText},
		{in: "JSON", want: FormatJSON},
		{in: "", wantErr: ErrUnknownFormat},
		{in: "logfmt", wantErr: ErrUnknownFormat},
	}

	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			t.Parallel()
			got, err := ParseFormat(tt.in)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("ParseFormat(%q) error = %v, want %v", tt.in, err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("ParseFormat(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestNewRejectsUnknownValues(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		level   string
		format  string
		wantErr error
	}{
		{name: "bad level", level: "loud", format: "json", wantErr: ErrUnknownLevel},
		{name: "bad format", level: "info", format: "xml", wantErr: ErrUnknownFormat},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			logger, err := New(&bytes.Buffer{}, tt.level, tt.format)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("New error = %v, want %v", err, tt.wantErr)
			}
			if logger != nil {
				t.Error("New returned a logger alongside an error")
			}
		})
	}
}

func TestNewLevelFiltering(t *testing.T) {
	t.Parallel()

	tests := []struct {
		level string
		want  []string
	}{
		{level: "debug", want: []string{"debug", "info", "warn", "error"}},
		{level: "info", want: []string{"info", "warn", "error"}},
		{level: "warn", want: []string{"warn", "error"}},
		{level: "error", want: []string{"error"}},
	}

	for _, tt := range tests {
		t.Run(tt.level, func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			logger, err := New(&buf, tt.level, FormatJSON)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			logger.Debug("debug")
			logger.Info("info")
			logger.Warn("warn")
			logger.Error("error")

			var got []string
			for line := range strings.Lines(buf.String()) {
				var entry struct {
					Msg string `json:"msg"`
				}
				if err := json.Unmarshal([]byte(line), &entry); err != nil {
					t.Fatalf("Unmarshal %q: %v", line, err)
				}
				got = append(got, entry.Msg)
			}
			if strings.Join(got, ",") != strings.Join(tt.want, ",") {
				t.Errorf("logged %v, want %v", got, tt.want)
			}
		})
	}
}

func TestNewOutputShape(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		format string
		check  func(t *testing.T, out string)
	}{
		{
			name:   "json",
			format: FormatJSON,
			check: func(t *testing.T, out string) {
				t.Helper()
				var entry map[string]any
				if err := json.Unmarshal([]byte(out), &entry); err != nil {
					t.Fatalf("Unmarshal %q: %v", out, err)
				}
				want := map[string]any{"level": "INFO", "msg": "hello", "app": "web", "attempt": float64(2)}
				for key, value := range want {
					if entry[key] != value {
						t.Errorf("entry[%q] = %v, want %v", key, entry[key], value)
					}
				}
				if _, ok := entry["time"]; !ok {
					t.Error("entry has no time field")
				}
			},
		},
		{
			name:   "text",
			format: FormatText,
			check: func(t *testing.T, out string) {
				t.Helper()
				for _, want := range []string{"level=INFO", "msg=hello", "app=web", "attempt=2"} {
					if !strings.Contains(out, want) {
						t.Errorf("output %q does not contain %q", out, want)
					}
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			logger, err := New(&buf, "info", tt.format)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			logger.Info("hello", slog.String("app", "web"), slog.Int("attempt", 2))
			tt.check(t, buf.String())
		})
	}
}

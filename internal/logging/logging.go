package logging

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
)

const (
	FormatJSON = "json"
	FormatText = "text"
)

var (
	ErrUnknownLevel  = errors.New("unknown log level")
	ErrUnknownFormat = errors.New("unknown log format")
)

func New(w io.Writer, level, format string) (*slog.Logger, error) {
	lvl, err := ParseLevel(level)
	if err != nil {
		return nil, err
	}
	normalized, err := ParseFormat(format)
	if err != nil {
		return nil, err
	}
	opts := &slog.HandlerOptions{Level: lvl}
	if normalized == FormatText {
		return slog.New(slog.NewTextHandler(w, opts)), nil
	}
	return slog.New(slog.NewJSONHandler(w, opts)), nil
}

func ParseLevel(level string) (slog.Level, error) {
	switch strings.ToLower(level) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("%w %q (want debug, info, warn or error)", ErrUnknownLevel, level)
	}
}

func ParseFormat(format string) (string, error) {
	switch normalized := strings.ToLower(format); normalized {
	case FormatJSON, FormatText:
		return normalized, nil
	default:
		return "", fmt.Errorf("%w %q (want json or text)", ErrUnknownFormat, format)
	}
}

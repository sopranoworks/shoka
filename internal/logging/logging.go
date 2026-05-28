// Package logging builds the server's structured logger from configuration.
// Logs are written to the provided writer (stderr in production). A logging
// failure (e.g. a closed sink) is swallowed by slog and never affects callers.
package logging

import (
	"fmt"
	"io"
	"log/slog"
	"strings"
)

// New returns a *slog.Logger writing to w at the given level/format. Empty
// level means "info"; empty format means "text". Invalid values return an error.
func New(level, format string, w io.Writer) (*slog.Logger, error) {
	var lvl slog.Level
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "", "info":
		lvl = slog.LevelInfo
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		return nil, fmt.Errorf("invalid log level %q (want error|warn|info|debug)", level)
	}

	opts := &slog.HandlerOptions{Level: lvl}
	var h slog.Handler
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "", "text":
		h = slog.NewTextHandler(w, opts)
	case "json":
		h = slog.NewJSONHandler(w, opts)
	default:
		return nil, fmt.Errorf("invalid log format %q (want text|json)", format)
	}
	return slog.New(h), nil
}

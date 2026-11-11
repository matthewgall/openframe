// Package logging centralises structured logging setup (log/slog).
//
// Every component derives a child logger via NewComponent, which adds a
// stable "component" attribute so log lines from the frame agent, server
// HTTP layer and update coordinator are easy to tell apart.
//
// OpenFrame never logs secrets. Callers are responsible for keeping
// credentials, tokens and signed URLs out of log attributes (see
// docs/security/threat-model.md, T-13).
package logging

import (
	"fmt"
	"log/slog"
	"os"
	"strings"
)

// Format is the log output encoding.
type Format string

const (
	FormatJSON Format = "json"
	FormatText Format = "text"
)

// Options configures the root logger.
type Options struct {
	Level  string // "debug", "info", "warn", "error" (case-insensitive)
	Format string // "json" (default) or "text"
	Output string // "stderr" (default) or "stdout"
}

// New builds the root logger.
func New(opts Options) (*slog.Logger, error) {
	var level slog.Level
	switch strings.ToLower(strings.TrimSpace(opts.Level)) {
	case "", "info":
		level = slog.LevelInfo
	case "debug":
		level = slog.LevelDebug
	case "warn", "warning":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		return nil, fmt.Errorf("invalid log level %q (want debug, info, warn or error)", opts.Level)
	}

	var out *os.File
	switch strings.ToLower(strings.TrimSpace(opts.Output)) {
	case "", "stderr":
		out = os.Stderr
	case "stdout":
		out = os.Stdout
	default:
		return nil, fmt.Errorf("invalid log output %q (want stderr or stdout)", opts.Output)
	}

	var handler slog.Handler
	opts2 := slog.HandlerOptions{Level: level}
	switch Format(opts.Format) {
	case "", FormatJSON:
		handler = slog.NewJSONHandler(out, &opts2)
	case FormatText:
		opts2.ReplaceAttr = func(_ []string, a slog.Attr) slog.Attr {
			if a.Key == slog.TimeKey {
				a.Value = slog.StringValue(a.Value.Time().Format("2006-01-02 15:04:05.000"))
			}
			return a
		}
		handler = slog.NewTextHandler(out, &opts2)
	default:
		return nil, fmt.Errorf("invalid log format %q (want json or text)", opts.Format)
	}

	return slog.New(handler), nil
}

// NewComponent returns a child logger tagged with the given component name
// (e.g. "server.http", "frame.agent", "frame.display").
func NewComponent(log *slog.Logger, component string) *slog.Logger {
	return log.With("component", component)
}

package vllog

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
)

// logMode selects where a logger writes its records.
type logMode int

const (
	modeOff     logMode = iota // discard everything
	modeFile                   // rotating log file only
	modeConsole                // stdout, with WARN/ERROR routed to stderr
	modeBoth                   // file + console
)

// parseMode converts a config string into a logMode.
func parseMode(s string) (logMode, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "off":
		return modeOff, nil
	case "file":
		return modeFile, nil
	case "console":
		return modeConsole, nil
	case "both":
		return modeBoth, nil
	default:
		return 0, fmt.Errorf("invalid log mode: %s", s)
	}
}

// consoleHandler routes records to stdout or stderr by level: WARN and above
// go to stderr, everything else to stdout.
type consoleHandler struct {
	out slog.Handler
	err slog.Handler
}

func newConsoleHandler(opts *slog.HandlerOptions) *consoleHandler {
	return &consoleHandler{
		out: slog.NewTextHandler(os.Stdout, opts),
		err: slog.NewTextHandler(os.Stderr, opts),
	}
}

func (h *consoleHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.out.Enabled(ctx, level)
}

func (h *consoleHandler) Handle(ctx context.Context, r slog.Record) error {
	if r.Level >= slog.LevelWarn {
		return h.err.Handle(ctx, r)
	}
	return h.out.Handle(ctx, r)
}

func (h *consoleHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &consoleHandler{out: h.out.WithAttrs(attrs), err: h.err.WithAttrs(attrs)}
}

func (h *consoleHandler) WithGroup(name string) slog.Handler {
	return &consoleHandler{out: h.out.WithGroup(name), err: h.err.WithGroup(name)}
}

// fanoutHandler dispatches each record to every wrapped handler. Used for the
// "both" mode (file + console).
type fanoutHandler struct {
	handlers []slog.Handler
}

func (h fanoutHandler) Enabled(ctx context.Context, level slog.Level) bool {
	for _, hh := range h.handlers {
		if hh.Enabled(ctx, level) {
			return true
		}
	}
	return false
}

func (h fanoutHandler) Handle(ctx context.Context, r slog.Record) error {
	var errs []error
	for _, hh := range h.handlers {
		if hh.Enabled(ctx, r.Level) {
			// Clone so a handler that retains the record can't observe
			// mutations made by another.
			if err := hh.Handle(ctx, r.Clone()); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

func (h fanoutHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	next := make([]slog.Handler, len(h.handlers))
	for i, hh := range h.handlers {
		next[i] = hh.WithAttrs(attrs)
	}
	return fanoutHandler{handlers: next}
}

func (h fanoutHandler) WithGroup(name string) slog.Handler {
	next := make([]slog.Handler, len(h.handlers))
	for i, hh := range h.handlers {
		next[i] = hh.WithGroup(name)
	}
	return fanoutHandler{handlers: next}
}

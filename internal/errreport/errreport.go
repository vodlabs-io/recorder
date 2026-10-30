// Package errreport sends error reports and bug reports to VodLabs' Sentry
// project.
//
// Error reports (error-level log records, panics and crashes) are sent only
// while the user has turned them on. Bug reports are sent when the user sends
// one, whether or not error reports are on. Every event passes through scrub
// before it leaves the process: emails, tokens, URL query strings and the
// user's home folder are removed (see https://vodlabs.io/privacy#error-reports).
//
// Only release builds report, unless VLREC_SENTRY_DSN names another project.
package errreport

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/getsentry/sentry-go"
	"github.com/vodlabs-io/recorder/internal/version"
)

// dsn is the recorder project's public client key. It only lets a client send
// events, and Sentry caps how many it accepts a day.
const dsn = "https://748fd4caebaf0ca976ef37c2c4b4ddc9@o4512343562780672.ingest.us.sentry.io/4512344170758144"

// DSNEnv overrides dsn, for testing against another project. "off" disables
// reporting.
const DSNEnv = "VLREC_SENTRY_DSN"

// minInterval caps how often one logger.Error call site is reported, so a
// failing loop can't use up the day's quota. Skipped records are counted on
// the next report from that site.
const minInterval = time.Hour

// flushTimeout bounds how long sending waits at exit and for a bug report.
const flushTimeout = 5 * time.Second

// tagReport marks an event the user sent on purpose, so it passes the
// enabled check.
const tagReport = "report"

var (
	ready   bool        // a client exists
	enabled atomic.Bool // error reports are on

	transport sentry.Transport // replaced in tests
)

// ErrUnavailable is returned by SendBugReport when this build cannot report.
var ErrUnavailable = errors.New("bug reports are not available in this build")

// Options configures Init.
type Options struct {
	// Component names the program: "daemon" or "cli".
	Component string
	// Launcher is what started the daemon ("desktop", "vod"), when known.
	Launcher string
	// Enabled is the user's answer; nil (not asked yet) counts as off.
	Enabled *bool
}

// Init creates the Sentry client. The returned func flushes queued events and
// must run before the process exits.
func Init(o Options) (flush func()) {
	key, env := clientDSN()
	if key == "" {
		return func() {}
	}
	home, _ := os.UserHomeDir()
	setHome(home)
	err := sentry.Init(sentry.ClientOptions{
		Dsn:            key,
		Environment:    env,
		Release:        "recorder@" + version.Version,
		SendDefaultPII: false,
		MaxBreadcrumbs: 50,
		Transport:      transport,
		BeforeSend: func(ev *sentry.Event, _ *sentry.EventHint) *sentry.Event {
			if ev.Tags[tagReport] == "" && !enabled.Load() {
				return nil
			}
			scrub(ev)
			return ev
		},
		BeforeBreadcrumb: func(b *sentry.Breadcrumb, _ *sentry.BreadcrumbHint) *sentry.Breadcrumb {
			b.Message = scrubString(b.Message)
			b.Data = scrubMap(b.Data)
			return b
		},
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "errreport: sentry init: %v\n", err)
		return func() {}
	}
	sentry.ConfigureScope(func(s *sentry.Scope) {
		s.SetTag("component", o.Component)
		s.SetTag("os", runtime.GOOS)
		s.SetTag("arch", runtime.GOARCH)
		if o.Launcher != "" {
			s.SetTag("launcher", o.Launcher)
		}
		if version.Commit != "" {
			s.SetTag("commit", version.Commit)
		}
		// Sentry fills in the address the report came from.
		s.SetUser(sentry.User{IPAddress: "{{auto}}"})
	})
	ready = true
	SetEnabled(o.Enabled != nil && *o.Enabled)
	return func() { sentry.Flush(flushTimeout) }
}

// clientDSN picks the key to send with, and the environment events are filed
// under. A development build reports only to an explicit override.
func clientDSN() (key, env string) {
	override, set := os.LookupEnv(DSNEnv)
	override = strings.TrimSpace(override)
	env = "production"
	if version.Version == "devel" {
		env = "development"
	}
	switch {
	case set && (override == "" || override == "off"):
		return "", env
	case set:
		return override, env
	case env == "development":
		return "", env
	}
	return dsn, env
}

// Available reports whether this build can send reports at all. A
// development build can't, unless VLREC_SENTRY_DSN is set.
func Available() bool { return ready }

// SetEnabled turns error reports on or off for the rest of the run.
func SetEnabled(on bool) { enabled.Store(on) }

// Enabled reports whether error reports are on.
func Enabled() bool { return ready && enabled.Load() }

// Handler returns h, also sending its error-level records to Sentry, and
// its info and warning records as breadcrumbs, while error reports are on.
func Handler(h slog.Handler) slog.Handler {
	return slog.NewMultiHandler(h, &sentryHandler{limits: &limiter{last: map[uintptr]siteState{}}})
}

// Recover reports a panic on the calling goroutine and then re-panics, so the
// process still crashes as before. Use it as `defer errreport.Recover()`.
func Recover() {
	if !Enabled() {
		return
	}
	if r := recover(); r != nil {
		sentry.CurrentHub().Recover(r)
		sentry.Flush(flushTimeout)
		panic(r)
	}
}

type siteState struct {
	at      time.Time
	skipped int
}

type limiter struct {
	mu   sync.Mutex
	last map[uintptr]siteState
}

// allow reports whether the call site pc may send now, and how many of its
// records were skipped since it last did.
func (l *limiter) allow(pc uintptr, now time.Time) (bool, int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	s := l.last[pc]
	if !s.at.IsZero() && now.Sub(s.at) < minInterval {
		s.skipped++
		l.last[pc] = s
		return false, 0
	}
	l.last[pc] = siteState{at: now}
	return true, s.skipped
}

type sentryHandler struct {
	attrs  []slog.Attr
	group  string
	limits *limiter
}

func (h *sentryHandler) Enabled(_ context.Context, l slog.Level) bool {
	return l >= slog.LevelInfo && Enabled()
}

func (h *sentryHandler) WithAttrs(as []slog.Attr) slog.Handler {
	c := *h
	c.attrs = append(append([]slog.Attr{}, h.attrs...), prefixed(h.group, as)...)
	return &c
}

func (h *sentryHandler) WithGroup(name string) slog.Handler {
	c := *h
	if c.group != "" {
		name = c.group + "." + name
	}
	c.group = name
	return &c
}

func (h *sentryHandler) Handle(_ context.Context, r slog.Record) error {
	if r.Level < slog.LevelError {
		data := map[string]any{}
		h.collect(r, data)
		sentry.AddBreadcrumb(&sentry.Breadcrumb{
			Type:      "default",
			Category:  "log",
			Level:     breadcrumbLevel(r.Level),
			Message:   r.Message,
			Data:      data,
			Timestamp: r.Time,
		})
		return nil
	}
	ok, skipped := h.limits.allow(r.PC, r.Time)
	if !ok {
		return nil
	}
	extra := map[string]any{}
	errValue := h.collect(r, extra)
	if skipped > 0 {
		extra["skipped_since_last_report"] = skipped
	}

	ev := sentry.NewEvent()
	ev.Level = sentry.LevelError
	ev.Timestamp = r.Time
	ev.Contexts["log"] = extra
	if svc, ok := extra["service"].(string); ok {
		ev.Tags["service"] = svc
	}
	// The log message is the issue title; the call site's stack groups it.
	ev.Exception = []sentry.Exception{{
		Type:       r.Message,
		Value:      errValue,
		Stacktrace: callerStack(),
	}}
	sentry.CurrentHub().CaptureEvent(ev)
	return nil
}

// collect adds the record's attributes to into, and returns the text of the
// first error among them.
func (h *sentryHandler) collect(r slog.Record, into map[string]any) (errValue string) {
	add := func(a slog.Attr) {
		val := a.Value.Resolve()
		v := val.Any()
		if e, isErr := v.(error); isErr {
			if errValue == "" {
				errValue = e.Error()
			}
			v = e.Error()
		} else if val.Kind() == slog.KindAny {
			// Formatted, so scrub sees structs (URLs, requests) as text.
			v = val.String()
		}
		into[a.Key] = v
	}
	for _, a := range h.attrs {
		add(a)
	}
	r.Attrs(func(a slog.Attr) bool {
		for _, p := range prefixed(h.group, []slog.Attr{a}) {
			add(p)
		}
		return true
	})
	return errValue
}

func breadcrumbLevel(l slog.Level) sentry.Level {
	if l >= slog.LevelWarn {
		return sentry.LevelWarning
	}
	return sentry.LevelInfo
}

// prefixed flattens groups into dotted keys.
func prefixed(group string, as []slog.Attr) []slog.Attr {
	var out []slog.Attr
	for _, a := range as {
		key := a.Key
		if group != "" {
			key = group + "." + key
		}
		if a.Value.Kind() == slog.KindGroup {
			out = append(out, prefixed(key, a.Value.Group())...)
			continue
		}
		out = append(out, slog.Attr{Key: key, Value: a.Value})
	}
	return out
}

// callerStack is the current stack without this package's, vllog's and
// slog's frames.
func callerStack() *sentry.Stacktrace {
	st := sentry.NewStacktrace()
	if st == nil {
		return nil
	}
	frames := st.Frames[:0]
	for _, f := range st.Frames {
		if strings.HasSuffix(f.Module, "/errreport") || strings.HasSuffix(f.Module, "/vllog") || f.Module == "log/slog" {
			continue
		}
		frames = append(frames, f)
	}
	st.Frames = frames
	return st
}

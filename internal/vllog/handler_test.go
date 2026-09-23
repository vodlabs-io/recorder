package vllog

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

func TestParseMode(t *testing.T) {
	valid := map[string]logMode{
		"off":     modeOff,
		"file":    modeFile,
		"console": modeConsole,
		"both":    modeBoth,
		"  Both ": modeBoth,    // trimmed + case-insensitive
		"CONSOLE": modeConsole, // case-insensitive
	}
	for in, want := range valid {
		got, err := parseMode(in)
		if err != nil {
			t.Errorf("parseMode(%q) returned error: %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("parseMode(%q) = %v, want %v", in, got, want)
		}
	}

	for _, in := range []string{"", "stdout", "stderr", "none", "bogus"} {
		if _, err := parseMode(in); err == nil {
			t.Errorf("parseMode(%q) expected error, got nil", in)
		}
	}
}

func TestConsoleHandlerRoutesByLevel(t *testing.T) {
	var out, errBuf bytes.Buffer
	opts := &slog.HandlerOptions{Level: slog.LevelDebug}
	h := &consoleHandler{
		out: slog.NewTextHandler(&out, opts),
		err: slog.NewTextHandler(&errBuf, opts),
	}
	log := slog.New(h)

	log.Debug("dbg")
	log.Info("inf")
	log.Warn("wrn")
	log.Error("err")

	outStr, errStr := out.String(), errBuf.String()

	// Debug/Info -> stdout only.
	for _, msg := range []string{"dbg", "inf"} {
		if !strings.Contains(outStr, msg) {
			t.Errorf("stdout missing %q; got %q", msg, outStr)
		}
		if strings.Contains(errStr, msg) {
			t.Errorf("stderr unexpectedly contains %q; got %q", msg, errStr)
		}
	}

	// Warn/Error -> stderr only.
	for _, msg := range []string{"wrn", "err"} {
		if !strings.Contains(errStr, msg) {
			t.Errorf("stderr missing %q; got %q", msg, errStr)
		}
		if strings.Contains(outStr, msg) {
			t.Errorf("stdout unexpectedly contains %q; got %q", msg, outStr)
		}
	}
}

func TestFanoutHandlerWritesToAllWithAttrs(t *testing.T) {
	var a, b bytes.Buffer
	opts := &slog.HandlerOptions{Level: slog.LevelInfo}
	h := fanoutHandler{handlers: []slog.Handler{
		slog.NewTextHandler(&a, opts),
		slog.NewTextHandler(&b, opts),
	}}
	// The service attr must propagate through WithAttrs to every child.
	log := slog.New(h).With("service", "test")
	log.Info("hello")

	for name, buf := range map[string]*bytes.Buffer{"a": &a, "b": &b} {
		s := buf.String()
		if !strings.Contains(s, "hello") {
			t.Errorf("handler %s missing msg; got %q", name, s)
		}
		if !strings.Contains(s, "service=test") {
			t.Errorf("handler %s missing service attr; got %q", name, s)
		}
	}
}

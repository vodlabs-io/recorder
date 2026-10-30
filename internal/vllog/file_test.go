package vllog

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vodlabs-io/recorder/internal/vlconfig"
)

func TestNewFileLoggerOffDiscards(t *testing.T) {
	lg, err := NewFileLogger(vlconfig.LoggerConfig{Mode: "off", Level: "info"}, "x")
	if err != nil {
		t.Fatalf("NewFileLogger: %v", err)
	}
	if lg.rotator != nil {
		t.Error("off mode should not create a rotator")
	}
	lg.Info("nope") // must not panic or write anywhere
	if err := lg.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}

func TestNewFileLoggerFileWritesServiceAttr(t *testing.T) {
	dir := t.TempDir()
	lg, err := NewFileLogger(vlconfig.LoggerConfig{
		Mode:      "file",
		Level:     "info",
		Directory: dir,
		FileName:  "svc.log",
	}, "myservice")
	if err != nil {
		t.Fatalf("NewFileLogger: %v", err)
	}
	lg.Info("hello", "k", "v")
	if err := lg.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "svc.log"))
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	s := string(data)
	if !strings.Contains(s, "service=myservice") {
		t.Errorf("missing service attr; got %q", s)
	}
	if !strings.Contains(s, "hello") {
		t.Errorf("missing msg; got %q", s)
	}
}

func TestNewFileLoggerInvalidMode(t *testing.T) {
	if _, err := NewFileLogger(vlconfig.LoggerConfig{Mode: "bogus", Level: "info"}, "x"); err == nil {
		t.Error("expected error for invalid mode, got nil")
	}
}

func TestWrapHandlersSeesEveryLevel(t *testing.T) {
	var seen []slog.Level
	WrapHandlers(func(h slog.Handler) slog.Handler {
		return slog.NewMultiHandler(h, levelRecorder{&seen})
	})
	t.Cleanup(func() { WrapHandlers(nil) })

	// Logging off and a level above error still reach the wrapper.
	l, err := NewFileLogger(vlconfig.LoggerConfig{Level: "error", Mode: "off"}, "test")
	if err != nil {
		t.Fatal(err)
	}
	l.Warn("w")
	l.Error("e")
	if len(seen) != 2 || seen[0] != slog.LevelWarn || seen[1] != slog.LevelError {
		t.Fatalf("wrapper saw %v, want [WARN ERROR]", seen)
	}
}

type levelRecorder struct{ seen *[]slog.Level }

func (r levelRecorder) Enabled(context.Context, slog.Level) bool { return true }
func (r levelRecorder) Handle(_ context.Context, rec slog.Record) error {
	*r.seen = append(*r.seen, rec.Level)
	return nil
}
func (r levelRecorder) WithAttrs([]slog.Attr) slog.Handler { return r }
func (r levelRecorder) WithGroup(string) slog.Handler      { return r }

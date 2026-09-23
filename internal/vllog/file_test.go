package vllog

import (
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

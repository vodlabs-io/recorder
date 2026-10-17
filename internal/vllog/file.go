package vllog

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/DeRuina/timberjack"
	"github.com/vodlabs-io/recorder/internal/vlconfig"
)

type FileLogger struct {
	logger  *slog.Logger
	rotator *timberjack.Logger
}

// NewFileLogger creates and initializes a new logger for the named service.
// The output destination is selected by cfg.Mode (off | file | console | both).
func NewFileLogger(cfg vlconfig.LoggerConfig, service string) (*FileLogger, error) {
	fl := &FileLogger{}
	if err := fl.init(cfg, service); err != nil {
		return nil, err
	}
	return fl, nil
}

// init builds the slog handler chain from cfg.Mode and stamps every record with
// service=<service>. Both file and console output use the logfmt format.
func (f *FileLogger) init(cfg vlconfig.LoggerConfig, service string) error {
	mode, err := parseMode(cfg.Mode)
	if err != nil {
		return err
	}

	level, err := parseLevel(cfg.Level)
	if err != nil {
		return err
	}

	opts := &slog.HandlerOptions{
		Level:     level,
		AddSource: false, // Disable source file and line number
	}

	var handlers []slog.Handler

	if mode == modeFile || mode == modeBoth {
		if cfg.Directory == "" {
			cfg.Directory = directory()
		}
		if err := os.MkdirAll(cfg.Directory, 0755); err != nil {
			return err
		}
		f.rotator = &timberjack.Logger{
			Filename:   filepath.Join(cfg.Directory, cfg.FileName),
			MaxSize:    5,    // Max size in MB before rotation
			MaxBackups: 20,   // Keep 20 rotated log files
			LocalTime:  true, // Use local time for timestamps in backup filenames
		}
		handlers = append(handlers, slog.NewTextHandler(f.rotator, opts))
	}

	if mode == modeConsole || mode == modeBoth {
		handlers = append(handlers, newConsoleHandler(opts))
	}

	var handler slog.Handler
	switch len(handlers) {
	case 0:
		handler = slog.DiscardHandler // mode == off
	case 1:
		handler = handlers[0]
	default:
		handler = fanoutHandler{handlers: handlers}
	}

	f.logger = slog.New(handler).With("service", service)

	return nil
}

// FilePath returns the resolved log file path for cfg, applying the same
// directory fallback as NewFileLogger when cfg.Directory is empty.
func FilePath(cfg vlconfig.LoggerConfig) string {
	if cfg.Directory == "" {
		cfg.Directory = directory()
	}
	return filepath.Join(cfg.Directory, cfg.FileName)
}

// directory is the logs folder in the cache dir, VLREC_CACHE_DIR when set.
func directory() string {
	return vlconfig.DefaultCacheFilePath("logs")
}

func parseLevel(s string) (slog.Level, error) {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("invalid log level: %s", s)
	}
}

// Info logs an info level message
func (f *FileLogger) Info(msg string, args ...any) {
	f.logger.Info(msg, args...)
}

// Warn logs a warning level message
func (f *FileLogger) Warn(msg string, args ...any) {
	f.logger.Warn(msg, args...)
}

// Error logs an error level message
func (f *FileLogger) Error(msg string, args ...any) {
	f.logger.Error(msg, args...)
}

// cLIError logs a basic error message to both file and CLI
func (f *FileLogger) cLIError(msg string, err ...error) {
	if len(err) == 0 || err[0] == nil {
		f.logger.Error(msg)
		cLI.Error("%s", msg)
	} else {
		f.logger.Error(msg, "error", err[0])
		cLI.Error(msg+": %v", err[0])
	}
}

// Debug logs a debug level message
func (f *FileLogger) Debug(msg string, args ...any) {
	f.logger.Debug(msg, args...)
}

// With returns a new logger with the given attributes
func (f *FileLogger) With(args ...any) *slog.Logger {
	return f.logger.With(args...)
}

// withGroup returns a new logger with the given group name
func (f *FileLogger) withGroup(name string) *slog.Logger {
	return f.logger.WithGroup(name)
}

// Close shuts down the log rotator and stops any background goroutines.
// This should be called when the application is shutting down.
func (f *FileLogger) Close() error {
	defer f.logger.Debug("Log rotator closed")
	if f.rotator != nil {
		return f.rotator.Close()
	}
	return nil
}

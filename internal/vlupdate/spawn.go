package vlupdate

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Spawn launches the daemon binary as a detached background process. The CLI
// can exit afterward without killing the daemon.
//
// logPath, when set, receives the daemon's stdout and stderr. Without it a
// daemon that dies during startup takes its reason with it, and the caller can
// only report that nothing came up — the difference between "port 4223 is
// already in use" and an unfixable mystery. A file rather than a pipe: the
// child outlives this process, and a pipe whose read end closed would hand it
// EPIPE on its next write.
func Spawn(ctx context.Context, exePath, configFilePath, logPath string) error {
	args := []string{}
	if configFilePath != "" {
		args = append(args, "--config", configFilePath)
	}

	devNull, err := os.Open(os.DevNull)
	if err != nil {
		return err
	}
	defer devNull.Close()

	var out *os.File
	if logPath != "" {
		if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err == nil {
			// Truncated per spawn: the interesting content is why *this* start
			// failed, and an append-only file would bury it.
			out, err = os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
			if err != nil {
				out = nil
			}
		}
	}
	if out != nil {
		defer out.Close()
	} else {
		out = devNull
	}

	c := exec.Command(exePath, args...)
	c.Stdin = devNull
	c.Stdout = out
	c.Stderr = out
	c.SysProcAttr = detachAttrs()

	if err := c.Start(); err != nil {
		return fmt.Errorf("starting daemon: %w", err)
	}
	// Release so the parent isn't tracking it as a child.
	if err := c.Process.Release(); err != nil {
		return fmt.Errorf("releasing daemon process: %w", err)
	}
	return nil
}

// SpawnLogTail returns the last maxBytes of the spawn log, trimmed, or "" when
// there is nothing to report. Used to attach a failed daemon's own words to the
// caller's "daemon not ready" error.
func SpawnLogTail(logPath string, maxBytes int64) string {
	if logPath == "" {
		return ""
	}
	f, err := os.Open(logPath)
	if err != nil {
		return ""
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return ""
	}
	if info.Size() > maxBytes {
		if _, err := f.Seek(-maxBytes, io.SeekEnd); err != nil {
			return ""
		}
	}
	buf := make([]byte, maxBytes)
	n, _ := f.Read(buf)
	return strings.TrimSpace(string(buf[:n]))
}

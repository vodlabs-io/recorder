package errreport

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"strings"

	"github.com/getsentry/sentry-go"
)

// CrashFile is where the Go runtime writes a fatal panic or error, and
// CrashFilePrev is where the last one is kept once it has been read.
const (
	CrashFile     = "crash.log"
	CrashFilePrev = "crash.prev.log"
)

// maxCrashBytes bounds how much of a crash is read: the panicking goroutine
// comes first, and a crash with thousands of goroutines can run to megabytes.
const maxCrashBytes = 256 << 10

// WatchCrashes reports the crash the previous run left in dir, if any and
// while error reports are on, then has the runtime write this run's fatal
// errors there too. Call it after Init.
func WatchCrashes(dir string) error {
	path := filepath.Join(dir, CrashFile)
	if b, err := readHead(path, maxCrashBytes); err == nil && len(strings.TrimSpace(string(b))) > 0 {
		// Kept for bug reports, which attach it, whether or not this is sent.
		_ = os.Rename(path, filepath.Join(dir, CrashFilePrev))
		if Enabled() {
			captureCrash(string(b))
		}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	// The runtime keeps its own copy of the descriptor.
	defer f.Close()
	return debug.SetCrashOutput(f, debug.CrashOptions{})
}

func readHead(path string, n int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	buf := make([]byte, n)
	m, err := f.Read(buf)
	if m > 0 {
		return buf[:m], nil
	}
	return nil, err
}

// numberRe matches the parts of a panic message that change between
// occurrences of the same bug: indexes, lengths and addresses.
var numberRe = regexp.MustCompile(`0x[0-9a-f]+|\d+`)

// captureCrash sends the runtime's crash output as a fatal event, grouped by
// its first line and the first frame in this module.
func captureCrash(out string) {
	title, frame := crashSummary(out)
	ev := sentry.NewEvent()
	ev.Level = sentry.LevelFatal
	ev.Message = title
	ev.Exception = []sentry.Exception{{Type: "crash", Value: title}}
	ev.Contexts["crash"] = map[string]any{"traceback": firstGoroutine(out)}
	ev.Fingerprint = []string{"crash", numberRe.ReplaceAllString(title, "N"), frame}
	ev.Tags["crash"] = "previous_run"
	hub := sentry.CurrentHub().Clone()
	hub.Scope().AddAttachment(&sentry.Attachment{
		Filename:    CrashFile,
		ContentType: "text/plain",
		Payload:     []byte(scrubString(out)),
	})
	hub.CaptureEvent(ev)
}

// crashSummary returns the crash's first non-empty line, such as
// "panic: runtime error: index out of range [3] with length 3", and the first
// function from this module in the traceback.
func crashSummary(out string) (title, frame string) {
	sc := bufio.NewScanner(strings.NewReader(out))
	sc.Buffer(make([]byte, 64<<10), maxCrashBytes)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		if title == "" {
			title = line
			continue
		}
		if frame == "" && strings.HasPrefix(line, "github.com/vodlabs-io/recorder/") {
			if i := strings.LastIndexByte(line, '('); i > 0 {
				line = line[:i]
			}
			frame = line
			break
		}
	}
	if title == "" {
		title = "crash"
	}
	return title, frame
}

// firstGoroutine is the output up to the second goroutine's trace: the
// panic and the goroutine that raised it.
func firstGoroutine(out string) string {
	first := strings.Index(out, "\ngoroutine ")
	if first < 0 {
		return out
	}
	if next := strings.Index(out[first+1:], "\ngoroutine "); next >= 0 {
		return out[:first+1+next]
	}
	return out
}

package errreport

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/getsentry/sentry-go"
)

const (
	// maxMessageBytes bounds the user's description of the problem.
	maxMessageBytes = 8 << 10
	// maxLogTailBytes is how much of the end of each log file is attached.
	maxLogTailBytes = 1 << 20
	// maxLogBytes bounds all attached logs together.
	maxLogBytes = 8 << 20
	// sendTimeout bounds how long a bug report, with its logs, takes to leave.
	sendTimeout = 30 * time.Second
)

// BugReport is a report the user sends on purpose.
type BugReport struct {
	// Message is what the user wrote.
	Message string
	// Component is where it was sent from: "app", "web" or "cli".
	Component string
	// ClientID is the recorder's client ID, so the report can be matched to
	// the service's records of this recorder.
	ClientID string
	// Extra holds facts about the recorder, such as the app version.
	Extra map[string]string
	// Logs are attached as files.
	Logs []Attachment
}

// Attachment is a file sent with a report.
type Attachment struct {
	Name string
	Data []byte
}

// SendBugReport sends r and waits for it to leave, returning its event ID,
// which identifies the report in Sentry.
func SendBugReport(r BugReport) (string, error) {
	if !ready {
		return "", ErrUnavailable
	}
	msg := strings.TrimSpace(r.Message)
	if len(msg) > maxMessageBytes {
		msg = truncateUTF8(msg, maxMessageBytes)
	}
	hub := sentry.CurrentHub().Clone()
	scope := hub.Scope()
	for _, a := range r.Logs {
		scope.AddAttachment(&sentry.Attachment{Filename: a.Name, ContentType: "text/plain", Payload: a.Data})
	}

	ev := sentry.NewEvent()
	ev.EventID = newEventID()
	ev.Level = sentry.LevelInfo
	ev.Message = "Bug report"
	if first, _, _ := strings.Cut(msg, "\n"); first != "" {
		ev.Message = "Bug report: " + truncateUTF8(first, 120)
	}
	ev.Tags[tagReport] = "bug"
	if r.Component != "" {
		ev.Tags["sent_from"] = r.Component
	}
	details := map[string]any{"description": msg, "logs_attached": len(r.Logs)}
	for k, v := range r.Extra {
		details[k] = v
	}
	ev.Contexts["bug_report"] = details
	ev.User = sentry.User{ID: r.ClientID, IPAddress: "{{auto}}"}
	// Every report is its own issue.
	ev.Fingerprint = []string{"bug-report", string(ev.EventID)}

	id := hub.CaptureEvent(ev)
	if id == nil {
		return "", ErrUnavailable
	}
	if !hub.Flush(sendTimeout) {
		return "", errSendTimeout
	}
	return string(*id), nil
}

func newEventID() sentry.EventID {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return sentry.EventID(hex.EncodeToString(b[:]))
}

var errSendTimeout = errorString("the report could not be sent in time; check the internet connection and try again")

type errorString string

func (e errorString) Error() string { return string(e) }

// rotatedLogRe matches the backups the log rotator leaves beside a log, such
// as daemon-2026-10-30T07-55-32.123.log.
var rotatedLogRe = regexp.MustCompile(`-\d{4}-\d{2}-\d{2}T[\d.-]+\.log(\.gz)?$`)

// LogTails reads the end of each current log file in dirs, scrubbed as
// reports are, for attaching to a bug report. Rotated backups are skipped.
func LogTails(dirs ...string) []Attachment {
	seen := map[string]bool{}
	var names []string
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() || !strings.HasSuffix(name, ".log") || rotatedLogRe.MatchString(name) {
				continue
			}
			path := filepath.Join(dir, name)
			if !seen[path] {
				seen[path] = true
				names = append(names, path)
			}
		}
	}
	sort.Strings(names)

	var out []Attachment
	budget := maxLogBytes
	for _, path := range names {
		if budget <= 0 {
			break
		}
		b, err := readTail(path, min(maxLogTailBytes, budget))
		if err != nil || len(b) == 0 {
			continue
		}
		data := []byte(scrubString(string(b)))
		budget -= len(data)
		out = append(out, Attachment{Name: filepath.Base(path), Data: data})
	}
	return out
}

// readTail returns up to n bytes from the end of the file, starting at a
// line boundary.
func readTail(path string, n int) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	off := st.Size() - int64(n)
	if off < 0 {
		off = 0
	}
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return nil, err
	}
	b, err := io.ReadAll(io.LimitReader(f, int64(n)))
	if err != nil {
		return nil, err
	}
	if off > 0 {
		if i := bytes.IndexByte(b, '\n'); i >= 0 {
			b = b[i+1:]
		}
	}
	return b, nil
}

func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n]
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}

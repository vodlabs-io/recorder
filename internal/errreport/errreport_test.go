package errreport

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/getsentry/sentry-go"
)

type fakeTransport struct {
	mu     sync.Mutex
	events []*sentry.Event
}

func (t *fakeTransport) Configure(sentry.ClientOptions) {}
func (t *fakeTransport) SendEvent(e *sentry.Event) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.events = append(t.events, e)
}
func (t *fakeTransport) Flush(time.Duration) bool              { return true }
func (t *fakeTransport) FlushWithContext(context.Context) bool { return true }
func (t *fakeTransport) Close()                                {}
func (t *fakeTransport) taken() []*sentry.Event {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := t.events
	t.events = nil
	return out
}

// setup starts a client that records events instead of sending them.
func setup(t *testing.T, on bool) *fakeTransport {
	t.Helper()
	ft := &fakeTransport{}
	transport = ft
	t.Setenv(DSNEnv, "https://key@o1.ingest.us.sentry.io/1")
	t.Setenv("HOME", "/Users/jane")
	flush := Init(Options{Component: "daemon", Enabled: &on})
	t.Cleanup(func() {
		flush()
		transport = nil
		ready = false
		SetEnabled(false)
	})
	return ft
}

func TestHandlerReportsErrorsOnlyWhenOn(t *testing.T) {
	ft := setup(t, false)
	log := slog.New(Handler(slog.DiscardHandler))

	log.Error("first", "error", errors.New("boom"))
	if got := ft.taken(); len(got) != 0 {
		t.Fatalf("sent %d events while off", len(got))
	}

	SetEnabled(true)
	log.Info("before", "url", "https://cdn.example/live.flv?token=abc")
	log.Error("second", "error", errors.New("boom at /Users/jane/Downloads/x"))
	got := ft.taken()
	if len(got) != 1 {
		t.Fatalf("sent %d events, want 1", len(got))
	}
	ev := got[0]
	if ev.Exception[0].Type != "second" || ev.Exception[0].Value != "boom at ~/Downloads/x" {
		t.Errorf("exception = %q: %q", ev.Exception[0].Type, ev.Exception[0].Value)
	}
	if ev.ServerName != "" {
		t.Errorf("ServerName = %q, want it removed", ev.ServerName)
	}
	if ev.User.IPAddress != "{{auto}}" {
		t.Errorf("User.IPAddress = %q", ev.User.IPAddress)
	}
	if len(ev.Breadcrumbs) != 1 || ev.Breadcrumbs[0].Data["url"] != "https://cdn.example/live.flv?[removed]" {
		t.Errorf("breadcrumbs = %+v", ev.Breadcrumbs)
	}
}

func TestHandlerLimitsEachCallSite(t *testing.T) {
	ft := setup(t, true)
	log := slog.New(Handler(slog.DiscardHandler))
	for range 3 {
		log.Error("loop")
	}
	if got := ft.taken(); len(got) != 1 {
		t.Fatalf("sent %d events from one call site, want 1", len(got))
	}
}

func TestBugReportSentWhileErrorReportsOff(t *testing.T) {
	ft := setup(t, false)
	id, err := SendBugReport(BugReport{
		Message:   "Recording stops\nafter an hour. Mail me at jane@example.com",
		Component: "app",
		ClientID:  "UABC",
		Extra:     map[string]string{"app_version": "0.0.20"},
		Logs:      []Attachment{{Name: "daemon.log", Data: []byte("x")}},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := ft.taken()
	if len(got) != 1 {
		t.Fatalf("sent %d events, want 1", len(got))
	}
	ev := got[0]
	if string(ev.EventID) != id {
		t.Errorf("event ID %q, returned %q", ev.EventID, id)
	}
	if ev.Message != "Bug report: Recording stops" || ev.User.ID != "UABC" {
		t.Errorf("message %q, user %q", ev.Message, ev.User.ID)
	}
	if d := ev.Contexts["bug_report"]["description"]; !strings.Contains(d.(string), "[email]") {
		t.Errorf("description = %q, want the email removed", d)
	}
	if len(ev.Attachments) != 1 || ev.Fingerprint[1] != id {
		t.Errorf("attachments %d, fingerprint %v", len(ev.Attachments), ev.Fingerprint)
	}
}

func TestBugReportUnavailableWithoutClient(t *testing.T) {
	if _, err := SendBugReport(BugReport{Message: "x"}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
}

func TestClientDSN(t *testing.T) {
	t.Setenv(DSNEnv, "off")
	if key, _ := clientDSN(); key != "" {
		t.Errorf("off: key %q", key)
	}
	t.Setenv(DSNEnv, "https://k@h/2")
	if key, env := clientDSN(); key != "https://k@h/2" || env != "development" {
		t.Errorf("override: %q %q", key, env)
	}
	os.Unsetenv(DSNEnv)
	if key, _ := clientDSN(); key != "" {
		t.Errorf("devel build without override: key %q", key)
	}
}

func TestScrubStringWindowsHome(t *testing.T) {
	setHome(`C:\Users\Jane`)
	t.Cleanup(func() { setHome("") })
	in := `open C:\Users\Jane\Videos\a.flv; path="C:\\Users\\Jane\\Videos" eyJa.eyJb.c`
	want := `open ~\Videos\a.flv; path="~\\Videos" [removed]`
	if got := scrubString(in); got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}
}

const sampleCrash = `panic: runtime error: index out of range [3] with length 3

goroutine 42 [running]:
github.com/vodlabs-io/recorder/internal/session.(*Worker).run(0x14000123456)
	/Users/jane/src/recorder/internal/session/worker.go:120 +0x1f4
created by github.com/vodlabs-io/recorder/internal/session.Start in goroutine 1
	/Users/jane/src/recorder/internal/session/registry.go:80 +0x88

goroutine 1 [select]:
main.main()
`

func TestCrashSummary(t *testing.T) {
	title, frame := crashSummary(sampleCrash)
	if title != "panic: runtime error: index out of range [3] with length 3" {
		t.Errorf("title = %q", title)
	}
	if frame != "github.com/vodlabs-io/recorder/internal/session.(*Worker).run" {
		t.Errorf("frame = %q", frame)
	}
	if g := firstGoroutine(sampleCrash); strings.Contains(g, "main.main") || !strings.Contains(g, "worker.go:120") {
		t.Errorf("firstGoroutine = %q", g)
	}
}

func TestWatchCrashesReportsPreviousRun(t *testing.T) {
	ft := setup(t, true)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, CrashFile), []byte(sampleCrash), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WatchCrashes(dir); err != nil {
		t.Fatal(err)
	}
	got := ft.taken()
	if len(got) != 1 || got[0].Level != sentry.LevelFatal {
		t.Fatalf("events = %+v", got)
	}
	if a := got[0].Attachments; len(a) != 1 || strings.Contains(string(a[0].Payload), "/Users/jane") {
		t.Errorf("crash attachment not scrubbed: %+v", a)
	}
	if _, err := os.Stat(filepath.Join(dir, CrashFilePrev)); err != nil {
		t.Errorf("previous crash not kept: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, CrashFile)); len(b) != 0 {
		t.Errorf("crash file not reset: %q", b)
	}
}

func TestLogTails(t *testing.T) {
	setHome("/Users/jane")
	t.Cleanup(func() { setHome("") })
	dir := t.TempDir()
	long := strings.Repeat("old line\n", maxLogTailBytes/9+10) + "level=ERROR path=/Users/jane/x\n"
	files := map[string]string{
		"daemon.log":                         long,
		"daemon-2026-10-30T07-55-32.123.log": "rotated",
		"desktop.prev.log":                   "prev",
		"notes.txt":                          "not a log",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got := LogTails(dir, dir)
	if len(got) != 2 || got[0].Name != "daemon.log" || got[1].Name != "desktop.prev.log" {
		t.Fatalf("attachments = %v", names(got))
	}
	d := string(got[0].Data)
	if len(d) > maxLogTailBytes || !strings.HasPrefix(d, "old line\n") || !strings.HasSuffix(d, "path=~/x\n") {
		t.Errorf("tail starts %q, ends %q, %d bytes", d[:20], d[len(d)-20:], len(d))
	}
}

func names(as []Attachment) []string {
	var out []string
	for _, a := range as {
		out = append(out, a.Name)
	}
	return out
}

package vlapi

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go/micro"
	"github.com/vodlabs-io/recorder/internal/streamer"
	"github.com/vodlabs-io/recorder/internal/vlconfig"
	"github.com/vodlabs-io/recorder/internal/webpage"
)

// fakeRequest is a micro.Request that captures the handler's reply instead of
// publishing it, so the add path can be driven without a NATS server.
type fakeRequest struct {
	data  []byte
	reply []byte
}

func (r *fakeRequest) Respond(b []byte, _ ...micro.RespondOpt) error {
	r.reply = b
	return nil
}

func (r *fakeRequest) RespondJSON(v any, _ ...micro.RespondOpt) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	r.reply = b
	return nil
}

func (r *fakeRequest) Error(code, description string, data []byte, _ ...micro.RespondOpt) error {
	r.reply = data
	return nil
}

func (r *fakeRequest) Data() []byte           { return r.data }
func (r *fakeRequest) Headers() micro.Headers { return nil }
func (r *fakeRequest) Subject() string        { return "streamer.add" }
func (r *fakeRequest) Reply() string          { return "" }

// envelope is the decoded reply.
type envelope struct {
	OK    bool `json:"ok"`
	Error *Err `json:"error"`
}

// addEnvelope is envelope with the add's data half decoded.
type addEnvelope struct {
	OK    bool         `json:"ok"`
	Data  *AddResponse `json:"data"`
	Error *Err         `json:"error"`
}

func (r *fakeRequest) addEnvelope(t *testing.T) addEnvelope {
	t.Helper()
	var env addEnvelope
	if err := json.Unmarshal(r.reply, &env); err != nil {
		t.Fatalf("decode reply %q: %v", r.reply, err)
	}
	return env
}

func (r *fakeRequest) envelope(t *testing.T) envelope {
	t.Helper()
	var env envelope
	if err := json.Unmarshal(r.reply, &env); err != nil {
		t.Fatalf("decode reply %q: %v", r.reply, err)
	}
	return env
}

// rejection is a stub verdict shaped like the cloud package's rejectionError,
// which vlapi reads through an interface.
type rejection struct {
	msg        string
	definitive bool
}

func (e *rejection) Error() string    { return e.msg }
func (e *rejection) Definitive() bool { return e.definitive }

// testSignals is a ConfigSignals whose channels nothing consumes; the sends are
// non-blocking, so a handler under test never stalls on them.
func testSignals() ConfigSignals {
	return ConfigSignals{
		Resubscribe: make(chan struct{}, 4),
		Reload:      make(chan struct{}, 4),
	}
}

// newAddService builds the slice of Service the add path touches, over a
// throwaway streamers.toml.
func newAddService(t *testing.T, resolve func(context.Context, streamer.StreamerRef) (streamer.StreamerRef, error)) (*Service, *vlconfig.StreamersFile, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "streamers.toml")
	sf, err := vlconfig.NewStreamersFile(path)
	if err != nil {
		t.Fatalf("streamers file: %v", err)
	}
	s := &Service{
		streamersFile: sf,
		streamerFuncs: StreamerFuncs{Resolve: resolve},
		// Buffered so the handler never blocks on a consumer that a unit test
		// does not have.
		signals:        testSignals(),
		statusReqPriCh: make(chan streamer.StreamerRef, 4),
	}
	return s, sf, path
}

func addRequest(t *testing.T, id streamer.StreamerRef) *fakeRequest {
	t.Helper()
	b, err := json.Marshal(id)
	if err != nil {
		t.Fatalf("marshal id: %v", err)
	}
	return &fakeRequest{data: b}
}

func fileContents(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return ""
	}
	if err != nil {
		t.Fatalf("read streamers file: %v", err)
	}
	return string(b)
}

// TestAddStreamer_RejectedWritesNothing is the property the whole preflight
// exists for. A URL that names no streamer must leave no trace: not in
// streamers.toml, and so not in a later subscribe, a view doc or a row.
func TestAddStreamer_RejectedWritesNothing(t *testing.T) {
	cases := []struct {
		name       string
		definitive bool
		wantCode   string
	}{
		{"platform says no", true, ErrNotFound.Error()},
		// Refused too, but distinguishable: the cloud could not answer, so a
		// client can tell "does not exist" from "try again".
		{"cloud unreachable", false, errUnavailable.Error()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, sf, path := newAddService(t, func(context.Context, streamer.StreamerRef) (streamer.StreamerRef, error) {
				return streamer.StreamerRef{}, &rejection{msg: "nope", definitive: tc.definitive}
			})

			req := addRequest(t, streamer.StreamerRef{URL: "https://www.twitch.tv/nosuchuser"})
			s.handleAddStreamer(req)

			env := req.envelope(t)
			if env.OK {
				t.Fatal("add was accepted, want a refusal")
			}
			if env.Error == nil || env.Error.Code != tc.wantCode {
				t.Errorf("error = %+v, want code %q", env.Error, tc.wantCode)
			}
			if env.Error.Message != "nope" {
				t.Errorf("message = %q, want the cloud's own wording", env.Error.Message)
			}
			if got := fileContents(t, path); strings.Contains(got, "nosuchuser") {
				t.Errorf("streamers.toml kept the rejected streamer:\n%s", got)
			}
			if err := sf.Load(); err != nil {
				t.Fatalf("reload: %v", err)
			}
			if ids := sf.AllIDs(); len(ids) != 0 {
				t.Errorf("streamers.toml lists %v, want nothing", ids.Strings())
			}
		})
	}
}

// TestAddStreamer_StoresResolvedIdentity covers the other half: what gets
// written is what the cloud decoded, not what the user pasted (a YouTube watch
// URL names a broadcast, not a streamer).
func TestAddStreamer_StoresResolvedIdentity(t *testing.T) {
	resolved := streamer.StreamerRef{StreamerKey: streamer.StreamerKey{Platform: "youtube", Username: "streamer_twelve"}}
	s, sf, path := newAddService(t, func(_ context.Context, got streamer.StreamerRef) (streamer.StreamerRef, error) {
		if got.URL != "https://www.youtube.com/watch?v=dQw0000000a" {
			t.Errorf("preflight got %+v, want the URL as pasted", got)
		}
		return resolved, nil
	})

	req := addRequest(t, streamer.StreamerRef{URL: "https://www.youtube.com/watch?v=dQw0000000a"})
	s.handleAddStreamer(req)

	if env := req.envelope(t); !env.OK {
		t.Fatalf("add refused: %+v", env.Error)
	}
	if err := sf.Load(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	ids := sf.AllIDs()
	if len(ids) != 1 || ids[0].StreamerKey != resolved.StreamerKey {
		t.Fatalf("streamers.toml lists %v, want %s", ids.Strings(), resolved.StreamerKey)
	}
	if got := fileContents(t, path); strings.Contains(got, "watch?v=") {
		t.Errorf("the pasted URL was written as well:\n%s", got)
	}
	// The subscription has to be told, or the streamer sits unwatched.
	select {
	case <-s.signals.Resubscribe:
	default:
		t.Error("no resubscribe was triggered")
	}
}

// TestAddStreamer_DuplicateAfterResolving covers the case only preflight can
// see: the same streamer added once by username and once by a URL. They look
// nothing alike until the cloud has decoded the URL, so the duplicate check has
// to run again on the resolved identity.
func TestAddStreamer_DuplicateAfterResolving(t *testing.T) {
	resolved := streamer.StreamerRef{StreamerKey: streamer.StreamerKey{Platform: "twitch", Username: "streamer_one"}}
	s, sf, _ := newAddService(t, func(context.Context, streamer.StreamerRef) (streamer.StreamerRef, error) {
		return resolved, nil
	})

	s.handleAddStreamer(addRequest(t, streamer.StreamerRef{StreamerKey: streamer.StreamerKey{Platform: "twitch", Username: "streamer_one"}}))

	req := addRequest(t, streamer.StreamerRef{URL: "https://twitch.tv/XQC"})
	s.handleAddStreamer(req)

	env := req.envelope(t)
	if env.OK {
		t.Fatal("the duplicate was accepted")
	}
	if env.Error == nil || env.Error.Code != ErrConflict.Error() {
		t.Errorf("error = %+v, want a conflict", env.Error)
	}
	if err := sf.Load(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if ids := sf.AllIDs(); len(ids) != 1 {
		t.Errorf("streamers.toml lists %v, want one entry", ids.Strings())
	}
}

// TestRecordOnce_Preflights checks that record-once refuses an unresolvable URL
// just as an ordinary add does.
func TestRecordOnce_Preflights(t *testing.T) {
	s, sf, _ := newAddService(t, func(context.Context, streamer.StreamerRef) (streamer.StreamerRef, error) {
		return streamer.StreamerRef{}, &rejection{msg: "no such account", definitive: true}
	})

	req := addRequest(t, streamer.StreamerRef{URL: "https://vimeo.com/12345"})
	s.handleRecordOnce(req)

	if env := req.envelope(t); env.OK {
		t.Fatal("record-once was accepted, want a refusal")
	}
	if err := sf.Load(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if ids := sf.AllIDs(); len(ids) != 0 {
		t.Errorf("streamers.toml lists %v, want nothing", ids.Strings())
	}
}

// TestLooksLikeWebAddress covers the only check made without the cloud. It is a
// filter for obvious nonsense, so it must stay generous: anything URL-shaped is
// the cloud's question, and a scheme-less paste is what browsers put on the
// clipboard.
func TestLooksLikeWebAddress(t *testing.T) {
	ok := []string{
		"https://twitch.tv/streamer_one",
		"http://twitch.tv/streamer_one",
		"twitch.tv/streamer_one",
		"www.youtube.com/@Streamer Five/live",
		"//kick.com/streamer_one",
		"  https://kick.com/streamer_one  ",
		// Nonsense to a plugin, but URL-shaped: the cloud decides, not this.
		"https://vimeo.com/12345",
	}
	for _, u := range ok {
		if !looksLikeWebAddress(u) {
			t.Errorf("looksLikeWebAddress(%q) = false, want true", u)
		}
	}

	bad := []string{
		"",
		"   ",
		"not a url at all",
		"streamer_one",
		"ftp://twitch.tv/streamer_one",
		"javascript:alert(1)",
	}
	for _, u := range bad {
		if looksLikeWebAddress(u) {
			t.Errorf("looksLikeWebAddress(%q) = true, want false", u)
		}
	}
}

func TestRejectionCode(t *testing.T) {
	if got := rejectionCode(&rejection{definitive: true}); !errors.Is(got, ErrNotFound) {
		t.Errorf("definitive rejection = %v, want ErrNotFound", got)
	}
	if got := rejectionCode(&rejection{definitive: false}); !errors.Is(got, errUnavailable) {
		t.Errorf("undecided rejection = %v, want ErrUnavailable", got)
	}
	// An error from outside the preflight contract is still a refusal, just not
	// a retryable one.
	if got := rejectionCode(errors.New("boom")); !errors.Is(got, ErrNotFound) {
		t.Errorf("plain error = %v, want ErrNotFound", got)
	}
}

// TestAddStreamer_RepliesWithResolvedIdentity pins that the add replies with
// the resolved platform/username, which a URL caller has no other way to learn
// (the CLI narrows its status view to it; the add dialog keys its options call
// on it).
func TestAddStreamer_RepliesWithResolvedIdentity(t *testing.T) {
	resolved := streamer.StreamerRef{StreamerKey: streamer.StreamerKey{Platform: "youtube", Username: "streamer_five"}}
	s, _, _ := newAddService(t, func(context.Context, streamer.StreamerRef) (streamer.StreamerRef, error) {
		return resolved, nil
	})

	// A broadcast URL, which doesn't name the streamer.
	req := addRequest(t, streamer.StreamerRef{URL: "https://www.youtube.com/watch?v=dQw0000000b"})
	s.handleAddStreamer(req)

	env := req.addEnvelope(t)
	if !env.OK || env.Data == nil {
		t.Fatalf("add failed: %+v", env)
	}
	if env.Data.Platform != "youtube" || env.Data.Username != "streamer_five" {
		t.Errorf("reply = %+v, want the resolved youtube/streamer_five", env.Data)
	}
	// The pasted URL is not echoed back: it names nothing the daemon keeps.
	if env.Data.URL != "" {
		t.Errorf("reply carried url %q, want it dropped", env.Data.URL)
	}
}

// record_once answers the same way, and for the same reason: `vod record --once
// <url>` waits on that streamer's KV keys, which it cannot name otherwise.
func TestRecordOnce_RepliesWithResolvedIdentity(t *testing.T) {
	resolved := streamer.StreamerRef{StreamerKey: streamer.StreamerKey{Platform: "twitch", Username: "streamer_one"}}
	s, _, _ := newAddService(t, func(context.Context, streamer.StreamerRef) (streamer.StreamerRef, error) {
		return resolved, nil
	})

	req := addRequest(t, streamer.StreamerRef{URL: "https://twitch.tv/XQC"})
	s.handleRecordOnce(req)

	env := req.addEnvelope(t)
	if !env.OK || env.Data == nil {
		t.Fatalf("record_once failed: %+v", env)
	}
	if env.Data.Platform != "twitch" || env.Data.Username != "streamer_one" {
		t.Errorf("reply = %+v, want the resolved twitch/streamer_one", env.Data)
	}
}

// fakeAdded records the keys MarkAdded was called for.
type fakeAdded struct {
	marked []streamer.StreamerKey
}

func (f *fakeAdded) MarkAdded(_ context.Context, key streamer.StreamerKey, _ time.Time) {
	f.marked = append(f.marked, key)
}

// TestAddStreamer_MarksAddedTime: an accepted add restarts the streamer's added
// time under its resolved identity; a refused one leaves any earlier time alone.
func TestAddStreamer_MarksAddedTime(t *testing.T) {
	resolved := streamer.StreamerRef{StreamerKey: streamer.StreamerKey{Platform: "twitch", Username: "streamer_one"}}
	refuse := false
	s, _, _ := newAddService(t, func(context.Context, streamer.StreamerRef) (streamer.StreamerRef, error) {
		if refuse {
			return streamer.StreamerRef{}, &rejection{msg: "nope", definitive: true}
		}
		return resolved, nil
	})
	added := &fakeAdded{}
	s.added = added

	req := addRequest(t, streamer.StreamerRef{URL: "https://www.twitch.tv/streamer_one"})
	s.handleAddStreamer(req)
	if env := req.envelope(t); !env.OK {
		t.Fatalf("add refused: %+v", env.Error)
	}
	if len(added.marked) != 1 || added.marked[0] != resolved.StreamerKey {
		t.Fatalf("marked %v, want only %s", added.marked, resolved.StreamerKey)
	}

	refuse = true
	req = addRequest(t, streamer.StreamerRef{URL: "https://www.twitch.tv/nosuchuser"})
	s.handleAddStreamer(req)
	if env := req.envelope(t); env.OK {
		t.Fatal("add was accepted, want a refusal")
	}
	if len(added.marked) != 1 {
		t.Errorf("a refused add marked %v", added.marked[1:])
	}
}

// unsupportedSite is the cloud's verdict that no platform claims a URL.
type unsupportedSiteErr struct{}

func (unsupportedSiteErr) Error() string         { return "That site isn't supported." }
func (unsupportedSiteErr) UnsupportedSite() bool { return true }

// genericResolve stands in for the cloud refusing a page on a site without its
// own support.
func genericResolve(context.Context, streamer.StreamerRef) (streamer.StreamerRef, error) {
	return streamer.StreamerRef{}, unsupportedSiteErr{}
}

// pageService wires a page resolver that answers live and records which pages
// it was asked about, and which it forgot.
type pageService struct {
	asked     []string
	forgotten []streamer.StreamerKey
}

func newPageService(t *testing.T) (*Service, *vlconfig.StreamersFile, *pageService) {
	s, sf, _ := newAddService(t, genericResolve)
	p := &pageService{}
	s.streamerFuncs.ResolvePage = func(_ context.Context, rawURL string) (streamer.StreamerRef, error) {
		p.asked = append(p.asked, rawURL)
		key, _ := webpage.KeyFor(rawURL)
		return streamer.StreamerRef{StreamerKey: key}, nil
	}
	s.streamerFuncs.ForgetPage = func(key streamer.StreamerKey) { p.forgotten = append(p.forgotten, key) }
	return s, sf, p
}

// A page that can only be recorded once is refused as a monitored add, with a
// code clients turn into a Record once offer, and is neither read nor written.
func TestAddStreamer_RefusesToMonitorANoMonitorPlatform(t *testing.T) {
	s, sf, pages := newPageService(t)
	req := addRequest(t, streamer.StreamerRef{URL: "https://example.com/live"})
	s.handleAddStreamer(req)

	env := req.envelope(t)
	if env.OK || env.Error == nil || env.Error.Code != ErrMonitorUnsupported.Error() {
		t.Fatalf("reply = %+v, want %s", env, ErrMonitorUnsupported)
	}
	if len(pages.asked) != 0 {
		t.Fatalf("a monitored add read the page: %v", pages.asked)
	}
	if err := sf.Load(); err != nil {
		t.Fatal(err)
	}
	if ids := sf.AllIDs(); len(ids) != 0 {
		t.Fatalf("streamers.toml lists %v, want nothing", ids.Strings())
	}
}

// Recording a page once checks it on this machine, and removing it by its URL
// needs no check at all.
func TestRecordOnce_PageIsCheckedHereAndRemovedByURL(t *testing.T) {
	s, _, pages := newPageService(t)
	req := addRequest(t, streamer.StreamerRef{URL: "https://example.com/live"})
	s.handleRecordOnce(req)
	env := req.addEnvelope(t)
	want, _ := webpage.KeyFor("https://example.com/live")
	if !env.OK || env.Data.StreamerKey != want {
		t.Fatalf("reply = %+v, want the one-shot %v accepted", env, want)
	}
	if len(pages.asked) != 1 {
		t.Fatalf("page checks = %v, want one", pages.asked)
	}

	rm := addRequest(t, streamer.StreamerRef{URL: "https://example.com/live"})
	s.handleRemoveStreamer(rm)
	if env := rm.envelope(t); !env.OK {
		t.Fatalf("remove reply = %+v", env)
	}
	if len(pages.asked) != 1 {
		t.Fatalf("removing by URL checked the page again: %v", pages.asked)
	}
	if len(pages.forgotten) != 1 || pages.forgotten[0] != want {
		t.Fatalf("forgotten = %v, want %v", pages.forgotten, want)
	}
}

// A URL the cloud cannot check at all is not taken for a page.
func TestRecordOnce_UnavailableCloudIsNotAPage(t *testing.T) {
	s, _, pages := newPageService(t)
	s.streamerFuncs.Resolve = func(context.Context, streamer.StreamerRef) (streamer.StreamerRef, error) {
		return streamer.StreamerRef{}, errors.New("cloud unreachable")
	}
	req := addRequest(t, streamer.StreamerRef{URL: "https://example.com/live"})
	s.handleRecordOnce(req)
	if env := req.envelope(t); env.OK || len(pages.asked) != 0 {
		t.Fatalf("reply = %+v, page checks = %v", env, pages.asked)
	}
}

func TestSetOptions_RefusesToMonitorANoMonitorPlatform(t *testing.T) {
	s, _, _ := newAddService(t, genericResolve)
	on := true
	b, _ := json.Marshal(setOptionsRequest{
		StreamerRef: streamer.StreamerRef{StreamerKey: streamer.StreamerKey{Platform: "generic", Username: "example.com-0123456789"}},
		Monitor:     &on,
	})
	req := &fakeRequest{data: b}
	s.handleSetStreamerOptions(req)
	if env := req.envelope(t); env.OK || env.Error.Code != ErrMonitorUnsupported.Error() {
		t.Fatalf("reply = %+v, want %s", env, ErrMonitorUnsupported)
	}
}

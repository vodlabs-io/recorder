package instagram

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

var testLog = slog.New(slog.NewTextHandler(io.Discard, nil))

// fakeCDN serves a broadcast's manifest and numbered segments.
type fakeCDN struct {
	mu       sync.Mutex
	end      int64
	listed   int64
	typ      string
	gone     bool
	first    int64
	relative bool
	segments []string
	srv      *httptest.Server
}

func newFakeCDN(t *testing.T, end int64) *fakeCDN {
	t.Helper()
	f := &fakeCDN{end: end, listed: 10, typ: "dynamic", first: firstNumber}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeCDN) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if strings.HasSuffix(r.URL.Path, ".mpd") {
		if f.gone {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, f.manifest())
		return
	}
	if r.Method == http.MethodHead {
		if strings.Contains(r.URL.Path, fmt.Sprintf("_0-%d.", f.first)) {
			return
		}
		http.NotFound(w, r)
		return
	}
	f.segments = append(f.segments, r.URL.Path)
	fmt.Fprint(w, "x")
}

func (f *fakeCDN) manifest() string {
	base := f.srv.URL + "/v/hash/live-dash/dash-abr/"
	if f.relative {
		base = ""
	}
	var tl strings.Builder
	for i := f.listed - 1; i >= 0; i-- {
		t := 4934 + (f.end-i-firstNumber)*2000
		fmt.Fprintf(&tl, `<S t="%d" d="2000"/>`, t)
	}
	rep := func(id, attrs, ext string) string {
		return fmt.Sprintf(`<Representation id="%s" %s>
  <BaseURL>%s</BaseURL><SegmentTemplate presentationTimeOffset="0" timescale="1000" initialization="../%s/1_0-init.%s?ms=m_C&amp;ccb=2-4" media="../%s/1_0-$Time$.%s?ms=m_C">
    <SegmentTimeline FBPredictedMedia="../ID/%s/1_0-$Number$.%s?ms=m_C&amp;ccb=2-4" FBPredictedMediaEndNumber="%d">%s</SegmentTimeline>
  </SegmentTemplate></Representation>`, id, attrs, base, id, ext, id, ext, id, ext, f.end, tl.String())
	}
	return fmt.Sprintf(`<MPD type="%s" firstAvTimeMs="1793832721577" publishFrameTime="4934"><Period id="dash-abr" start="PT0S">
<AdaptationSet>%s%s</AdaptationSet><AdaptationSet>%s</AdaptationSet></Period></MPD>`, f.typ,
		rep("dash-lp-hd-v", `mimeType="video/mp4" codecs="avc1.4d401e" width="432" height="768" frameRate="30" bandwidth="500000"`, "m4v"),
		rep("dash-lp-pst-v", `mimeType="video/mp4" codecs="avc1.4d401e" width="504" height="896" frameRate="30" bandwidth="2000000"`, "m4v"),
		rep("dash-lp-hd-a", `mimeType="audio/mp4" codecs="mp4a.40.2" audioSamplingRate="44100" bandwidth="44000"`, "m4a"))
}

func (f *fakeCDN) set(fn func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn()
}

func openSeat(t *testing.T, f *fakeCDN) *Seat {
	t.Helper()
	refreshEvery, advanceEvery = 0, 0
	raw := Scheme + "://17956329360037581?" + url.Values{"mpd": {f.srv.URL + "/v/hash/live-dash/dash-abr/17956329360037581.mpd?oh=1"}}.Encode()
	s, err := Open(context.Background(), raw, f.srv.Client(), "ua", testLog)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

func get(t *testing.T, s *Seat, u string) string {
	t.Helper()
	resp, err := s.Client(http.DefaultClient).Get(u)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestParse(t *testing.T) {
	id, mpd, err := Parse("instagram://123?mpd=https%3A%2F%2Fa.cdninstagram.com%2Fx.mpd%3Foh%3D1")
	if err != nil || id != "123" || mpd != "https://a.cdninstagram.com/x.mpd?oh=1" {
		t.Fatalf("%q %q %v", id, mpd, err)
	}
	for _, bad := range []string{"https://a/x.mpd", "instagram://abc?mpd=https://a/x.mpd", "instagram://123", "instagram://123?mpd=file:///etc/passwd"} {
		if _, _, err := Parse(bad); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
}

func TestMasterListsTheRenditionsAndTheirAudio(t *testing.T) {
	s := openSeat(t, newFakeCDN(t, 500))
	m := get(t, s, s.MasterURL)
	for _, want := range []string{
		`#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="audio"`, "dash-lp-hd-a.m3u8",
		"RESOLUTION=432x768", "dash-lp-hd-v.m3u8",
		"RESOLUTION=504x896", "dash-lp-pst-v.m3u8", `AUDIO="audio"`,
	} {
		if !strings.Contains(m, want) {
			t.Errorf("master lacks %q:\n%s", want, m)
		}
	}
}

// A broadcast noticed late is read from its first segment, a batch at a time,
// so the ingest's skip to the live edge finds nothing to skip.
func TestMediaPlaylistCatchesUpFromTheFirstSegment(t *testing.T) {
	s := openSeat(t, newFakeCDN(t, 500))
	u := playlistURL("dash-lp-pst-v")

	first := get(t, s, u)
	if !strings.Contains(first, "#EXT-X-MEDIA-SEQUENCE:100\n") {
		t.Errorf("first playlist does not start at the first segment:\n%s", first)
	}
	if n := strings.Count(first, "#EXTINF"); n != CatchUpBatch {
		t.Errorf("first playlist holds %d segments, want %d", n, CatchUpBatch)
	}
	if !strings.Contains(first, "/live-dash/ID/dash-lp-pst-v/1_0-100.m4v?ms=m_C&ccb=2-4") {
		t.Errorf("segment address wrong:\n%s", first)
	}
	if !strings.Contains(first, `#EXT-X-MAP:URI="`) || !strings.Contains(first, "/live-dash/dash-lp-pst-v/1_0-init.m4v") {
		t.Errorf("init address wrong:\n%s", first)
	}
	if !strings.Contains(first, "#EXT-X-PROGRAM-DATE-TIME:2026-11-04T") {
		t.Errorf("no wall clock:\n%s", first)
	}

	var last string
	for range 60 {
		last = get(t, s, u)
	}
	if !strings.Contains(last, "1_0-500.m4v") {
		t.Errorf("never reached the live edge:\n%s", last)
	}
	if strings.Contains(last, "#EXT-X-ENDLIST") {
		t.Error("ended while the broadcast is on")
	}
	if n := strings.Count(last, "#EXTINF"); n != playlistDepth {
		t.Errorf("playlist holds %d segments, want %d", n, playlistDepth)
	}
}

// The ingest reads a playlist twice as it starts, and skips what is more than half
// a minute behind the head of the first poll: both reads must hold the first batch.
func TestReadingAPlaylistTwiceAtOncePublishesOneBatch(t *testing.T) {
	s := openSeat(t, newFakeCDN(t, 500))
	advanceEvery = time.Hour
	defer func() { advanceEvery = 0 }()
	u := playlistURL("dash-lp-pst-v")
	get(t, s, u)
	if got := get(t, s, u); strings.Count(got, "#EXTINF") != CatchUpBatch {
		t.Errorf("second read holds %d segments", strings.Count(got, "#EXTINF"))
	}
}

// The ingest holds only so much of one rendition for the other to catch up with.
func TestRenditionsAdvanceTogetherWhileReadingThePast(t *testing.T) {
	f := newFakeCDN(t, 500)
	s := openSeat(t, f)
	video, audio := playlistURL("dash-lp-pst-v"), playlistURL("dash-lp-hd-a")
	get(t, s, video)
	get(t, s, audio)
	// Audio has been read and the video has not: the audio waits.
	for range 3 {
		if got := get(t, s, audio); strings.Count(got, "#EXTINF") != CatchUpBatch {
			t.Fatalf("audio ran ahead:\n%s", got)
		}
	}
	// The video catches up.
	client := s.Client(f.srv.Client())
	resp, err := client.Get(f.srv.URL + "/v/hash/live-dash/ID/dash-lp-pst-v/1_0-109.m4v?ms=m_C&ccb=2-4")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got := get(t, s, audio); strings.Count(got, "#EXTINF") != 2*CatchUpBatch {
		t.Fatalf("audio did not move on:\n%s", got)
	}
}

// The manifest a playback URL serves gives relative addresses, and the one a
// check's reply embeds absolute ones.
func TestAddressesRelativeToTheManifest(t *testing.T) {
	f := newFakeCDN(t, 105)
	f.relative = true
	s := openSeat(t, f)
	got := get(t, s, playlistURL("dash-lp-hd-v"))
	host := f.srv.URL
	if !strings.Contains(got, host+"/v/hash/live-dash/ID/dash-lp-hd-v/1_0-105.m4v") ||
		!strings.Contains(got, host+"/v/hash/live-dash/dash-lp-hd-v/1_0-init.m4v") {
		t.Errorf("addresses not resolved against the manifest:\n%s", got)
	}
}

func TestPlaylistsFollowTheBroadcastAndCloseAtItsEnd(t *testing.T) {
	f := newFakeCDN(t, 105)
	s := openSeat(t, f)
	u := playlistURL("dash-lp-hd-a")
	if got := get(t, s, u); !strings.Contains(got, "1_0-105.m4a") {
		t.Fatalf("%s", got)
	}
	f.set(func() { f.end = 107 })
	if got := get(t, s, u); !strings.Contains(got, "1_0-107.m4a") || strings.Contains(got, "1_0-108") {
		t.Fatalf("%s", got)
	}
	f.set(func() { f.end = 108; f.typ = "static" })
	if got := get(t, s, u); !strings.Contains(got, "1_0-108.m4a") || !strings.Contains(got, "#EXT-X-ENDLIST") {
		t.Fatalf("%s", got)
	}
}

func TestEndListWaitsForWhatIsStillUnpublished(t *testing.T) {
	f := newFakeCDN(t, 150)
	s := openSeat(t, f)
	u := playlistURL("dash-lp-hd-v")
	get(t, s, u)
	f.set(func() { f.gone = true })
	var got string
	for i := 0; ; i++ {
		got = get(t, s, u)
		if strings.Contains(got, "#EXT-X-ENDLIST") {
			break
		}
		if i > 10 {
			t.Fatal("never closed")
		}
	}
	if !strings.Contains(got, "1_0-150.m4v") {
		t.Errorf("closed before the last segment:\n%s", got)
	}
}

// An encoder restart is a new run of numbers under a new name.
func TestEncoderRestartStartsANewRun(t *testing.T) {
	f := newFakeCDN(t, 105)
	s := openSeat(t, f)
	u := playlistURL("dash-lp-hd-a")
	get(t, s, u)
	s.mu.Lock()
	s.renditions[2].media = "old"
	s.mu.Unlock()
	f.set(func() { f.end = 200 })
	got := get(t, s, u)
	if !strings.Contains(got, "#EXT-X-DISCONTINUITY") {
		t.Fatalf("%s", got)
	}
	if strings.Contains(got, "1_0-106.m4a") || !strings.Contains(got, "1_0-191.m4a") {
		t.Fatalf("%s", got)
	}
}

// A seat taken again after a failure carries on, rather than reading the whole
// broadcast over into a new part.
func TestSeatTakenAgainCarriesOn(t *testing.T) {
	f := newFakeCDN(t, 500)
	refreshEvery, advanceEvery = 0, 0
	raw := Scheme + "://777?" + url.Values{"mpd": {f.srv.URL + "/v/hash/a.mpd"}}.Encode()
	s, err := Open(context.Background(), raw, f.srv.Client(), "ua", testLog)
	if err != nil {
		t.Fatal(err)
	}
	video, audio := playlistURL("dash-lp-pst-v"), playlistURL("dash-lp-hd-a")
	get(t, s, video)
	get(t, s, audio)
	client := s.Client(f.srv.Client())
	for _, p := range []string{"dash-lp-pst-v/1_0-109.m4v", "dash-lp-hd-a/1_0-109.m4a"} {
		resp, err := client.Get(f.srv.URL + "/v/hash/live-dash/ID/" + p + "?ms=m_C&ccb=2-4")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	s.Close()

	s2, err := Open(context.Background(), raw, f.srv.Client(), "ua", testLog)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if got := get(t, s2, video); !strings.Contains(got, "#EXT-X-MEDIA-SEQUENCE:104\n") {
		t.Errorf("did not carry on:\n%s", got)
	}
}

// A broadcast read to its end is over, whatever a stale status still says.
func TestSeatOnABroadcastReadToItsEndIsRefused(t *testing.T) {
	f := newFakeCDN(t, 101)
	refreshEvery, advanceEvery = 0, 0
	raw := Scheme + "://888?" + url.Values{"mpd": {f.srv.URL + "/v/hash/a.mpd"}}.Encode()
	s, err := Open(context.Background(), raw, f.srv.Client(), "ua", testLog)
	if err != nil {
		t.Fatal(err)
	}
	u := playlistURL("dash-lp-hd-a")
	get(t, s, u)
	f.set(func() { f.typ = "static" })
	if got := get(t, s, u); !strings.Contains(got, "#EXT-X-ENDLIST") {
		t.Fatalf("%s", got)
	}
	client := s.Client(f.srv.Client())
	resp, err := client.Get(f.srv.URL + "/v/hash/live-dash/ID/dash-lp-hd-a/1_0-101.m4a?ms=m_C&ccb=2-4")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	s.Close()
	if _, err := Open(context.Background(), raw, f.srv.Client(), "ua", testLog); err != ErrEnded {
		t.Fatalf("err %v", err)
	}
}

func TestBroadcastWithItsPastGoneStartsAtWhatIsListed(t *testing.T) {
	f := newFakeCDN(t, 500)
	f.first = 1
	s := openSeat(t, f)
	got := get(t, s, playlistURL("dash-lp-hd-v"))
	if !strings.Contains(got, "#EXT-X-MEDIA-SEQUENCE:491\n") {
		t.Errorf("%s", got)
	}
}

func TestOpenAnEndedBroadcast(t *testing.T) {
	f := newFakeCDN(t, 500)
	f.gone = true
	raw := Scheme + "://1?" + url.Values{"mpd": {f.srv.URL + "/a.mpd"}}.Encode()
	if _, err := Open(context.Background(), raw, f.srv.Client(), "", testLog); err != ErrEnded {
		t.Fatalf("err %v", err)
	}
}

func TestGivesUpOnAManifestThatKeepsFailing(t *testing.T) {
	var fail bool
	var mu sync.Mutex
	f := newFakeCDN(t, 105)
	inner := f.srv.Config.Handler
	f.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if fail && strings.HasSuffix(r.URL.Path, ".mpd") {
			http.Error(w, "Bad URL hash", http.StatusForbidden)
			return
		}
		inner.ServeHTTP(w, r)
	})
	s := openSeat(t, f)
	u := playlistURL("dash-lp-hd-v")
	get(t, s, u)
	mu.Lock()
	fail = true
	mu.Unlock()
	if got := get(t, s, u); strings.Contains(got, "#EXT-X-ENDLIST") {
		t.Fatal("one failure ended the broadcast")
	}
	old := goneAfter
	goneAfter = time.Millisecond
	defer func() { goneAfter = old }()
	time.Sleep(5 * time.Millisecond)
	if got := get(t, s, u); !strings.Contains(got, "#EXT-X-ENDLIST") {
		t.Fatalf("%s", got)
	}
}

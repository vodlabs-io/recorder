package hlsingest

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// grantOrigin models an Origin that signs its playlist for less time than a
// Broadcast lasts: the URLs a mint hands out serve their segments until
// grantLife has passed, after which every one of them — the segments already
// published as readily as the ones still to come — answers 403.
//
// That is YouTube's shape, and the part that makes it hurt is that re-fetching
// the playlist does not renew anything: the same grant's URLs come back. Only
// another mint helps.
type grantOrigin struct {
	srv       *httptest.Server
	segDur    time.Duration
	window    int // segments published at once
	grantLife time.Duration

	mu      sync.Mutex
	minted  map[int]time.Time
	nextID  int
	started time.Time
}

func newGrantOrigin(t *testing.T, segDur, grantLife time.Duration, window int) *grantOrigin {
	t.Helper()
	o := &grantOrigin{
		segDur:    segDur,
		window:    window,
		grantLife: grantLife,
		minted:    map[int]time.Time{},
		started:   time.Now(),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/g/", func(w http.ResponseWriter, r *http.Request) {
		// /g/<id>/media.m3u8 or /g/<id>/<seq>.ts
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		if len(parts) != 3 {
			http.Error(w, "bad path", http.StatusNotFound)
			return
		}
		id, err := strconv.Atoi(parts[1])
		if err != nil {
			http.Error(w, "bad grant", http.StatusNotFound)
			return
		}
		if o.expired(id) {
			http.Error(w, "grant expired", http.StatusForbidden)
			return
		}
		if parts[2] == "media.m3u8" {
			o.writePlaylist(w, id)
			return
		}
		fmt.Fprint(w, strings.TrimSuffix(parts[2], ".ts"))
	})
	o.srv = httptest.NewServer(mux)
	t.Cleanup(o.srv.Close)
	return o
}

func (o *grantOrigin) expired(id int) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	at, ok := o.minted[id]
	return !ok || time.Since(at) > o.grantLife
}

// mint issues a new grant and returns its media playlist URL.
func (o *grantOrigin) mint() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.nextID++
	o.minted[o.nextID] = time.Now()
	return fmt.Sprintf("%s/g/%d/media.m3u8", o.srv.URL, o.nextID)
}

// edge is the sequence number of the newest published segment.
func (o *grantOrigin) edge() int {
	return int(time.Since(o.started) / o.segDur)
}

// writePlaylist publishes the live window. The media sequence is the
// Broadcast's own, so it is the same number under every grant — which is what
// lets an ingest tell one segment from another across a refresh.
func (o *grantOrigin) writePlaylist(w http.ResponseWriter, id int) {
	edge := o.edge()
	first := edge - o.window + 1
	if first < 0 {
		first = 0
	}
	fmt.Fprintf(w, "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:1\n#EXT-X-MEDIA-SEQUENCE:%d\n", first)
	for seq := first; seq <= edge; seq++ {
		fmt.Fprintf(w, "#EXTINF:%.3f,\n%d.ts\n", o.segDur.Seconds(), seq)
	}
}

// runGrantIngest reads the Origin for d, refreshing the playlist URL when ttl
// says to. A zero ttl leaves the ingest on its first grant.
func runGrantIngest(t *testing.T, o *grantOrigin, ttl time.Duration, d time.Duration) (*capture, int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()

	var mints int
	var mu sync.Mutex
	cfg := Config{
		URL:          o.mint(),
		PollInterval: 10 * time.Millisecond,
		Retries:      2,
		URLTTL:       ttl,
	}
	if ttl > 0 {
		cfg.RefreshURL = func(context.Context) (string, error) {
			mu.Lock()
			mints++
			mu.Unlock()
			return o.mint(), nil
		}
	}
	sink := &capture{}
	_ = Run(ctx, cfg, o.srv.Client(), slog.New(slog.DiscardHandler), sink)
	mu.Lock()
	defer mu.Unlock()
	return sink, mints
}

// sequences reads back the segment numbers a capture received, in order.
func sequences(t *testing.T, c *capture) []int {
	t.Helper()
	out := make([]int, 0, len(c.bodies()))
	for _, b := range c.bodies() {
		n, err := strconv.Atoi(b)
		if err != nil {
			t.Fatalf("segment body %q is not a sequence number", b)
		}
		out = append(out, n)
	}
	return out
}

// TestRefreshedGrantRecordsWithoutAGap is the whole point of Config.URLTTL: an
// Origin whose grant dies mid-recording keeps being recorded, contiguously,
// because the URL is re-resolved before it expires rather than after it fails.
func TestRefreshedGrantRecordsWithoutAGap(t *testing.T) {
	o := newGrantOrigin(t, 20*time.Millisecond, 300*time.Millisecond, 4)
	sink, mints := runGrantIngest(t, o, 250*time.Millisecond, 2*time.Second)

	got := sequences(t, sink)
	if len(got) < 40 {
		t.Fatalf("recorded %d segments in 2s of 20ms segments, want most of them", len(got))
	}
	if mints == 0 {
		t.Errorf("the playlist URL was never refreshed")
	}
	for i := 1; i < len(got); i++ {
		if got[i] != got[i-1]+1 {
			t.Fatalf("gap in the recording: %d follows %d (recorded %v)", got[i], got[i-1], got)
		}
	}
}

// TestExpiredGrantWithoutRefreshStops is the control: the same Origin, with no
// TTL declared, records only what the first grant lived long enough to serve.
// This is what the refresh is buying.
func TestExpiredGrantWithoutRefreshStops(t *testing.T) {
	o := newGrantOrigin(t, 20*time.Millisecond, 300*time.Millisecond, 4)
	sink, _ := runGrantIngest(t, o, 0, 2*time.Second)

	if got := len(sequences(t, sink)); got > 25 {
		t.Errorf("recorded %d segments on a grant that dies after 300ms; the ingest should have stopped", got)
	}
}

// TestRefreshDedupsBySequenceNotURL pins the identity a refresh depends on. A
// re-signed playlist re-publishes the same content under URLs that share no
// string with the old ones, so a Sink would receive every segment of the live
// window twice if the URL were what "already downloaded" meant.
func TestRefreshDedupsBySequenceNotURL(t *testing.T) {
	o := newGrantOrigin(t, 20*time.Millisecond, time.Second, 8)
	sink, _ := runGrantIngest(t, o, 100*time.Millisecond, time.Second)

	got := sequences(t, sink)
	seen := map[int]bool{}
	for _, seq := range got {
		if seen[seq] {
			t.Fatalf("segment %d was recorded twice (recorded %v)", seq, got)
		}
		seen[seq] = true
	}
	if len(got) < 20 {
		t.Fatalf("recorded %d segments, want a run long enough to cross several refreshes", len(got))
	}
}

// TestRefreshFailureKeepsTheCurrentURL covers a refresh that cannot be served:
// the grant in hand may have life left in it, so the ingest carries on with it
// rather than treating a failed re-resolve as the end of the Broadcast.
func TestRefreshFailureKeepsTheCurrentURL(t *testing.T) {
	o := newGrantOrigin(t, 20*time.Millisecond, 10*time.Second, 4)
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()

	sink := &capture{}
	_ = Run(ctx, Config{
		URL:          o.mint(),
		PollInterval: 10 * time.Millisecond,
		Retries:      2,
		URLTTL:       50 * time.Millisecond,
		RefreshURL: func(context.Context) (string, error) {
			return "", fmt.Errorf("cloud is unreachable")
		},
	}, o.srv.Client(), slog.New(slog.DiscardHandler), sink)

	if got := len(sink.bodies()); got < 10 {
		t.Errorf("recorded %d segments, want the ingest to have carried on with the URL it had", got)
	}
}

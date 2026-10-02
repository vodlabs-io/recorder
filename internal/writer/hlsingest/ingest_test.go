package hlsingest

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// capture is a Sink that records what an ingest handed it, in order. It stands
// in for both real Sinks: what they do with the bytes differs, that they
// receive them in playlist order does not.
type capture struct {
	mu    sync.Mutex
	inits []string  // map URIs, in the order they arrived
	segs  []string  // segment bodies, in the order they arrived
	meta  []Segment // the metadata alongside each body

	// rejectSegment, when set, decides whether a Segment is refused.
	rejectSegment func(s Segment) error

	finished  bool
	finishSaw error
}

func (c *capture) Init(m Map, body []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.inits = append(c.inits, m.URI)
	return nil
}

func (c *capture) Segment(s Segment, body []byte) error {
	if c.rejectSegment != nil {
		if err := c.rejectSegment(s); err != nil {
			return err
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.segs = append(c.segs, string(body))
	c.meta = append(c.meta, s)
	return nil
}

func (c *capture) Finish(err error) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.finished = true
	c.finishSaw = err
	return nil
}

func (c *capture) bodies() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.segs...)
}

// liveOrigin serves a two-segment playlist that closes once closeAfter polls
// have been served, and answers segment requests with their own path so a
// captured body can be traced back to the segment it came from.
func liveOrigin(t *testing.T, closeAfter int32, segment http.HandlerFunc) *httptest.Server {
	t.Helper()
	var polls atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/live/media.m3u8", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:2\n#EXT-X-MEDIA-SEQUENCE:0\n")
		fmt.Fprint(w, "#EXTINF:2.000,\na0.ts\n#EXTINF:2.000,\na1.ts\n")
		if polls.Add(1) >= closeAfter {
			fmt.Fprint(w, "#EXT-X-ENDLIST\n")
		}
	})
	mux.HandleFunc("/live/", segment)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func ingestOrigin(t *testing.T, srv *httptest.Server, sink Sink, retries int) error {
	t.Helper()
	return ingestOriginAt(t, srv, "/live/media.m3u8", sink, retries)
}

func ingestOriginAt(t *testing.T, srv *httptest.Server, path string, sink Sink, retries int) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return Run(ctx, Config{
		URL:          srv.URL + path,
		PollInterval: 10 * time.Millisecond,
		Retries:      retries,
	}, srv.Client(), slog.New(slog.DiscardHandler), sink)
}

func assertBodies(t *testing.T, got []string, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("handed over %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("segment[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestTransientPollFailureKeepsIngesting pins that a playlist poll which fails
// once does not end the ingest. Origins drop a poll routinely — a brief 5xx, a
// connection closed mid-body — and an ingest that ends is one the Session has
// to restart, which the dashboard reports as lagging.
func TestTransientPollFailureKeepsIngesting(t *testing.T) {
	var polls atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/live/media.m3u8", func(w http.ResponseWriter, r *http.Request) {
		switch polls.Add(1) {
		case 1, 2: // the media-playlist resolve, then the first ingest poll
			fmt.Fprint(w, "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:2\n#EXT-X-MEDIA-SEQUENCE:0\n")
			fmt.Fprint(w, "#EXTINF:2.000,\na0.ts\n")
		case 3:
			http.Error(w, "try again", http.StatusServiceUnavailable)
		default:
			fmt.Fprint(w, "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:2\n#EXT-X-MEDIA-SEQUENCE:0\n")
			fmt.Fprint(w, "#EXTINF:2.000,\na0.ts\n#EXTINF:2.000,\na1.ts\n#EXT-X-ENDLIST\n")
		}
	})
	mux.HandleFunc("/live/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "TSDATA:%s", r.URL.Path)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	sink := &capture{}
	if err := ingestOrigin(t, srv, sink, 3); err != nil {
		t.Fatalf("Run: a single failed poll must not end the ingest: %v", err)
	}
	assertBodies(t, sink.bodies(), "TSDATA:/live/a0.ts", "TSDATA:/live/a1.ts")
	if !sink.finished {
		t.Error("Finish must be called on a clean end of stream")
	}
	if sink.finishSaw != nil {
		t.Errorf("Finish saw %v, want nil on a clean end", sink.finishSaw)
	}
}

// TestSustainedPollFailureEndsIngest is the other half of the budget: once the
// retries are spent the ingest ends, so a Broadcast that really has gone away
// is not held open forever.
func TestSustainedPollFailureEndsIngest(t *testing.T) {
	var polls atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/live/media.m3u8", func(w http.ResponseWriter, r *http.Request) {
		if polls.Add(1) > 2 {
			http.Error(w, "gone", http.StatusNotFound)
			return
		}
		fmt.Fprint(w, "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:2\n#EXT-X-MEDIA-SEQUENCE:0\n")
		fmt.Fprint(w, "#EXTINF:2.000,\na0.ts\n")
	})
	mux.HandleFunc("/live/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "TSDATA:%s", r.URL.Path)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	sink := &capture{}
	err := ingestOrigin(t, srv, sink, 2)
	if err == nil {
		t.Fatal("Run: expected the exhausted retry budget to surface an error")
	}
	if !strings.Contains(err.Error(), "3 times") {
		t.Errorf("error should report the attempts spent, got %v", err)
	}
	if !sink.finished || sink.finishSaw == nil {
		t.Error("Finish must be called with the terminating error")
	}
}

// TestFailedSegmentIsRetriedInOrder pins that a segment the Origin fails to
// serve is asked for again rather than lost. Segments are handed over in
// playlist order, so a failure cannot be skipped and collected later — the poll
// stops there and resumes, and the next answer lands in the right place.
func TestFailedSegmentIsRetriedInOrder(t *testing.T) {
	var a0Requests atomic.Int32
	srv := liveOrigin(t, 3, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "a0.ts") && a0Requests.Add(1) == 1 {
			http.Error(w, "try again", http.StatusServiceUnavailable)
			return
		}
		fmt.Fprintf(w, "TSDATA:%s", r.URL.Path)
	})

	sink := &capture{}
	if err := ingestOrigin(t, srv, sink, 1); err != nil {
		t.Fatalf("Run: %v", err)
	}
	assertBodies(t, sink.bodies(), "TSDATA:/live/a0.ts", "TSDATA:/live/a1.ts")
}

// TestUnservableSegmentIsDroppedAfterBudget is the other half: a segment the
// Origin never serves must not hold the rest of the Broadcast behind it.
func TestUnservableSegmentIsDroppedAfterBudget(t *testing.T) {
	srv := liveOrigin(t, 12, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "a0.ts") {
			http.Error(w, "gone", http.StatusNotFound)
			return
		}
		fmt.Fprintf(w, "TSDATA:%s", r.URL.Path)
	})

	sink := &capture{}
	if err := ingestOrigin(t, srv, sink, 1); err != nil {
		t.Fatalf("Run: %v", err)
	}
	assertBodies(t, sink.bodies(), "TSDATA:/live/a1.ts")
}

// TestSegmentSequenceCountsHandovers pins that Sequence numbers what the Sink
// actually received, so a Sink naming files by it produces no gaps when the
// Origin drops a segment.
func TestSegmentSequenceCountsHandovers(t *testing.T) {
	srv := liveOrigin(t, 12, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "a0.ts") {
			http.Error(w, "gone", http.StatusNotFound)
			return
		}
		fmt.Fprintf(w, "TSDATA:%s", r.URL.Path)
	})

	sink := &capture{}
	if err := ingestOrigin(t, srv, sink, 1); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(sink.meta) != 1 {
		t.Fatalf("handed over %d segments, want 1", len(sink.meta))
	}
	if sink.meta[0].Sequence != 0 {
		t.Errorf("Sequence = %d, want 0: the dropped segment was never handed over",
			sink.meta[0].Sequence)
	}
}

// TestFailedInitMapHoldsItsSegmentsBack pins that a fragment is never handed
// over ahead of the init map it cannot be decoded without.
func TestFailedInitMapHoldsItsSegmentsBack(t *testing.T) {
	var mapRequests atomic.Int32
	var polls atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/live/media.m3u8", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "#EXTM3U\n#EXT-X-VERSION:6\n#EXT-X-TARGETDURATION:2\n#EXT-X-MEDIA-SEQUENCE:0\n")
		fmt.Fprint(w, "#EXT-X-MAP:URI=\"init.mp4\"\n")
		fmt.Fprint(w, "#EXTINF:2.000,\na0.m4s\n")
		if polls.Add(1) >= 4 {
			fmt.Fprint(w, "#EXT-X-ENDLIST\n")
		}
	})
	mux.HandleFunc("/live/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "init.mp4") && mapRequests.Add(1) <= 1 {
			http.Error(w, "try again", http.StatusServiceUnavailable)
			return
		}
		fmt.Fprintf(w, "DATA:%s", r.URL.Path)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	sink := &capture{}
	if err := ingestOrigin(t, srv, sink, 1); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(sink.inits) != 1 || !strings.HasSuffix(sink.inits[0], "/live/init.mp4") {
		t.Fatalf("inits = %v, want the map handed over exactly once", sink.inits)
	}
	assertBodies(t, sink.bodies(), "DATA:/live/a0.m4s")
	if len(sink.meta) > 0 && !sink.meta[0].IsFMP4 {
		t.Error("a segment behind an EXT-X-MAP must be marked IsFMP4")
	}
}

// TestUnreachableInitMapEndsIngest is the give-up path: nothing behind a map
// that never arrives can be used, so the ingest ends rather than handing over
// fragments nothing initialises.
func TestUnreachableInitMapEndsIngest(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/live/media.m3u8", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "#EXTM3U\n#EXT-X-VERSION:6\n#EXT-X-TARGETDURATION:2\n#EXT-X-MEDIA-SEQUENCE:0\n")
		fmt.Fprint(w, "#EXT-X-MAP:URI=\"init.mp4\"\n#EXTINF:2.000,\na0.m4s\n")
	})
	mux.HandleFunc("/live/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "init.mp4") {
			http.Error(w, "gone", http.StatusNotFound)
			return
		}
		fmt.Fprintf(w, "DATA:%s", r.URL.Path)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	sink := &capture{}
	if err := ingestOrigin(t, srv, sink, 1); err == nil {
		t.Fatal("Run: expected an unreachable init map to end the ingest")
	}
	if len(sink.segs) != 0 {
		t.Errorf("handed over %v; no fragment may go over without its map", sink.segs)
	}
}

// TestSinkRejectionStopsAndResumes pins that a Sink refusing a Segment gets the
// same stop-and-resume the Origin gets: the segment is offered again on the
// next poll rather than lost, and the ones behind it do not overtake it.
func TestSinkRejectionStopsAndResumes(t *testing.T) {
	srv := liveOrigin(t, 4, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "TSDATA:%s", r.URL.Path)
	})

	var rejections atomic.Int32
	sink := &capture{
		rejectSegment: func(s Segment) error {
			if strings.HasSuffix(s.URI, "a0.ts") && rejections.Add(1) == 1 {
				return errors.New("not ready")
			}
			return nil
		},
	}
	if err := ingestOrigin(t, srv, sink, 1); err != nil {
		t.Fatalf("Run: one rejection must not end the ingest: %v", err)
	}
	assertBodies(t, sink.bodies(), "TSDATA:/live/a0.ts", "TSDATA:/live/a1.ts")
}

// TestSinkFailureSurfacesAsSinkError pins the taxonomy the callers rely on: a
// Sink that keeps refusing ends the ingest, and the error says it was ours
// rather than the Origin's.
func TestSinkFailureSurfacesAsSinkError(t *testing.T) {
	srv := liveOrigin(t, 20, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "TSDATA:%s", r.URL.Path)
	})

	sentinel := errors.New("disk full")
	sink := &capture{rejectSegment: func(s Segment) error { return sentinel }}
	err := ingestOrigin(t, srv, sink, 1)
	if err == nil {
		t.Fatal("Run: expected a persistently failing Sink to end the ingest")
	}
	var se *SinkError
	if !errors.As(err, &se) {
		t.Fatalf("error must be a SinkError so the caller can tell it from an Origin failure, got %T: %v", err, err)
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("SinkError must unwrap to the Sink's own error, got %v", err)
	}
}

// TestOriginSessionCookieIsKept pins that a cookie the playlist response sets
// is sent with the segment requests behind it. TwitCasting answers 401 to a
// segment fetched without the session cookie its playlist hands out.
func TestOriginSessionCookieIsKept(t *testing.T) {
	srv := liveOrigin(t, 2, func(w http.ResponseWriter, r *http.Request) {
		if c, err := r.Cookie("ssid"); err != nil || c.Value != "s1" {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		fmt.Fprintf(w, "TSDATA:%s", r.URL.Path)
	})
	srv.Config.Handler = cookieSetter(srv.Config.Handler)

	sink := &capture{}
	if err := ingestOrigin(t, srv, sink, 1); err != nil {
		t.Fatalf("Run: %v", err)
	}
	assertBodies(t, sink.bodies(), "TSDATA:/live/a0.ts", "TSDATA:/live/a1.ts")
}

// TestCookieJarIsPerIngest pins that one ingest's cookies never reach another's
// requests, though both share a client.
func TestCookieJarIsPerIngest(t *testing.T) {
	shared := &http.Client{}
	a, b := withCookieJar(shared), withCookieJar(shared)
	if a.Jar == nil || b.Jar == nil || a.Jar == b.Jar {
		t.Fatal("each ingest should get a jar of its own")
	}
	if shared.Jar != nil {
		t.Error("the shared client must be left without a jar")
	}
}

// cookieSetter sets the session cookie on every playlist response.
func cookieSetter(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".m3u8") {
			http.SetCookie(w, &http.Cookie{Name: "ssid", Value: "s1", Path: "/live/"})
		}
		next.ServeHTTP(w, r)
	})
}

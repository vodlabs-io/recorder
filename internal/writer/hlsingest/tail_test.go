package hlsingest

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestTailForgetsOldest(t *testing.T) {
	tail := NewTail([]string{"a", "b"})
	for i := range tailSize {
		tail.Add(fmt.Sprint(i))
	}
	if tail.Has("a") || tail.Has("b") {
		t.Fatal("the oldest hashes should be forgotten past tailSize")
	}
	if !tail.Has(fmt.Sprint(tailSize-1)) || len(tail.Hashes()) != tailSize {
		t.Fatalf("tail holds %d hashes", len(tail.Hashes()))
	}
	var none *Tail
	if none.Has("a") || none.Hashes() != nil {
		t.Fatal("a nil Tail remembers nothing")
	}
	none.Add("a") // must not panic
}

// windowOrigin serves a closed playlist of the named segments, each answering
// with its own name as its body.
func windowOrigin(t *testing.T, names ...string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/live/media.m3u8", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:2\n#EXT-X-MEDIA-SEQUENCE:0\n")
		for _, n := range names {
			fmt.Fprintf(w, "#EXTINF:2.000,\n%s\n", n)
		}
		fmt.Fprint(w, "#EXT-X-ENDLIST\n")
	})
	mux.HandleFunc("/live/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, r.URL.Path[len("/live/"):])
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func ingestWithTail(t *testing.T, srv *httptest.Server, tail *Tail) []string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	sink := &capture{}
	if err := Run(ctx, Config{
		URL:          srv.URL + "/live/media.m3u8",
		PollInterval: 10 * time.Millisecond,
		Retries:      1,
		Tail:         tail,
	}, srv.Client(), slog.New(slog.DiscardHandler), sink); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	return sink.bodies()
}

// A part that starts at the live edge after a reconnect or a restart picks up
// where the part before it stopped, rather than recording its last segments
// again.
func TestTailSkipsWhatThePartBeforeRecorded(t *testing.T) {
	tail := NewTail(nil)
	assertBodies(t, ingestWithTail(t, windowOrigin(t, "s1.ts", "s2.ts", "s3.ts"), tail),
		"s1.ts", "s2.ts", "s3.ts")

	// The next part's window still publishes s2 and s3.
	assertBodies(t, ingestWithTail(t, windowOrigin(t, "s2.ts", "s3.ts", "s4.ts", "s5.ts"), tail),
		"s4.ts", "s5.ts")

	// Seeded from a checkpoint, as after a restart.
	restarted := NewTail(tail.Hashes())
	assertBodies(t, ingestWithTail(t, windowOrigin(t, "s4.ts", "s5.ts", "s6.ts"), restarted),
		"s6.ts")
}

// Only the start of a part is matched: once a new segment is recorded,
// everything after it is recorded too.
func TestTailOnlyTrimsTheStartOfAPart(t *testing.T) {
	tail := NewTail([]string{SegmentHash([]byte("s1.ts")), SegmentHash([]byte("s3.ts"))})
	assertBodies(t, ingestWithTail(t, windowOrigin(t, "s1.ts", "s2.ts", "s3.ts"), tail),
		"s2.ts", "s3.ts")
}

func TestNoTailRecordsEverything(t *testing.T) {
	assertBodies(t, ingestWithTail(t, windowOrigin(t, "s1.ts", "s2.ts"), nil), "s1.ts", "s2.ts")
}

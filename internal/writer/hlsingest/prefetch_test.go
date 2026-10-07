package hlsingest

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestSlowOriginIsReadAtItsOwnPace models a far-away Origin: a sliding
// four-segment window that advances one segment per tick, and segments that
// each take three ticks to arrive. Read one at a time, the ingest falls a
// segment further behind with every one it takes and the window slides past
// the rest; overlapping the downloads keeps every segment, in order.
func TestSlowOriginIsReadAtItsOwnPace(t *testing.T) {
	const (
		tick    = 100 * time.Millisecond
		window  = 4
		total   = 24
		latency = 3 * tick
	)
	start := time.Now()
	head := func() int { return min(int(time.Since(start)/tick), total) }

	var inFlight, peak atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/live/media.m3u8", func(w http.ResponseWriter, r *http.Request) {
		h := head()
		first := max(0, h-window+1)
		var b strings.Builder
		fmt.Fprintf(&b, "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:1\n#EXT-X-MEDIA-SEQUENCE:%d\n", first)
		for i := first; i <= h; i++ {
			fmt.Fprintf(&b, "#EXTINF:0.100,\ns%d.ts\n", i)
		}
		if h == total {
			b.WriteString("#EXT-X-ENDLIST\n")
		}
		fmt.Fprint(w, b.String())
	})
	mux.HandleFunc("/live/", func(w http.ResponseWriter, r *http.Request) {
		n := inFlight.Add(1)
		defer inFlight.Add(-1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		time.Sleep(latency)
		fmt.Fprint(w, strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/live/"), ".ts"))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	sink := &capture{}
	err := Run(ctx, Config{
		URL:            srv.URL + "/live/media.m3u8",
		PollInterval:   tick,
		Retries:        3,
		LiveEdgeWindow: time.Hour, // adopt the whole first window
	}, srv.Client(), slog.New(slog.DiscardHandler), sink)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	got := sink.bodies()
	for i := 1; i < len(got); i++ {
		prev, _ := strconv.Atoi(strings.TrimPrefix(got[i-1], "s"))
		cur, _ := strconv.Atoi(strings.TrimPrefix(got[i], "s"))
		if cur != prev+1 {
			t.Errorf("handed over %s after %s: the window slid past what was in between", got[i], got[i-1])
		}
	}
	if len(got) < total-window {
		t.Errorf("handed over %d of %d segments", len(got), total+1)
	}
	if peak.Load() < 2 {
		t.Errorf("at most %d download at once, want them overlapped", peak.Load())
	}
	if peak.Load() > maxParallelFetches {
		t.Errorf("%d downloads at once, want at most %d", peak.Load(), maxParallelFetches)
	}
}

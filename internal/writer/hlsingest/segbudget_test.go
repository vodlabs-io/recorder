package hlsingest

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// TestSegRetryCountsAgainstTheSequence pins that the poll budget follows the
// segment rather than the URL naming it. A refreshable Origin re-signs every
// URL it publishes, so a URL-keyed budget starts over at each refresh and
// bounds nothing.
func TestSegRetryCountsAgainstTheSequence(t *testing.T) {
	var r segRetry

	if got := r.spend(41); got != 1 {
		t.Errorf("first poll on a segment = %d, want 1", got)
	}
	// The same content, re-signed: a new URL, the same media sequence.
	if got := r.spend(41); got != 2 {
		t.Errorf("second poll on the same sequence = %d, want 2 — a re-signed URL must not reset the budget", got)
	}
	if got := r.spend(42); got != 1 {
		t.Errorf("first poll on the next sequence = %d, want 1", got)
	}
	r.clear()
	if got := r.spend(42); got != 1 {
		t.Errorf("poll after clear = %d, want 1", got)
	}
	// Sequence zero is a real sequence, not "nothing held".
	if got := r.spend(0); got != 1 {
		t.Errorf("first poll on sequence 0 = %d, want 1", got)
	}
	if got := r.spend(0); got != 2 {
		t.Errorf("second poll on sequence 0 = %d, want 2", got)
	}
}

// TestSegmentFetchBudgetTracksTheSlideRate pins the sizing rule: attempts at
// one segment get less time than the Origin takes to drop one, since a retry
// can only reach a segment still published. A window deep enough to keep a
// segment regardless lifts the bound entirely.
func TestSegmentFetchBudgetTracksTheSlideRate(t *testing.T) {
	tests := []struct {
		name    string
		window  time.Duration
		segment time.Duration
		want    time.Duration
	}{
		{"nothing measured yet", 0, 0, 0},
		{"douyin: 3 x 4s", 12 * time.Second, 4 * time.Second, 2 * time.Second},
		{"segments too short to halve", 6 * time.Second, 1 * time.Second, minSegmentFetchBudget},
		{"a DVR window is unbounded", 2 * time.Hour, time.Second, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := segmentFetchBudget(tt.window, tt.segment); got != tt.want {
				t.Errorf("segmentFetchBudget(%v, %v) = %v, want %v",
					tt.window, tt.segment, got, tt.want)
			}
		})
	}
}

// TestFetchRetryStopsWithinItsBudget pins what makes segmentPollRetries worth
// anything on a short-window Origin: attempts at a segment that answers slowly
// have to give up in time for the next poll to still find that segment
// published. Unbudgeted, the attempts outlast the window, every poll lands on a
// newly published segment, and the poll budget never counts past its first.
func TestFetchRetryStopsWithinItsBudget(t *testing.T) {
	const answerDelay = 150 * time.Millisecond
	const budget = 600 * time.Millisecond

	var requests int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		time.Sleep(answerDelay)
		http.Error(w, "not ready", http.StatusNotFound)
	}))
	defer srv.Close()

	o := testIngest(t, srv, Config{URL: srv.URL, Retries: 5})

	start := time.Now()
	if _, err := o.fetchRetry(context.Background(), srv.URL, budget); err == nil {
		t.Fatal("fetchRetry: expected the failure to be reported")
	}
	spent := time.Since(start)

	// One attempt over the budget is inherent — the budget is checked before a
	// pause, not mid-request — so the bound is the budget plus one answer.
	if limit := budget + answerDelay + 100*time.Millisecond; spent > limit {
		t.Errorf("spent %v on a failing segment, want at most %v: a failure that "+
			"outlasts the window is one the next poll cannot retry", spent, limit)
	}
	if requests < 2 {
		t.Errorf("made %d attempts, want at least 2: the budget must not cost the retry itself", requests)
	}
	if requests >= 5 {
		t.Errorf("made %d attempts, want fewer than the %d configured: the budget was not applied", requests, 5)
	}
}

// TestFetchRetryWithoutBudgetSpendsEveryAttempt pins that an Origin publishing
// a window wide enough to forgive a slow failure still gets every attempt.
func TestFetchRetryWithoutBudgetSpendsEveryAttempt(t *testing.T) {
	var requests int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		http.Error(w, "not ready", http.StatusNotFound)
	}))
	defer srv.Close()

	o := testIngest(t, srv, Config{URL: srv.URL, Retries: 3})
	if _, err := o.fetchRetry(context.Background(), srv.URL, 0); err == nil {
		t.Fatal("fetchRetry: expected the failure to be reported")
	}
	if requests != 3 {
		t.Errorf("made %d attempts, want the 3 configured", requests)
	}
}

// TestSlowSegmentIsRetriedBeforeItsWindowSlides models an Origin with a
// three-segment window that answers the first requests for any segment slowly
// and uselessly, sliding while those answers are waited on. Attempts at one
// segment must cost less than the Origin takes to drop it, or the failed
// segment is never asked for again and the Session records nothing.
func TestSlowSegmentIsRetriedBeforeItsWindowSlides(t *testing.T) {
	// Long enough that a fetch budget has to cut the attempts short.
	const answerDelay = 500 * time.Millisecond
	// Useless answers before a segment finally serves. More than one fetch's
	// worth of attempts, so recording anything requires coming back to the same
	// segment on a later poll.
	const uselessAnswers = 3
	// Segments published at once. The window slides one segment per segment
	// request, so a fetch spending more attempts than this loses what it is
	// fetching.
	const windowSegments = 3

	var mu sync.Mutex
	requests := map[string]int{}
	slid := 0

	mux := http.NewServeMux()
	mux.HandleFunc("/live/media.m3u8", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seq := slid
		mu.Unlock()
		fmt.Fprintf(w, "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:2\n#EXT-X-MEDIA-SEQUENCE:%d\n", seq)
		for i := seq; i < seq+windowSegments; i++ {
			fmt.Fprintf(w, "#EXTINF:2.000,\na%d.ts\n", i)
		}
	})
	mux.HandleFunc("/live/", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests[r.URL.Path]++
		n := requests[r.URL.Path]
		slid++ // the Origin moves on while this request is being answered
		mu.Unlock()
		if n <= uselessAnswers {
			time.Sleep(answerDelay)
			http.Error(w, "not ready", http.StatusNotFound)
			return
		}
		fmt.Fprintf(w, "TSDATA:%s", r.URL.Path)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	sink := &capture{}
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Config{
			URL:          srv.URL + "/live/media.m3u8",
			PollInterval: 10 * time.Millisecond,
			Retries:      3,
			// Start at the live edge, so each segment is met with its whole
			// window ahead of it rather than partway through.
			LiveEdgeWindow: time.Millisecond,
		}, srv.Client(), slog.New(slog.DiscardHandler), sink)
	}()

	deadline := time.After(20 * time.Second)
	for len(sink.bodies()) == 0 {
		select {
		case err := <-done:
			cancel()
			t.Fatalf("ingest ended having recorded nothing (%v)", err)
		case <-deadline:
			mu.Lock()
			asked, most := len(requests), 0
			for _, n := range requests {
				most = max(most, n)
			}
			mu.Unlock()
			cancel()
			<-done
			t.Fatalf("recorded nothing: asked for %d segments and never more than %d "+
				"times for any one of them, so none got past its %d useless answers "+
				"— every retry landed on a newly published segment instead of the "+
				"one that failed", asked, most, uselessAnswers)
		case <-time.After(20 * time.Millisecond):
		}
	}
	cancel()
	<-done
}

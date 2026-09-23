package cloud

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nkeys"

	"github.com/vodlabs-io/recorder/internal/streamer"
	"github.com/vodlabs-io/recorder/internal/vlconfig"
	"github.com/vodlabs-io/recorder/internal/vllog"
)

// The query flags are the whole contract between a request and what the cloud
// does with it: streams=1 asks it to skip a status check's bookkeeping, and
// recheck=1 asks it to un-retire a streamer. Sending the wrong one for a
// re-sign would have a running recording judged on a single lookup.
func TestStatusQueryFlags(t *testing.T) {
	var (
		mu       sync.Mutex
		lastQS   string
		lastPath string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		lastQS, lastPath = r.URL.RawQuery, r.URL.Path
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(streamersResponse{
			Streamers: streamer.List{{
				StreamerRef: streamer.StreamerRef{
					StreamerKey: streamer.StreamerKey{Platform: "youtube", Username: "alice"},
				},
				Status: streamer.StatusLive,
			}},
			AsOf: time.Now().Unix(),
		})
	}))
	defer srv.Close()

	orig := apiBaseURL
	apiBaseURL = srv.URL
	t.Cleanup(func() { apiBaseURL = orig })

	logger, err := vllog.NewFileLogger(vlconfig.LoggerConfig{Mode: "off", Level: "error"}, "test")
	if err != nil {
		t.Fatalf("logger: %v", err)
	}
	kp, _ := nkeys.CreateUser()
	seed, _ := kp.Seed()
	c := &Client{logger: logger, httpClient: srv.Client(), nkeySeed: string(seed)}

	id := streamer.StreamerRef{
		StreamerKey: streamer.StreamerKey{Platform: "youtube", Username: "alice"},
	}

	for _, tc := range []struct {
		name string
		opts statusOpts
		want string
	}{
		{"plainCheck", statusOpts{}, ""},
		{"authedCheck", statusOpts{authed: true}, "auth=1"},
		{"userRecheck", statusOpts{recheck: true}, "recheck=1"},
		{"reSign", statusOpts{streamsOnly: true}, "streams=1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := c.fetchStatus(t.Context(), tc.opts, id); err != nil {
				t.Fatalf("fetchStatus: %v", err)
			}
			mu.Lock()
			gotQS, gotPath := lastQS, lastPath
			mu.Unlock()
			if gotQS != tc.want {
				t.Errorf("query = %q, want %q", gotQS, tc.want)
			}
			if gotPath != "/api/v1/streamers/youtube/alice" {
				t.Errorf("path = %q, want the single-streamer route", gotPath)
			}
		})
	}

	// A whole-list fetch names no streamer, so the flags have nothing to apply
	// to and must not leak onto it.
	t.Run("listFetchCarriesNoFlags", func(t *testing.T) {
		if _, err := c.fetchStatus(t.Context(), statusOpts{streamsOnly: true}); err != nil {
			t.Fatalf("fetchStatus: %v", err)
		}
		mu.Lock()
		gotQS, gotPath := lastQS, lastPath
		mu.Unlock()
		if gotQS != "" || gotPath != "/api/v1/streamers" {
			t.Errorf("list fetch hit %q?%q, want the bare list route", gotPath, gotQS)
		}
	})
}

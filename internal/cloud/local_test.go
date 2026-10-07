package cloud

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/vodlabs-io/recorder/internal/streamer"
	"github.com/vodlabs-io/recorder/internal/vlstate"
)

type fakeLocal struct {
	mu    sync.Mutex
	fresh []bool
}

func (f *fakeLocal) Check(_ context.Context, key streamer.StreamerKey, fresh bool) (streamer.StreamerState, error) {
	f.mu.Lock()
	f.fresh = append(f.fresh, fresh)
	f.mu.Unlock()
	return streamer.StreamerState{StreamerRef: streamer.StreamerRef{StreamerKey: key}, Status: streamer.StatusOffline}, nil
}

// A local streamer is subscribed without the cloud hearing of it.
func TestSubscriptionKeepsLocalStreamersFromTheCloud(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("the cloud was asked %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	prev := apiBaseURL
	apiBaseURL = srv.URL
	defer func() { apiBaseURL = prev }()

	c, _ := diffClient(t, "[streamers]\ngeneric = [\"example.com-0123456789\"]\n")
	_, kv := newTestKV(t)
	c.kv, c.store = kv, vlstate.NewStore(kv)

	if err := c.newSubscription(context.Background()); err != nil {
		t.Fatalf("newSubscription: %v", err)
	}
	want := streamer.StreamerRef{StreamerKey: streamer.StreamerKey{Platform: "generic", Username: "example.com-0123456789"}}
	if !c.IsSubscribed(want) {
		t.Fatalf("subscriptions = %v, want the local streamer", c.subscriptions.Load())
	}
	c.mu.RLock()
	subscribed := c.isSubscribed
	c.mu.RUnlock()
	if subscribed {
		t.Fatal("a local-only subscription marked the cloud connection subscribed")
	}
}

// Local checks run through the platform's lanes with no cloud subscription,
// and a re-sign asks for a fresh reading.
func TestLocalChecksRunWithoutTheCloud(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c, _ := diffClient(t, "")
	_, kv := newTestKV(t)
	c.kv, c.store = kv, vlstate.NewStore(kv)
	local := &fakeLocal{}
	c.local = local
	c.ceiling = newTokenBucket(ctx, ceilingLimit)

	id := streamer.StreamerRef{StreamerKey: streamer.StreamerKey{Platform: "generic", Username: "example.com-0123456789"}}
	l := c.limiterFor(ctx, "generic")
	l.highPri <- id
	l.stream <- id
	for i := 0; i < 2; i++ {
		select {
		case str := <-c.StreamerCh:
			if str.StreamerRef != id {
				t.Fatalf("dispatched %v", str.StreamerRef)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("a local check waited for a cloud subscription")
		}
	}
	local.mu.Lock()
	defer local.mu.Unlock()
	if len(local.fresh) != 2 || local.fresh[0] == local.fresh[1] {
		t.Fatalf("fresh = %v, want one status check and one re-sign", local.fresh)
	}
}

func TestSplitLocal(t *testing.T) {
	g := streamer.StreamerRef{StreamerKey: streamer.StreamerKey{Platform: "generic", Username: "a.com-1"}}
	tw := streamer.StreamerRef{StreamerKey: streamer.StreamerKey{Platform: "twitch", Username: "x"}}
	u := streamer.StreamerRef{URL: "https://example.com/live"}
	local, remote := splitLocal(streamer.StreamerRefs{g, tw, u, g})
	if len(local) != 1 || local[0] != g || len(remote) != 2 {
		t.Fatalf("local = %v, remote = %v", local, remote)
	}
}

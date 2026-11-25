package cloud

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/nats-io/nkeys"

	"github.com/vodlabs-io/recorder/internal/streamer"
	"github.com/vodlabs-io/recorder/internal/vlstate"
)

// TestUnaskedEntryFindsLostAdd covers an add written after a cycle read the
// file, whose trigger was lost: the ticker must notice it.
func TestUnaskedEntryFindsLostAdd(t *testing.T) {
	c, _ := diffClient(t, "[streamers]\ntwitch = [\"a\"]\n")
	if err := c.streamersFile.Load(); err != nil {
		t.Fatalf("load: %v", err)
	}
	c.askedIDs.Store(streamer.StreamerRefs{ref("twitch", "a")})
	c.subscriptions.Store(streamer.StreamerRefs{ref("twitch", "a")})

	if id, ok := c.unaskedEntry(); ok {
		t.Fatalf("unaskedEntry = %v with nothing added", id)
	}

	// What streamer.add does: the in-memory file first, then the disk.
	c.streamersFile.AddID(ref("kwai", "150000061240802"))
	id, ok := c.unaskedEntry()
	if !ok || id.String() != ref("kwai", "150000061240802").String() {
		t.Fatalf("unaskedEntry = %v, %v; want the added kwai entry", id, ok)
	}
}

// TestUnaskedEntryIgnoresRefused runs a real cycle the cloud answers by
// refusing the only entry as over capacity. The entry stays in the file but
// was asked for, so the ticker must not resubscribe for it every 10 s.
func TestUnaskedEntryIgnoresRefused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(subscribeResponse{
			UserJWT:   "jwt",
			ExpiresAt: time.Now().Add(time.Hour).Unix(),
			Rejected: []subscribeRejection{
				{Platform: "twitch", Username: "full", Error: codeOverCapacity},
			},
		})
	}))
	defer srv.Close()
	prevAPI := apiBaseURL
	apiBaseURL = srv.URL
	defer func() { apiBaseURL = prevAPI }()

	c, _ := diffClient(t, "[streamers]\ntwitch = [\"full\"]\n")
	_, kv := newTestKV(t)
	c.kv, c.store = kv, vlstate.NewStore(kv)
	c.httpClient = srv.Client()
	kp, _ := nkeys.CreateUser()
	seed, _ := kp.Seed()
	c.nkeySeed = string(seed)

	if err := c.newSubscription(context.Background()); err != nil {
		t.Fatalf("newSubscription: %v", err)
	}
	if id, ok := c.unaskedEntry(); ok {
		t.Fatalf("unaskedEntry = %v for an entry the cloud refused", id)
	}

	c.streamersFile.AddID(ref("twitch", "later"))
	if id, ok := c.unaskedEntry(); !ok || id.String() != ref("twitch", "later").String() {
		t.Fatalf("unaskedEntry = %v, %v; want twitch.later", id, ok)
	}
}

package cloud

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nkeys"
	"github.com/vodlabs-io/recorder/internal/streamer"
	"github.com/vodlabs-io/recorder/internal/vlconfig"
	"github.com/vodlabs-io/recorder/internal/vllog"
)

// countingTransport fails every request and counts the attempts.
type countingTransport struct {
	attempts atomic.Int32
	fail     atomic.Bool
}

func (t *countingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	t.attempts.Add(1)
	return nil, context.DeadlineExceeded
}

// A daemon started while the service is unreachable used to exit. It has to
// come up and keep asking instead, or it is not running when the outage clears.
func TestInitStatusRetriesWhileTheServiceIsUnreachable(t *testing.T) {
	log, err := vllog.NewFileLogger(vlconfig.LoggerConfig{Mode: "off", Level: "error"}, "test")
	if err != nil {
		t.Fatalf("NewFileLogger: %v", err)
	}

	kp, err := nkeys.CreateUser()
	if err != nil {
		t.Fatalf("nkey: %v", err)
	}
	seed, err := kp.Seed()
	if err != nil {
		t.Fatalf("nkey seed: %v", err)
	}

	tr := &countingTransport{}
	c := &Client{
		logger:     log,
		httpClient: &http.Client{Transport: tr},
		nkeySeed:   string(seed),
	}
	c.subscriptions.Store(streamer.StreamerRefs{{
		StreamerKey: streamer.StreamerKey{Platform: "tiktok", Username: "someone"},
	}})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.initStatusRetrying(ctx, time.Millisecond, 5*time.Millisecond)

	deadline := time.After(10 * time.Second)
	for tr.attempts.Load() < 3 {
		select {
		case <-deadline:
			t.Fatalf("stopped after %d attempts; the daemon must keep retrying", tr.attempts.Load())
		case <-time.After(5 * time.Millisecond):
		}
	}

	// And it stops when the daemon does, rather than retrying forever.
	cancel()
	time.Sleep(50 * time.Millisecond)
	settled := tr.attempts.Load()
	time.Sleep(100 * time.Millisecond)
	if grew := tr.attempts.Load() - settled; grew > 1 {
		t.Fatalf("%d more attempts after shutdown; the loop must end with the context", grew)
	}
}

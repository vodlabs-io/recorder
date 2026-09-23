package cloud

import (
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	"github.com/vodlabs-io/recorder/internal/streamer"
	"github.com/vodlabs-io/recorder/internal/vlconfig"
)

// diffClient builds a Client with just the fields applySubscriptionDiff needs:
// a streamers file on disk, the status-request queue it fans out to, and the
// subscription/disabled baselines it keeps between cycles.
func diffClient(t *testing.T, streamersTOML string) (*Client, string) {
	t.Helper()
	c := testClient(t)
	path := filepath.Join(t.TempDir(), "streamers.toml")
	if err := os.WriteFile(path, []byte(streamersTOML), 0644); err != nil {
		t.Fatalf("write streamers.toml: %v", err)
	}
	sf, err := vlconfig.NewStreamersFile(path)
	if err != nil {
		t.Fatalf("NewStreamersFile: %v", err)
	}
	c.cond = sync.NewCond(&c.mu)
	c.streamersFile = sf
	c.statusReqCh = make(chan streamer.StreamerRef, 16)
	c.StatusReqPriCh = make(chan streamer.StreamerRef, 16)
	c.StreamerCh = make(chan streamer.StreamerState, 16)
	c.subscriptions.Store(streamer.StreamerRefs{})
	return c, path
}

// rewriteStreamers replaces the streamers file contents and reloads it, standing
// in for the enable/disable handlers' write + the reload newSubscription does.
func rewriteStreamers(t *testing.T, c *Client, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("rewrite streamers.toml: %v", err)
	}
	if err := c.streamersFile.Load(); err != nil {
		t.Fatalf("reload streamers.toml: %v", err)
	}
}

// drainStatusReqs collects the streamer IDs queued for a status fetch.
func drainStatusReqs(c *Client) []string {
	return drainIDs(c.statusReqCh)
}

// drainPriStatusReqs collects the streamer IDs queued for a priority fetch.
func drainPriStatusReqs(c *Client) []string {
	return drainIDs(c.StatusReqPriCh)
}

func drainIDs(ch chan streamer.StreamerRef) []string {
	var got []string
	for {
		select {
		case id := <-ch:
			got = append(got, id.String())
		default:
			return got
		}
	}
}

const threeEnabled = `[streamers]
tiktok = ['alice', 'bob', 'carol']
`

const bothRecordOff = `[streamers]
tiktok = ['alice', 'bob']

[[rules]]
record = false
tiktok = ['alice', 'bob']
`

const bothRecordOn = `[streamers]
tiktok = ['alice', 'bob']
`

// TestApplySubscriptionDiff_RecordOnAllAfterStartupOff covers streamers that
// are already disabled when the daemon starts: the first subscription cycle
// must record them as disabled, so the "Enable All" that follows is seen as a
// disabled→enabled transition and refetches their status.
func TestApplySubscriptionDiff_RecordOnAllAfterStartupOff(t *testing.T) {
	c, path := diffClient(t, bothRecordOff)
	ids := streamer.StreamerRefs{
		{StreamerKey: streamer.StreamerKey{Platform: "tiktok", Username: "alice"}},
		{StreamerKey: streamer.StreamerKey{Platform: "tiktok", Username: "bob"}},
	}

	// Startup cycle: nothing is dispatched (initialSubDone is still false), but
	// the disabled baseline must be captured.
	c.applySubscriptionDiff(ids)
	if got := drainStatusReqs(c); len(got) != 0 {
		t.Fatalf("initial cycle queued status fetches %v, want none", got)
	}
	if len(c.prevStopped) != 2 {
		t.Fatalf("prevStopped after startup = %v, want both streamers recorded as switched off", c.prevStopped)
	}
	c.markInitialSubDone()

	// "Record on all": same subscription set, the record=false rule gone.
	rewriteStreamers(t, c, path, bothRecordOn)
	c.applySubscriptionDiff(ids)

	got := drainStatusReqs(c)
	want := []string{"tiktok.alice", "tiktok.bob"}
	if len(got) != len(want) {
		t.Fatalf("after record-on-all, status fetches = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("status fetch %d = %q, want %q", i, got[i], want[i])
		}
	}
	if len(c.prevStopped) != 0 {
		t.Errorf("prevStopped after record-on-all = %v, want empty", c.prevStopped)
	}
}

// TestApplySubscriptionDiff_AddRouting pins which queue a newly subscribed
// streamer's status fetch lands in. This is also where the add path's fetch
// comes from at all: queuing one when the streamer is written to the file
// instead races the subscription POST that authorises it, and the backend
// answers a status request it has no subscription for with a 403.
func TestApplySubscriptionDiff_AddRouting(t *testing.T) {
	alice := streamer.StreamerRef{StreamerKey: streamer.StreamerKey{Platform: "tiktok", Username: "alice"}}
	bob := streamer.StreamerRef{StreamerKey: streamer.StreamerKey{Platform: "tiktok", Username: "bob"}}
	carol := streamer.StreamerRef{StreamerKey: streamer.StreamerKey{Platform: "tiktok", Username: "carol"}}

	// Both cases start from a settled one-streamer subscription, so what the
	// second cycle adds is the only difference between them.
	settled := func(t *testing.T) *Client {
		t.Helper()
		c, _ := diffClient(t, threeEnabled)
		c.applySubscriptionDiff(streamer.StreamerRefs{alice})
		c.markInitialSubDone()
		return c
	}

	t.Run("lone add jumps the queue", func(t *testing.T) {
		c := settled(t)
		c.applySubscriptionDiff(streamer.StreamerRefs{alice, bob})

		if got, want := drainPriStatusReqs(c), []string{"tiktok.bob"}; !slices.Equal(got, want) {
			t.Errorf("priority fetches = %v, want %v", got, want)
		}
		if got := drainStatusReqs(c); len(got) != 0 {
			t.Errorf("normal fetches = %v, want none", got)
		}
	})

	t.Run("batch add stays in the normal queue", func(t *testing.T) {
		c := settled(t)
		c.applySubscriptionDiff(streamer.StreamerRefs{alice, bob, carol})

		// Diff walks a map, so the two added IDs arrive in either order.
		got := drainStatusReqs(c)
		slices.Sort(got)
		if want := []string{"tiktok.bob", "tiktok.carol"}; !slices.Equal(got, want) {
			t.Errorf("normal fetches = %v, want %v", got, want)
		}
		if got := drainPriStatusReqs(c); len(got) != 0 {
			t.Errorf("priority fetches = %v, want none", got)
		}
	})
}

// TestApplySubscriptionDiff_StillDisabledNotRefetched guards the other side of
// the snapshot: a streamer that stays disabled across cycles must not be
// refetched, or every resubscribe would spam the status queue.
func TestApplySubscriptionDiff_StillDisabledNotRefetched(t *testing.T) {
	c, _ := diffClient(t, bothRecordOff)
	ids := streamer.StreamerRefs{
		{StreamerKey: streamer.StreamerKey{Platform: "tiktok", Username: "alice"}},
		{StreamerKey: streamer.StreamerKey{Platform: "tiktok", Username: "bob"}},
	}

	c.applySubscriptionDiff(ids)
	c.markInitialSubDone()
	c.applySubscriptionDiff(ids)

	if got := drainStatusReqs(c); len(got) != 0 {
		t.Errorf("still-disabled streamers queued status fetches %v, want none", got)
	}
}

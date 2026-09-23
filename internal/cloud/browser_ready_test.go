package cloud

import (
	"context"
	"testing"

	"github.com/vodlabs-io/recorder/internal/streamer"
	"github.com/vodlabs-io/recorder/internal/vlconfig"
	"github.com/vodlabs-io/recorder/internal/vllog"
)

func quietLogger(t *testing.T) *vllog.FileLogger {
	t.Helper()
	l, err := vllog.NewFileLogger(vlconfig.LoggerConfig{Mode: "off", Level: "error"}, "test")
	if err != nil {
		t.Fatalf("logger: %v", err)
	}
	return l
}

func parkRef(platform, username string) streamer.StreamerRef {
	return streamer.StreamerRef{StreamerKey: streamer.StreamerKey{Platform: platform, Username: username}}
}

// A check parked twice must be asked for once: the same streamer can fail an
// authed check on several polls before any desktop app turns up.
func TestParkedChecks_DedupesAndDrains(t *testing.T) {
	var p parkedChecks
	alice := parkRef("tiktok", "alice")

	p.park(alice)
	p.park(alice)
	p.park(parkRef("tiktok", "bob"))

	got := p.take()
	if len(got) != 2 {
		t.Fatalf("take() returned %d ids, want 2 (%v)", len(got), got)
	}

	// Taking empties the set, so a browser context that reconnects does not
	// re-ask for checks the previous one already released.
	if again := p.take(); len(again) != 0 {
		t.Errorf("second take() returned %v, want nothing", again)
	}
}

// The whole point of parking: a released check is asked for again.
func TestReleaseParkedChecks_Requeues(t *testing.T) {
	c := &Client{
		logger:         quietLogger(t),
		StatusReqPriCh: make(chan streamer.StreamerRef, 4),
	}
	c.parked.park(parkRef("tiktok", "alice"))

	c.releaseParkedChecks(context.Background(), c.logger.With("func", "test"))

	select {
	case got := <-c.StatusReqPriCh:
		if got.Username != "alice" {
			t.Errorf("requeued %v, want alice", got)
		}
	default:
		t.Fatal("nothing was requeued; the parked check would never run")
	}
}

// A full queue must not lose the check — dropping it strands the streamer with
// nothing left to correct it, which is what parking exists to prevent.
func TestReleaseParkedChecks_KeepsWhatDoesNotFit(t *testing.T) {
	c := &Client{
		logger:         quietLogger(t),
		StatusReqPriCh: make(chan streamer.StreamerRef), // unbuffered: nothing fits
	}
	c.parked.park(parkRef("tiktok", "alice"))

	c.releaseParkedChecks(context.Background(), c.logger.With("func", "test"))

	if got := c.parked.take(); len(got) != 1 {
		t.Errorf("parked set holds %v, want the check kept for the next announcement", got)
	}
}

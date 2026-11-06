package cloud

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/zalando/go-keyring"

	"github.com/vodlabs-io/recorder/internal/platformauth"
	"github.com/vodlabs-io/recorder/internal/streamer"
	"github.com/vodlabs-io/recorder/internal/vlconfig"
	"github.com/vodlabs-io/recorder/internal/vllog"
	"github.com/vodlabs-io/recorder/internal/vlstate"
)

// TestHandleStatusUpdateRechecksRestricted checks that a pushed "restricted"
// status is re-checked rather than dispatched: authenticated when a login is
// stored, plainly from this machine when not.
func TestHandleStatusUpdateRechecksRestricted(t *testing.T) {
	keyring.MockInit() // in-process keyring — never touches the real OS keychain
	ctx := context.Background()

	cookies := platformauth.NewStore(
		platformauth.WithFilePath(filepath.Join(t.TempDir(), "cookies.json")),
	)
	if err := cookies.Set(platformauth.Session{
		Platform: "tiktok",
		Cookies:  []platformauth.Cookie{{Name: "sessionid", Value: "test"}},
		Healthy:  true,
	}); err != nil {
		t.Fatalf("seed cookie store: %v", err)
	}

	logger, err := vllog.NewFileLogger(vlconfig.LoggerConfig{Mode: "off", Level: "error"}, "test")
	if err != nil {
		t.Fatalf("logger: %v", err)
	}
	_, kv := newTestKV(t)
	c := &Client{
		cookies:         cookies,
		kv:              kv,
		store:           vlstate.NewStore(kv),
		logger:          logger,
		StatusReqPriCh:  make(chan streamer.StreamerRef, 1),
		authStatusReqCh: make(chan streamer.StreamerRef, 1),
		StreamerCh:      make(chan streamer.StreamerState, 1),
	}

	restricted := func(platform, user string) streamer.StreamerState {
		return streamer.StreamerState{
			StreamerRef: streamer.StreamerRef{StreamerKey: streamer.StreamerKey{Platform: platform, Username: user}},
			Status:      streamer.StatusRestricted,
		}
	}
	noDispatch := func(t *testing.T) {
		t.Helper()
		select {
		case got := <-c.StreamerCh:
			t.Fatalf("restricted push must be re-checked, not dispatched: %v", got.StreamerRef)
		default:
		}
	}

	t.Run("withoutLoginRechecksLocally", func(t *testing.T) {
		str := restricted("bigo", "272391434") // no login stored for bigo
		c.handleStatusUpdate(ctx, str)
		select {
		case id := <-c.StatusReqPriCh:
			if id != str.StreamerRef {
				t.Fatalf("re-checked wrong ID: %v", id)
			}
		default:
			t.Fatal("restricted push without a login was not re-checked locally")
		}
		select {
		case id := <-c.authStatusReqCh:
			t.Fatalf("unexpected authed re-check without a login: %v", id)
		default:
		}
		noDispatch(t)
	})

	t.Run("withLoginRechecksAuthed", func(t *testing.T) {
		str := restricted("tiktok", "alice")
		c.handleStatusUpdate(ctx, str)
		select {
		case id := <-c.authStatusReqCh:
			if id != str.StreamerRef {
				t.Fatalf("escalated wrong ID: %v", id)
			}
		default:
			t.Fatal("restricted push with a stored login was not escalated")
		}
		select {
		case id := <-c.StatusReqPriCh:
			t.Fatalf("unexpected plain re-check with a login: %v", id)
		default:
		}
		noDispatch(t)
	})
}

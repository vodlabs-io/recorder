package vlapi

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/vodlabs-io/recorder/internal/streamer"
)

// disabledPlatformErr is the cloud refusing a URL whose platform it knows but
// does not run, shaped like the cloud package's rejectionError.
type disabledPlatformErr struct{}

func (disabledPlatformErr) Error() string            { return "Tango is not enabled on this server." }
func (disabledPlatformErr) UnsupportedSite() bool    { return true }
func (disabledPlatformErr) RejectedPlatform() string { return "tango" }
func (disabledPlatformErr) Definitive() bool         { return true }

// A URL on a platform the cloud knows but does not run is refused with the
// cloud's own words, rather than being tried as a page to record on its own.
func TestRecordOnce_DisabledPlatformIsNotAPage(t *testing.T) {
	s, _, pages := newPageService(t)
	s.streamerFuncs.Resolve = func(context.Context, streamer.StreamerRef) (streamer.StreamerRef, error) {
		return streamer.StreamerRef{}, disabledPlatformErr{}
	}
	req := addRequest(t, streamer.StreamerRef{URL: "https://www.tango.me/someone"})
	s.handleRecordOnce(req)

	env := req.envelope(t)
	if env.OK || len(pages.asked) != 0 {
		t.Fatalf("reply = %+v, page checks = %v", env, pages.asked)
	}
	if env.Error.Message != "Tango is not enabled on this server." {
		t.Fatalf("message = %q", env.Error.Message)
	}
}

type unreachableErr struct{}

func (unreachableErr) Error() string {
	return "This recorder could not reach Twitch: connection reset by peer."
}
func (unreachableErr) PlatformUnreachable() bool { return true }

func checkRequest(t *testing.T, wait bool) *fakeRequest {
	t.Helper()
	b, err := json.Marshal(CheckRequest{
		StreamerRef: streamer.StreamerRef{StreamerKey: streamer.StreamerKey{Platform: "twitch", Username: "someone"}},
		Wait:        wait,
	})
	if err != nil {
		t.Fatal(err)
	}
	return &fakeRequest{data: b}
}

// A check that waits reports why it failed, and a failure on this machine's
// network has a code of its own.
func TestCheckStreamer_WaitReportsTheFailure(t *testing.T) {
	s, _, _ := newAddService(t, nil)
	for _, tc := range []struct {
		err      error
		wantCode string
	}{
		{unreachableErr{}, ErrPlatformUnreachable.Error()},
		{errors.New("status failed: 500"), errUnavailable.Error()},
	} {
		s.streamerFuncs.Recheck = func(context.Context, streamer.StreamerRef) error { return tc.err }
		req := checkRequest(t, true)
		s.handleCheckStreamer(req)
		env := req.envelope(t)
		if env.OK || env.Error.Code != tc.wantCode || env.Error.Message != tc.err.Error() {
			t.Fatalf("reply = %+v, want %s: %q", env.Error, tc.wantCode, tc.err)
		}
	}

	s.streamerFuncs.Recheck = func(context.Context, streamer.StreamerRef) error { return nil }
	req := checkRequest(t, true)
	s.handleCheckStreamer(req)
	if env := req.envelope(t); !env.OK {
		t.Fatalf("reply = %+v", env)
	}
}

// Without wait, the check is queued as before.
func TestCheckStreamer_WithoutWaitQueues(t *testing.T) {
	s, _, _ := newAddService(t, nil)
	_, s.kv = newTestNATSKV(t)
	s.streamerFuncs.Recheck = func(context.Context, streamer.StreamerRef) error {
		t.Fatal("a queued check ran synchronously")
		return nil
	}
	req := checkRequest(t, false)
	s.handleCheckStreamer(req)
	if env := req.envelope(t); !env.OK || len(s.statusReqPriCh) != 1 {
		t.Fatalf("reply = %+v, queued = %d", env, len(s.statusReqPriCh))
	}
}

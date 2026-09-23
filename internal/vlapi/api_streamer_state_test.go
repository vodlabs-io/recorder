package vlapi

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/vodlabs-io/recorder/internal/streamer"
	"github.com/vodlabs-io/recorder/internal/vlstate"
)

// fakeKVEntry is the one method vlstate reads off an entry.
type fakeKVEntry struct {
	jetstream.KeyValueEntry
	value []byte
}

func (e fakeKVEntry) Value() []byte { return e.value }

// stateKV is a minimal jetstream.KeyValue serving seeded keys from memory, so
// the monitor/record guards can be driven without a NATS server. Anything not
// seeded reports "not found", which is what an unwritten doc looks like.
type stateKV struct {
	jetstream.KeyValue
	data map[string][]byte
}

func (k *stateKV) Get(_ context.Context, key string) (jetstream.KeyValueEntry, error) {
	v, ok := k.data[key]
	if !ok {
		return nil, jetstream.ErrKeyNotFound
	}
	return fakeKVEntry{value: v}, nil
}

// stateService seeds a listed, recording streamer plus its view doc.
func stateService(t *testing.T, doc vlstate.StreamerDoc) (*Service, streamer.StreamerRef, *stateKV, string) {
	t.Helper()
	key := streamer.StreamerKey{Platform: "kick", Username: "streamer_ten"}
	id := streamer.StreamerRef{StreamerKey: key}
	s, sf, path := newAddService(t, resolveTo(key))
	sf.AddID(id)
	if err := sf.Write(); err != nil {
		t.Fatalf("seed streamers file: %v", err)
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal doc: %v", err)
	}
	kv := &stateKV{data: map[string][]byte{vlstate.DocKVKey(key): raw}}
	s.kv = kv
	return s, id, kv, path
}

// markOneShot seeds the KV marker a purged one-shot is known by.
func markOneShot(kv *stateKV, id streamer.StreamerRef) {
	kv.data[id.StreamerKey.KVKey()+".monitor"] = []byte("false")
}

// monitorRequest builds a set_options payload carrying just the monitor flag.
func monitorRequest(t *testing.T, id streamer.StreamerRef, monitor bool) *fakeRequest {
	t.Helper()
	b, err := json.Marshal(setOptionsRequest{StreamerRef: id, Monitor: &monitor})
	if err != nil {
		t.Fatalf("marshal set_options: %v", err)
	}
	return &fakeRequest{data: b}
}

// Switching monitoring off on an offline streamer must be refused: the purge
// takes it out of streamers.toml, and the KV entry it lands in expires because
// no recording starts to lift the TTL.
func TestMonitorOff_RefusedWhenNotLive(t *testing.T) {
	for _, status := range []streamer.LiveStatus{
		streamer.StatusOffline, streamer.StatusUnknown, streamer.StatusDefunct,
	} {
		t.Run(string(status), func(t *testing.T) {
			s, id, _, path := stateService(t, vlstate.StreamerDoc{Status: status})

			req := monitorRequest(t, id, false)
			s.handleSetStreamerOptions(req)

			env := req.envelope(t)
			if env.OK {
				t.Fatalf("monitor off accepted for a %s streamer", status)
			}
			if env.Error == nil || env.Error.Code != ErrConflict.Error() {
				t.Errorf("error = %+v, want a conflict", env.Error)
			}
			if got := fileContents(t, path); strings.Contains(got, "monitor") {
				t.Errorf("a refused request still wrote a monitor rule:\n%s", got)
			}
		})
	}
}

// The legal transition still has to work, or the switch is just broken.
func TestMonitorOff_AllowedWhenLive(t *testing.T) {
	s, id, _, path := stateService(t, vlstate.StreamerDoc{Status: streamer.StatusLive})

	req := monitorRequest(t, id, false)
	s.handleSetStreamerOptions(req)

	if env := req.envelope(t); !env.OK {
		t.Fatalf("monitor off refused for a live streamer: %+v", env.Error)
	}
	if err := s.streamersFile.Load(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !s.streamersFile.HasOneShotRule(id) {
		t.Errorf("no one-shot rule written:\n%s", fileContents(t, path))
	}
}

// A running session outranks the status field, which lags a stream that just
// came back — the recording in front of the user is the fact that matters.
func TestMonitorOff_AllowedWhileRecording(t *testing.T) {
	s, id, _, _ := stateService(t, vlstate.StreamerDoc{
		Status:          streamer.StatusOffline,
		ActiveSessionID: "sess-1",
	})

	req := monitorRequest(t, id, false)
	s.handleSetStreamerOptions(req)

	if env := req.envelope(t); !env.OK {
		t.Fatalf("monitor off refused mid-recording: %+v", env.Error)
	}
}

// Monitor off + record off is the state that cannot be stored: the purge drops
// the record=false rule along with the file entry, which would switch
// recording back ON.
func TestMonitorOff_RefusedWhenRecordingOff(t *testing.T) {
	s, id, _, path := stateService(t, vlstate.StreamerDoc{Status: streamer.StatusLive})

	off := addRequest(t, id)
	s.handleRecordOff(off)
	if env := off.envelope(t); !env.OK {
		t.Fatalf("record off failed: %+v", env.Error)
	}

	req := monitorRequest(t, id, false)
	s.handleSetStreamerOptions(req)

	env := req.envelope(t)
	if env.OK {
		t.Fatalf("monitor off accepted while recording is off:\n%s", fileContents(t, path))
	}
	if env.Error == nil || env.Error.Code != ErrConflict.Error() {
		t.Errorf("error = %+v, want a conflict", env.Error)
	}
	if err := s.streamersFile.Load(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if s.streamersFile.IsRecording(id) {
		t.Errorf("the refused request switched recording back on:\n%s", fileContents(t, path))
	}
}

// Only the transition is gated. A one-shot's options dialog sends monitor=false
// unchanged along with everything else, and saving a variant must not require
// the streamer to still be live.
func TestMonitorOff_OneShotSavesOtherOptions(t *testing.T) {
	s, id, kv, _ := stateService(t, vlstate.StreamerDoc{Status: streamer.StatusOffline})
	markOneShot(kv, id)

	variant := "720p"
	monitor := false
	b, err := json.Marshal(setOptionsRequest{StreamerRef: id, Monitor: &monitor, Variant: &variant})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := &fakeRequest{data: b}
	s.handleSetStreamerOptions(req)

	if env := req.envelope(t); !env.OK {
		t.Fatalf("a one-shot could not save its options: %+v", env.Error)
	}
	if err := s.streamersFile.Load(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got := s.streamersFile.OptionsFor(id).Variant; got != variant {
		t.Errorf("variant = %q, want %q", got, variant)
	}
}

// `streamer disable` must not reach monitor off + record off via a one-shot.
func TestRecordOff_RefusedForOneShot(t *testing.T) {
	s, id, kv, path := stateService(t, vlstate.StreamerDoc{Status: streamer.StatusLive})
	markOneShot(kv, id)

	req := addRequest(t, id)
	s.handleRecordOff(req)

	env := req.envelope(t)
	if env.OK {
		t.Fatalf("record off accepted for a one-shot:\n%s", fileContents(t, path))
	}
	if env.Error == nil || env.Error.Code != ErrConflict.Error() {
		t.Errorf("error = %+v, want a conflict", env.Error)
	}
}

// `record --once` on a streamer already being monitored must be refused rather
// than converting it, which would delete it when the run finished.
func TestRecordOnce_RefusedWhenAlreadyTracked(t *testing.T) {
	s, id, _, path := stateService(t, vlstate.StreamerDoc{Status: streamer.StatusLive})

	b, err := json.Marshal(AddRequest{StreamerRef: id})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := &fakeRequest{data: b}
	s.handleRecordOnce(req)

	env := req.envelope(t)
	if env.OK {
		t.Fatalf("record_once accepted for a monitored streamer:\n%s", fileContents(t, path))
	}
	if env.Error == nil || env.Error.Code != ErrConflict.Error() {
		t.Errorf("error = %+v, want a conflict", env.Error)
	}
	if err := s.streamersFile.Load(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if s.streamersFile.HasOneShotRule(id) {
		t.Errorf("the monitored streamer was converted to a one-shot:\n%s", fileContents(t, path))
	}
}

// drained reports how many signals are queued on a channel, emptying it.
func drained(ch chan struct{}) int {
	n := 0
	for {
		select {
		case <-ch:
			n++
		default:
			return n
		}
	}
}

// A record switch leaves the subscription untouched, so it must not cost a
// subscribe round trip, which would lose notifications published meanwhile.
func TestRecordSwitch_ReloadsWithoutResubscribing(t *testing.T) {
	s, id, _, _ := stateService(t, vlstate.StreamerDoc{Status: streamer.StatusLive})
	drained(s.signals.Resubscribe)

	off := addRequest(t, id)
	s.handleRecordOff(off)
	if env := off.envelope(t); !env.OK {
		t.Fatalf("record off failed: %+v", env.Error)
	}
	on := addRequest(t, id)
	s.handleRecordOn(on)
	if env := on.envelope(t); !env.OK {
		t.Fatalf("record on failed: %+v", env.Error)
	}

	if got := drained(s.signals.Resubscribe); got != 0 {
		t.Errorf("record toggles triggered %d resubscribes, want 0", got)
	}
	if got := drained(s.signals.Reload); got == 0 {
		t.Error("record toggles triggered no config reload; the switch would never reach a live worker")
	}
}

// Every other per-streamer option is local too. Only monitor moves a streamer
// between streamers.toml and the one-shot list, and only that move needs the
// subscription rebuilt.
func TestSetOptions_ReloadsWhenOnlyOptionsChange(t *testing.T) {
	s, id, _, _ := stateService(t, vlstate.StreamerDoc{Status: streamer.StatusOffline})
	drained(s.signals.Resubscribe)

	variant := "720p"
	monitor := true
	b, err := json.Marshal(setOptionsRequest{StreamerRef: id, Monitor: &monitor, Variant: &variant})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := &fakeRequest{data: b}
	s.handleSetStreamerOptions(req)
	if env := req.envelope(t); !env.OK {
		t.Fatalf("set_options failed: %+v", env.Error)
	}

	if got := drained(s.signals.Resubscribe); got != 0 {
		t.Errorf("a variant change triggered %d resubscribes, want 0", got)
	}
	if got := drained(s.signals.Reload); got == 0 {
		t.Error("a variant change triggered no config reload")
	}
}

func TestSetOptions_ResubscribesOnMonitorChange(t *testing.T) {
	s, id, _, _ := stateService(t, vlstate.StreamerDoc{Status: streamer.StatusLive})
	drained(s.signals.Resubscribe)

	req := monitorRequest(t, id, false)
	s.handleSetStreamerOptions(req)
	if env := req.envelope(t); !env.OK {
		t.Fatalf("monitor off failed: %+v", env.Error)
	}

	if got := drained(s.signals.Resubscribe); got == 0 {
		t.Error("a monitor change did not resubscribe; the one-shot purge only runs in a subscription cycle")
	}
}

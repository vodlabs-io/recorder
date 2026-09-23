package vlapi

import (
	"testing"

	"github.com/vodlabs-io/recorder/internal/streamer"
)

// recordingStreamer seeds a plain listed streamer — recording, like every
// streamer is by default.
func recordingStreamer(t *testing.T) (*Service, streamer.StreamerRef, string) {
	t.Helper()
	key := streamer.StreamerKey{Platform: "kick", Username: "streamer_ten"}
	id := streamer.StreamerRef{StreamerKey: key}
	s, sf, path := newAddService(t, resolveTo(key))
	sf.AddID(id)
	if err := sf.Write(); err != nil {
		t.Fatalf("seed streamers file: %v", err)
	}
	return s, id, path
}

// TestRecordOff_SwitchesRecordingOff checks that a recording streamer is not
// refused as "already disabled".
func TestRecordOff_SwitchesRecordingOff(t *testing.T) {
	s, id, path := recordingStreamer(t)

	req := addRequest(t, id)
	s.handleRecordOff(req)
	if env := req.envelope(t); !env.OK {
		t.Fatalf("record off failed: %+v", env.Error)
	}

	if err := s.streamersFile.Load(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if s.streamersFile.IsRecording(id) {
		t.Errorf("streamer still recording after record_off:\n%s", fileContents(t, path))
	}
}

// A second record_off is a no-op, not another rule: without the guard each
// click would append one more record=false rule to streamers.toml.
func TestRecordOff_TwiceIsRejected(t *testing.T) {
	s, id, path := recordingStreamer(t)

	first := addRequest(t, id)
	s.handleRecordOff(first)
	if env := first.envelope(t); !env.OK {
		t.Fatalf("record off failed: %+v", env.Error)
	}

	second := addRequest(t, id)
	s.handleRecordOff(second)
	if env := second.envelope(t); env.OK {
		t.Errorf("second record_off accepted, want conflict:\n%s", fileContents(t, path))
	}
}

// record_on is the way back: the switch has to survive a full round trip.
func TestRecordOn_SwitchesRecordingBackOn(t *testing.T) {
	s, id, path := recordingStreamer(t)

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

	if err := s.streamersFile.Load(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !s.streamersFile.IsRecording(id) {
		t.Errorf("streamer not recording after record_on:\n%s", fileContents(t, path))
	}
	// The streamer itself must survive the round trip: the rule that carried it
	// was dropped, so it has to be re-homed into [streamers].
	if !listed(s.streamersFile.AllIDs(), id) {
		t.Errorf("streamer lost after record_on/off round trip:\n%s", fileContents(t, path))
	}
}

// listed reports whether ids covers id.
func listed(ids streamer.StreamerRefs, id streamer.StreamerRef) bool {
	for _, got := range ids {
		if got.String() == id.String() {
			return true
		}
	}
	return false
}

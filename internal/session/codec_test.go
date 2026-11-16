package session

import (
	"log/slog"
	"strings"
	"testing"

	"github.com/vodlabs-io/recorder/internal/agora"
	"github.com/vodlabs-io/recorder/internal/stream"
	"github.com/vodlabs-io/recorder/internal/streamer"
	"github.com/vodlabs-io/recorder/internal/writer"
)

const superliveURL = "agora://app/chan?mint=superlive&room=152&token=t&uid=5&user=99"

func codecWorker() *worker {
	w := &worker{logger: slog.New(slog.DiscardHandler)}
	w.str.Store(&streamer.StreamerState{Status: streamer.StatusLive, Streams: stream.List{{URL: superliveURL, QualityID: "source"}}})
	return w
}

// TestAdoptCodecSwitchesToTheBroadcastersCodec pins that a session that has
// recorded nothing rejoins a VP8 broadcaster in VP8, as WebM, and keeps doing
// so for every stream URL the cloud sends after.
func TestAdoptCodecSwitchesToTheBroadcastersCodec(t *testing.T) {
	w := codecWorker()
	if err := w.initStream(*w.str.Load()); err != nil {
		t.Fatal(err)
	}
	if w.writer.Name() != "flvwriter" {
		t.Fatalf("writer %s before the switch", w.writer.Name())
	}
	if !w.adoptCodec(agora.CodecVP8) {
		t.Fatal("a session that recorded nothing did not switch")
	}
	if w.writer.Name() != "webmwriter" || w.output.WriterName != "webmwriter" || !w.selectedStream.IsVP8() {
		t.Fatalf("after the switch: writer %s, url %s", w.writer.Name(), w.selectedStream.URL)
	}

	// A later status update still names no codec.
	if err := w.initStream(*w.str.Load()); err != nil {
		t.Fatal(err)
	}
	if w.writer.Name() != "webmwriter" || !w.selectedStream.IsVP8() {
		t.Fatalf("after an update: writer %s, url %s", w.writer.Name(), w.selectedStream.URL)
	}
	if strings.Contains(w.str.Load().Streams[0].URL, "codec=") {
		t.Error("the cloud's stream record was rewritten in place")
	}
}

func TestAdoptCodecRefusesOnceRecording(t *testing.T) {
	w := codecWorker()
	if err := w.initStream(*w.str.Load()); err != nil {
		t.Fatal(err)
	}
	w.output.Stats.TotalBytes = 1 << 20
	if w.adoptCodec(agora.CodecVP8) {
		t.Fatal("a session with H.264 parts switched to VP8")
	}
	if w.writer.Name() != "flvwriter" || w.selectedStream.IsVP8() {
		t.Fatalf("writer %s, url %s", w.writer.Name(), w.selectedStream.URL)
	}
}

// TestRestoredWebMSessionJoinsInVP8 pins that a resumed session keeps the
// codec its parts are in, though the cloud's URL names none.
func TestRestoredWebMSessionJoinsInVP8(t *testing.T) {
	w := codecWorker()
	w.restore(&checkpoint{Platform: "superlive", Username: "99", SessionID: "s1", Writer: "webmwriter", TotalBytes: 1 << 20})
	if err := w.initStream(*w.str.Load()); err != nil {
		t.Fatal(err)
	}
	if w.writer.Name() != "webmwriter" || !w.selectedStream.IsVP8() {
		t.Fatalf("writer %s, url %s", w.writer.Name(), w.selectedStream.URL)
	}
}

func TestSessionCodecLeavesOtherStreamsAlone(t *testing.T) {
	w := codecWorker()
	w.agoraCodec, w.agoraCodecSet = agora.CodecVP8, true
	s := &stream.Stream{URL: "https://cdn.example/live.flv"}
	if got := w.inSessionCodec(s); got != s {
		t.Errorf("rewrote %q to %q", s.URL, got.URL)
	}
	if _, ok := writer.Get(writerFor(s)); !ok {
		t.Fatal("no writer for an HTTP-FLV stream")
	}
}

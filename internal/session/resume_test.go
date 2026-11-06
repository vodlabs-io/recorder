package session

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/vodlabs-io/recorder/internal/recording"
	"github.com/vodlabs-io/recorder/internal/stream"
	"github.com/vodlabs-io/recorder/internal/streamer"
	"github.com/vodlabs-io/recorder/internal/vlconfig"
	"github.com/vodlabs-io/recorder/internal/vllog"
	"github.com/vodlabs-io/recorder/internal/vlstate"
	"github.com/vodlabs-io/recorder/internal/writer"
	"github.com/vodlabs-io/recorder/internal/writer/hlsingest"
)

var resumeKey = streamer.StreamerKey{Platform: "twitch", Username: "alice"}

// heldCheckpoint is an interrupted HLS session with one closed part in an open
// group, last recording a minute ago, in a folder that exists.
func heldCheckpoint(t *testing.T) *checkpoint {
	t.Helper()
	now := time.Now()
	return &checkpoint{
		Version:    checkpointVersion,
		Platform:   resumeKey.Platform,
		Username:   resumeKey.Username,
		SessionID:  "sess1",
		BaseOutDir: t.TempDir(),
		Writer:     "hlswriter",
		Variant:    "best",
		Started:    now.Add(-time.Hour),
		LastByteAt: now.Add(-time.Minute),
		LiveStart:  now.Add(-2 * time.Hour).Unix(),
		TotalBytes: 5 << 20,
		Parts: []recording.Part{{
			Index: 1, Bytes: 5 << 20, Path: "pt1_best",
			StartTime: now.Add(-time.Hour), EndTime: now.Add(-time.Minute),
		}},
		PartIndex:     2,
		Group:         &groupState{Index: 1, Stems: []string{"pt1_best"}, Bytes: 5 << 20, Start: now.Add(-time.Hour)},
		GroupSeq:      1,
		InterruptedAt: now.Add(-time.Minute),
		Reason:        stopShutdown.String(),
	}
}

func liveState(liveStart int64) streamer.StreamerState {
	return streamer.StreamerState{
		StreamerRef:   streamer.StreamerRef{StreamerKey: resumeKey},
		Status:        streamer.StatusLive,
		Streams:       stream.List{{QualityID: "best", URL: "https://cdn.example/live/index.m3u8"}},
		LastLiveStart: liveStart,
	}
}

func TestCanResume(t *testing.T) {
	now := time.Now()
	window := 15 * time.Minute
	tests := []struct {
		name   string
		mutate func(cp *checkpoint, str *streamer.StreamerState, writer *string, window *time.Duration)
		want   bool
	}{
		{"same broadcast within the window", nil, true},
		{"resuming off", func(_ *checkpoint, _ *streamer.StreamerState, _ *string, w *time.Duration) { *w = 0 }, false},
		{"gap longer than the window", func(cp *checkpoint, _ *streamer.StreamerState, _ *string, _ *time.Duration) {
			cp.LastByteAt = now.Add(-16 * time.Minute)
		}, false},
		{"service saw a new broadcast", func(cp *checkpoint, str *streamer.StreamerState, _ *string, _ *time.Duration) {
			str.LastLiveStart = cp.LiveStart + 600
		}, false},
		{"service reports no start time", func(_ *checkpoint, str *streamer.StreamerState, _ *string, _ *time.Duration) {
			str.LastLiveStart = 0
		}, true},
		{"session had no start time", func(cp *checkpoint, str *streamer.StreamerState, _ *string, _ *time.Duration) {
			cp.LiveStart = 0
			str.LastLiveStart = now.Unix()
		}, true},
		{"stream now in another format", func(_ *checkpoint, _ *streamer.StreamerState, w *string, _ *time.Duration) {
			*w = "flvwriter"
		}, false},
		{"folder gone", func(cp *checkpoint, _ *streamer.StreamerState, _ *string, _ *time.Duration) {
			cp.BaseOutDir = filepath.Join(cp.BaseOutDir, "missing")
		}, false},
		{"checkpoint from another version", func(cp *checkpoint, _ *streamer.StreamerState, _ *string, _ *time.Duration) {
			cp.Version = checkpointVersion + 1
		}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cp := heldCheckpoint(t)
			str := liveState(cp.LiveStart)
			writerName, w := "hlswriter", window
			if tt.mutate != nil {
				tt.mutate(cp, &str, &writerName, &w)
			}
			got, why := canResume(cp, str, writerName, w, now)
			if got != tt.want {
				t.Fatalf("canResume = %v (%s), want %v", got, why, tt.want)
			}
			if !got && why == "" {
				t.Fatal("a refusal must say why")
			}
		})
	}
}

func TestBroadcastOver(t *testing.T) {
	cp := heldCheckpoint(t)
	tests := []struct {
		name string
		str  streamer.StreamerState
		want bool
	}{
		{"still live", liveState(cp.LiveStart), false},
		{"live, no start time", liveState(0), false},
		{"live again as a new broadcast", liveState(cp.LiveStart + 60), true},
		{"paused", streamer.StreamerState{Status: streamer.StatusPaused, LastLiveStart: cp.LiveStart}, false},
		{"offline", streamer.StreamerState{Status: streamer.StatusOffline}, true},
		{"removed", streamer.StreamerState{Status: streamer.StatusRemoved}, true},
		{"restricted", streamer.StreamerState{Status: streamer.StatusRestricted}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := broadcastOver(cp, tt.str); got != tt.want {
				t.Fatalf("broadcastOver = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestResumeStoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s, cps, err := openResumeStore(dir)
	if err != nil || len(cps) != 0 {
		t.Fatalf("open empty: %v, %d checkpoints", err, len(cps))
	}
	cp := heldCheckpoint(t)
	if err := s.hold(cp); err != nil {
		t.Fatalf("hold: %v", err)
	}
	if got := s.heldDirs(); len(got) != 1 || got[0] != cp.BaseOutDir {
		t.Fatalf("heldDirs = %v", got)
	}
	// Junk in the folder is dropped, not fatal.
	if err := os.WriteFile(filepath.Join(dir, "junk.json"), []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, cps, err = openResumeStore(dir)
	if err == nil {
		t.Fatal("want an error naming the unreadable checkpoint")
	}
	if len(cps) != 1 || cps[0].SessionID != cp.SessionID || len(cps[0].Parts) != 1 || cps[0].Group.Stems[0] != "pt1_best" {
		t.Fatalf("reopened checkpoints = %+v", cps)
	}
	if _, err := os.Stat(filepath.Join(dir, "junk.json")); !os.IsNotExist(err) {
		t.Fatal("unreadable checkpoint not removed")
	}

	if s.take(resumeKey) == nil || s.take(resumeKey) != nil {
		t.Fatal("take hands a checkpoint out exactly once")
	}
	s.remove(cp.SessionID)
	if _, cps, _ = openResumeStore(dir); len(cps) != 0 {
		t.Fatalf("removed checkpoint still on disk: %d", len(cps))
	}
}

func TestTakeExpired(t *testing.T) {
	s, _, err := openResumeStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cp := heldCheckpoint(t) // last byte a minute ago
	if err := s.hold(cp); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if got := s.takeExpired(now, 15*time.Minute); len(got) != 0 {
		t.Fatalf("expired too early: %d", len(got))
	}
	if got := s.takeExpired(now.Add(15*time.Minute), 15*time.Minute); len(got) != 1 {
		t.Fatalf("not expired after the window: %d", len(got))
	}
	if err := s.hold(cp); err != nil {
		t.Fatal(err)
	}
	if got := s.takeExpired(now, 0); len(got) != 1 {
		t.Fatal("turning resuming off must expire every interrupted session")
	}
}

func TestCheckpointRestoresWorker(t *testing.T) {
	w, _ := emitWorker(t, "hlswriter")
	w.strKey = resumeKey
	w.started = time.Now().Add(-time.Hour)
	w.config.Variant = "720p"
	w.config.SplitDuration = "2h"
	w.output.PartIndex = 3
	w.output.Stats.TotalBytes = 9 << 20
	w.output.Parts = []recording.Part{{Index: 1, Bytes: 4 << 20}, {Index: 2, Bytes: 5 << 20}}
	w.completePart("pt1_720p", 4<<20, false)
	w.completePart("pt2_720p", 5<<20, false)
	last := time.Now().Add(-30 * time.Second)
	w.lastByteAt.Store(last.UnixNano())
	w.str.Store(&streamer.StreamerState{LastLiveStart: 1234})

	cp := w.checkpoint(nil)

	r, _ := emitWorker(t, "hlswriter")
	r.restore(cp)
	if r.SessionID != w.SessionID || r.output.BaseOutDir != w.output.BaseOutDir || r.output.PartIndex != 3 {
		t.Fatalf("identity not restored: %+v", r.output)
	}
	if r.config.Variant != "720p" || r.config.SplitDuration != "2h" {
		t.Fatalf("session settings not kept: %+v", r.config)
	}
	if r.group == nil || len(r.group.stems) != 2 || r.groupSeq != 1 {
		t.Fatalf("group not restored: %+v", r.group)
	}
	if !r.priorLastByte.Equal(last.Truncate(0)) && r.priorLastByte.Sub(last).Abs() > time.Millisecond {
		t.Fatalf("priorLastByte = %v, want %v", r.priorLastByte, last)
	}
	if !r.lastByteTime().IsZero() {
		t.Fatal("a resumed session's gap clock must start at the resume, not the last byte")
	}
	if cp.LiveStart != 1234 {
		t.Fatalf("LiveStart = %d", cp.LiveStart)
	}
	// The recording clock carries on from the original start.
	if d := r.Tracker.GetStats(r.SessionID).Duration; d < time.Hour {
		t.Fatalf("resumed clock reads %v, want at least an hour", d)
	}
}

// A session resumed after an interruption still comes out as the one VOD a
// recorder that never stopped would have produced.
func TestResumedSessionEmitsOneVOD(t *testing.T) {
	w, _ := emitWorker(t, "hlswriter")
	w.strKey = resumeKey
	w.started = time.Now().Add(-time.Hour)
	w.completePart("pt1_best", 4<<20, false)
	w.completePart("pt2_best", 4<<20, false)
	cp := w.checkpoint(nil)

	r, ch := emitWorker(t, "hlswriter")
	r.restore(cp)
	r.completePart("pt3_best", 4<<20, false)
	r.finishSession()

	got := drain(ch)
	if len(got) != 2 || got[0].kind != "group" || got[0].output != "vod.mp4" || len(got[0].inputs) != 3 || got[1].kind != "finalize" {
		t.Fatalf("emitted = %+v, want one vod.mp4 of pt1..pt3 then finalize", got)
	}
}

func TestResumable(t *testing.T) {
	store, _, err := openResumeStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	base := func() *worker {
		w, _ := emitWorker(t, "hlswriter")
		w.resume = store
		w.resumeWindow = 15 * time.Minute
		w.output.Parts = []recording.Part{{Index: 1, Bytes: 1 << 20}}
		w.str.Store(&streamer.StreamerState{Status: streamer.StatusLive})
		return w
	}
	if !base().resumable() {
		t.Fatal("a live session with a kept part is resumable")
	}
	w := base()
	w.stop.Store(int32(stopSwitchedOff))
	if !w.resumable() {
		t.Fatal("switching recording off keeps the session for a resume")
	}
	w = base()
	w.stop.Store(int32(stopRemoved))
	if w.resumable() {
		t.Fatal("a removed streamer's session is finished")
	}
	w = base()
	w.output.Parts = nil
	if w.resumable() {
		t.Fatal("a session that kept nothing has nothing to resume")
	}
	w = base()
	w.str.Store(&streamer.StreamerState{Status: streamer.StatusOffline})
	if w.resumable() {
		t.Fatal("a session whose stream ended is finished")
	}
	w = base()
	w.resumeWindow = 0
	if w.resumable() {
		t.Fatal("resuming off finishes every session")
	}
}

func TestFoldCrashedPart(t *testing.T) {
	w, ch := emitWorker(t, "hlswriter")
	w.strKey = resumeKey
	w.output.BaseOutDir = t.TempDir()
	w.started = time.Now().Add(-time.Hour)
	w.completePart("pt1_best", 4<<20, false)
	w.output.Parts = []recording.Part{{Index: 1, Bytes: 4 << 20}}
	w.output.PartIndex = 2

	partDir := filepath.Join(w.output.BaseOutDir, "pt2_best")
	if err := os.MkdirAll(partDir, 0o755); err != nil {
		t.Fatal(err)
	}
	seg := make([]byte, 300<<10)
	if err := os.WriteFile(filepath.Join(partDir, "seg1.ts"), seg, 0o644); err != nil {
		t.Fatal(err)
	}
	w.foldCrashedPart(&currentPart{Stem: "pt2_best", Path: partDir, Index: 2, Start: time.Now().Add(-10 * time.Minute)})

	if w.output.PartIndex != 3 || len(w.output.Parts) != 2 || w.output.Parts[1].Bytes != int64(len(seg)) {
		t.Fatalf("crashed part not kept: %+v", w.output)
	}
	if w.group == nil || len(w.group.stems) != 2 || w.group.stems[1] != "pt2_best" {
		t.Fatalf("crashed part did not join its group: %+v", w.group)
	}
	if w.priorLastByte.IsZero() {
		t.Fatal("the crashed part's last write is the session's last byte")
	}
	if got := drain(ch); len(got) != 0 {
		t.Fatalf("an HLS part needs no conversion job: %+v", got)
	}

	// A part too small to keep is removed, and its number is not reused.
	w2, _ := emitWorker(t, "hlswriter")
	w2.output.BaseOutDir = t.TempDir()
	w2.output.PartIndex = 1
	small := filepath.Join(w2.output.BaseOutDir, "pt1_best")
	if err := os.MkdirAll(small, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(small, "seg1.ts"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	w2.foldCrashedPart(&currentPart{Stem: "pt1_best", Path: small, Index: 1})
	if _, err := os.Stat(small); !os.IsNotExist(err) {
		t.Fatal("an empty crashed part is discarded")
	}
	if w2.output.PartIndex != 2 || len(w2.output.Parts) != 0 {
		t.Fatalf("after an empty crashed part: %+v", w2.output)
	}
}

// resumeRegistry is a registry with a checkpoint store and a real view-doc
// store, whose processing jobs land on the returned channel.
func resumeRegistry(t *testing.T) (*Registry, chan recording.Job) {
	t.Helper()
	logger, err := vllog.NewFileLogger(vlconfig.LoggerConfig{Mode: "off", Level: "error"}, "test")
	if err != nil {
		t.Fatalf("logger: %v", err)
	}
	t.Cleanup(func() { logger.Close() })
	kv := testKV(t)
	ch := make(chan recording.Job, 64)
	r := &Registry{
		workers:             make(map[streamer.StreamerKey]*worker),
		logger:              logger,
		cfg:                 vlconfig.SessionConfig{DownloadDir: t.TempDir(), ResumeWindow: "15m"},
		kv:                  kv,
		store:               vlstate.NewStore(kv),
		pendingRecordingsCh: ch,
	}
	r.openResume(t.TempDir())
	if r.resume == nil {
		t.Fatal("resume store not opened")
	}
	return r, ch
}

func testKV(t *testing.T) jetstream.KeyValue {
	t.Helper()
	srv, err := server.NewServer(&server.Options{
		Host: "127.0.0.1", Port: -1, JetStream: true, StoreDir: t.TempDir(), NoLog: true, NoSigs: true,
	})
	if err != nil {
		t.Fatalf("nats server: %v", err)
	}
	go srv.Start()
	if !srv.ReadyForConnections(5 * time.Second) {
		t.Fatal("nats server not ready")
	}
	t.Cleanup(srv.Shutdown)
	nc, err := nats.Connect(srv.ClientURL())
	if err != nil {
		t.Fatalf("nats connect: %v", err)
	}
	t.Cleanup(nc.Close)
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	kv, err := js.CreateKeyValue(context.Background(), jetstream.KeyValueConfig{Bucket: "streamers"})
	if err != nil {
		t.Fatalf("create kv: %v", err)
	}
	return kv
}

func TestRegistryResumesOrFinishes(t *testing.T) {
	t.Run("same broadcast resumes", func(t *testing.T) {
		r, ch := resumeRegistry(t)
		cp := heldCheckpoint(t)
		if err := r.resume.hold(cp); err != nil {
			t.Fatal(err)
		}
		got := r.takeResumable(liveState(cp.LiveStart))
		if got == nil || got.SessionID != cp.SessionID {
			t.Fatal("the interrupted session should resume")
		}
		if jobs := drain(ch); len(jobs) != 0 {
			t.Fatalf("a resumed session sends nothing for processing: %+v", jobs)
		}
	})
	t.Run("new broadcast finishes the old one", func(t *testing.T) {
		r, ch := resumeRegistry(t)
		cp := heldCheckpoint(t)
		if err := r.resume.hold(cp); err != nil {
			t.Fatal(err)
		}
		if got := r.takeResumable(liveState(cp.LiveStart + 300)); got != nil {
			t.Fatal("a new broadcast must not resume the old session")
		}
		jobs := drain(ch)
		if len(jobs) != 2 || jobs[0].kind != "group" || jobs[0].output != "vod.mp4" || jobs[1].kind != "finalize" {
			t.Fatalf("finished session emitted %+v", jobs)
		}
		if r.resume.peek(resumeKey) != nil {
			t.Fatal("a finished session is no longer interrupted")
		}
		if _, cps, _ := openResumeStore(r.resume.dir); len(cps) != 0 {
			t.Fatal("a finished session leaves no checkpoint")
		}
	})
	t.Run("offline status finishes it without waiting", func(t *testing.T) {
		r, ch := resumeRegistry(t)
		cp := heldCheckpoint(t)
		if err := r.resume.hold(cp); err != nil {
			t.Fatal(err)
		}
		r.settleInterrupted(context.Background(), streamer.StreamerState{
			StreamerRef: streamer.StreamerRef{StreamerKey: resumeKey},
			Status:      streamer.StatusOffline,
		})
		if jobs := drain(ch); len(jobs) != 2 {
			t.Fatalf("offline should finish the session: %+v", jobs)
		}
	})
	t.Run("nothing is sent once the registry is closed", func(t *testing.T) {
		r, ch := resumeRegistry(t)
		cp := heldCheckpoint(t)
		r.Wait()
		r.finishInterrupted(cp, "test")
		if jobs := drain(ch); len(jobs) != 0 {
			t.Fatalf("sent after Wait: %+v", jobs)
		}
	})
}

func TestOpenResumeRecoversCrash(t *testing.T) {
	r, _ := resumeRegistry(t)
	cp := heldCheckpoint(t)
	cp.InterruptedAt = time.Time{}
	cp.Reason = ""
	partDir := filepath.Join(cp.BaseOutDir, "pt2_best")
	if err := os.MkdirAll(partDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(partDir, "seg1.ts"), make([]byte, 200<<10), 0o644); err != nil {
		t.Fatal(err)
	}
	cp.Current = &currentPart{Stem: "pt2_best", Path: partDir, Index: 2, Start: time.Now().Add(-time.Minute)}
	if err := r.resume.save(cp); err != nil {
		t.Fatal(err)
	}

	r.openResume(r.resume.dir)
	got := r.resume.peek(resumeKey)
	if got == nil {
		t.Fatal("crashed session not held")
	}
	if got.Reason != reasonCrashed || got.InterruptedAt.IsZero() || got.Current != nil {
		t.Fatalf("crashed checkpoint not converted: %+v", got)
	}
	if got.PartIndex != 3 || len(got.Parts) != 2 || got.Group == nil || len(got.Group.Stems) != 2 {
		t.Fatalf("crashed part not folded in: parts=%d index=%d group=%+v", len(got.Parts), got.PartIndex, got.Group)
	}
	logged, _ := os.ReadFile(filepath.Join(cp.BaseOutDir, "session.log"))
	if len(logged) == 0 {
		t.Fatal("recovery is logged in the session log")
	}
}

func TestCheckpointCarriesHLSTail(t *testing.T) {
	w, _ := emitWorker(t, "hlswriter")
	w.strKey = resumeKey
	w.carryover = writer.NewCarryover()
	w.carryover.SeedHLSTail([]string{"h1", "h2"})
	cp := w.checkpoint(nil)

	r, _ := emitWorker(t, "hlswriter")
	r.restore(cp)
	if got := r.carryover.HLSTail(); len(got) != 2 || got[0] != "h1" || got[1] != "h2" {
		t.Fatalf("restored tail = %v", got)
	}
}

// A part cut off by a crash leaves its segments on disk, which are what the
// resumed part must not record again.
func TestCrashedHLSPartSeedsTheTail(t *testing.T) {
	w, _ := emitWorker(t, "hlswriter")
	w.output.BaseOutDir = t.TempDir()
	w.output.PartIndex = 1
	w.carryover = writer.NewCarryover()
	dir := filepath.Join(w.output.BaseOutDir, "pt1_best")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	var segs [][]byte
	for i := range 3 {
		seg := make([]byte, 60<<10)
		seg[0] = byte(i)
		segs = append(segs, seg)
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("%06d.ts", i)), seg, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Unterminated, as a crash leaves it.
	playlist := "#EXTM3U\n#EXTINF:5.000,\n000000.ts\n#EXTINF:5.000,\n000001.ts\n#EXTINF:5.000,\n000002.ts\n"
	if err := os.WriteFile(filepath.Join(dir, "index.m3u8"), []byte(playlist), 0o644); err != nil {
		t.Fatal(err)
	}

	w.foldCrashedPart(&currentPart{Stem: "pt1_best", Path: dir, Index: 1})
	got := w.carryover.HLSTail()
	if len(got) != 3 || got[2] != hlsingest.SegmentHash(segs[2]) || got[0] != hlsingest.SegmentHash(segs[0]) {
		t.Fatalf("tail after crash = %v", got)
	}
}

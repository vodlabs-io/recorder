package vlstate

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/vodlabs-io/recorder/internal/recording"
	"github.com/vodlabs-io/recorder/internal/stream"
	"github.com/vodlabs-io/recorder/internal/streamer"
)

// fakeKV records doc writes/deletes. The embedded interface panics on any
// method the store isn't expected to call.
type fakeKV struct {
	jetstream.KeyValue
	data map[string][]byte
	puts int
}

func newFakeKV() *fakeKV {
	return &fakeKV{data: map[string][]byte{}}
}

func (f *fakeKV) Put(_ context.Context, key string, value []byte) (uint64, error) {
	f.data[key] = value
	f.puts++
	return 0, nil
}

func (f *fakeKV) PutString(ctx context.Context, key string, value string) (uint64, error) {
	return f.Put(ctx, key, []byte(value))
}

func (f *fakeKV) Delete(_ context.Context, key string, _ ...jetstream.KVDeleteOpt) error {
	delete(f.data, key)
	return nil
}

func (f *fakeKV) doc(t *testing.T, kvKey string) StreamerDoc {
	t.Helper()
	data, ok := f.data[kvKey]
	if !ok {
		t.Fatalf("no doc at %s", kvKey)
	}
	var doc StreamerDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("unmarshal doc at %s: %v", kvKey, err)
	}
	return doc
}

var (
	alice    = streamer.StreamerKey{Platform: "tiktok", Username: "alice"}
	aliceDoc = DocKVKey(alice)
)

func monitoredSeed(key streamer.StreamerKey) DocSeed {
	return DocSeed{
		ID:   streamer.StreamerRef{StreamerKey: key},
		Opts: DocOptions{Monitor: true},
	}
}

func liveStreamer(key streamer.StreamerKey) streamer.StreamerState {
	return streamer.StreamerState{
		StreamerRef: streamer.StreamerRef{StreamerKey: key},
		Status:      streamer.StatusLive,
		Streams:     stream.List{{}},
	}
}

func TestReconcileCreatesAndPrunes(t *testing.T) {
	ctx := context.Background()
	kv := newFakeKV()
	s := NewStore(kv)

	s.Reconcile(ctx, []DocSeed{monitoredSeed(alice)})

	doc := kv.doc(t, aliceDoc)
	if !doc.Monitor || doc.Activity != activityMonitoring {
		t.Errorf("got monitor=%v activity=%q, want monitored/monitoring", doc.Monitor, doc.Activity)
	}
	if doc.Status != streamer.StatusUnknown {
		t.Errorf("got status=%q, want unknown", doc.Status)
	}

	s.Reconcile(ctx, nil)
	if _, ok := kv.data[aliceDoc]; ok {
		t.Error("doc not pruned after streamer left membership")
	}
}

func TestOneShotDocOnlyWhenRecordable(t *testing.T) {
	ctx := context.Background()
	kv := newFakeKV()
	s := NewStore(kv)

	seed := DocSeed{ID: streamer.StreamerRef{StreamerKey: alice}, Opts: DocOptions{Monitor: false}}
	s.Reconcile(ctx, []DocSeed{seed})
	if _, ok := kv.data[aliceDoc]; ok {
		t.Fatal("ineligible one-shot must not get a doc")
	}

	// Offline status: still no doc.
	s.SetStatus(ctx, streamer.StreamerState{StreamerRef: streamer.StreamerRef{StreamerKey: alice}, Status: streamer.StatusOffline}, false)
	if _, ok := kv.data[aliceDoc]; ok {
		t.Fatal("offline one-shot must not get a doc")
	}

	// Recordable: doc appears, and stays through a later offline update.
	s.SetStatus(ctx, liveStreamer(alice), false)
	doc := kv.doc(t, aliceDoc)
	if doc.Monitor || doc.Status != streamer.StatusLive || doc.Activity != activityIdle {
		t.Errorf("got monitor=%v status=%q activity=%q", doc.Monitor, doc.Status, doc.Activity)
	}
	s.SetStatus(ctx, streamer.StreamerState{StreamerRef: streamer.StreamerRef{StreamerKey: alice}, Status: streamer.StatusOffline}, false)
	if kv.doc(t, aliceDoc).Status != streamer.StatusOffline {
		t.Error("one-shot doc should keep updating after it earned a row")
	}
}

func TestSetStatusMergesPartialUpdates(t *testing.T) {
	ctx := context.Background()
	kv := newFakeKV()
	s := NewStore(kv)
	s.Reconcile(ctx, []DocSeed{monitoredSeed(alice)})

	full := liveStreamer(alice)
	full.LiveMetadata = streamer.LiveMetadata{Title: "hello"}
	full.LastLiveStart = 42
	s.SetStatus(ctx, full, true)

	// A status-only update must not wipe metadata or last-live fields.
	s.SetStatus(ctx, streamer.StreamerState{StreamerRef: streamer.StreamerRef{StreamerKey: alice}, Status: streamer.StatusOffline}, true)

	doc := kv.doc(t, aliceDoc)
	if doc.Status != streamer.StatusOffline {
		t.Errorf("status = %q, want offline", doc.Status)
	}
	if doc.LiveMetadata.Title != "hello" || doc.LastLiveStart != 42 {
		t.Errorf("partial update wiped fields: %+v", doc)
	}
}

func TestSetStatusMergesAvatar(t *testing.T) {
	ctx := context.Background()
	kv := newFakeKV()
	s := NewStore(kv)
	s.Reconcile(ctx, []DocSeed{monitoredSeed(alice)})

	// An update carrying an avatar records it.
	withAvatar := liveStreamer(alice)
	withAvatar.AvatarURL = "https://img/avatar.jpg"
	s.SetStatus(ctx, withAvatar, true)
	if got := kv.doc(t, aliceDoc).AvatarURL; got != "https://img/avatar.jpg" {
		t.Fatalf("avatar = %q, want set", got)
	}

	// A later status-only update (empty avatar) must not wipe it.
	s.SetStatus(ctx, streamer.StreamerState{StreamerRef: streamer.StreamerRef{StreamerKey: alice}, Status: streamer.StatusOffline}, true)
	if got := kv.doc(t, aliceDoc).AvatarURL; got != "https://img/avatar.jpg" {
		t.Fatalf("avatar = %q, want preserved after status-only update", got)
	}
}

func TestSetAvatarURL(t *testing.T) {
	ctx := context.Background()
	kv := newFakeKV()
	s := NewStore(kv)
	s.Reconcile(ctx, []DocSeed{monitoredSeed(alice)})

	s.SetAvatarURL(ctx, alice, "https://img/a.jpg")
	if got := kv.doc(t, aliceDoc).AvatarURL; got != "https://img/a.jpg" {
		t.Fatalf("avatar = %q, want set via SetAvatarURL", got)
	}

	// Re-setting the same URL must not write again.
	puts := kv.puts
	s.SetAvatarURL(ctx, alice, "https://img/a.jpg")
	if kv.puts != puts {
		t.Errorf("unchanged avatar re-written (puts = %d, want %d)", kv.puts, puts)
	}

	// An untracked streamer must not be resurrected.
	bob := streamer.StreamerKey{Platform: "tiktok", Username: "bob"}
	s.SetAvatarURL(ctx, bob, "https://img/b.jpg")
	if _, ok := kv.data[DocKVKey(bob)]; ok {
		t.Error("SetAvatarURL resurrected an untracked streamer")
	}
}

func TestWriteOnlyOnChange(t *testing.T) {
	ctx := context.Background()
	kv := newFakeKV()
	s := NewStore(kv)
	s.Reconcile(ctx, []DocSeed{monitoredSeed(alice)})

	puts := kv.puts
	s.SetStatus(ctx, liveStreamer(alice), true)
	if kv.puts != puts+1 {
		t.Fatalf("puts = %d, want %d", kv.puts, puts+1)
	}
	s.SetStatus(ctx, liveStreamer(alice), true)
	s.Reconcile(ctx, []DocSeed{monitoredSeed(alice)})
	if kv.puts != puts+1 {
		t.Errorf("unchanged doc was re-written (puts = %d, want %d)", kv.puts, puts+1)
	}
}

func TestSessionAndProcessingActivity(t *testing.T) {
	ctx := context.Background()
	kv := newFakeKV()
	s := NewStore(kv)
	s.Reconcile(ctx, []DocSeed{monitoredSeed(alice)})

	s.SetSessionState(ctx, alice, "sess1", recording.SessionRecording)
	doc := kv.doc(t, aliceDoc)
	if doc.Activity != activityRecording || doc.ActiveSessionID != "sess1" {
		t.Errorf("got activity=%q session=%q, want recording/sess1", doc.Activity, doc.ActiveSessionID)
	}
	// The session-state key (the CLI's channel) is still written.
	if got := string(kv.data[alice.KVSessionKey("sess1")+".state"]); got != "recording" {
		t.Errorf("session state key = %q, want recording", got)
	}

	// Part conversion overlapping the recording must not demote the activity.
	s.SetProcessingState(ctx, alice, "sess1", recording.ProcessProcessing)
	if doc = kv.doc(t, aliceDoc); doc.Activity != activityRecording {
		t.Errorf("activity = %q, want recording while session active", doc.Activity)
	}

	// Session ends: processing takes over, then clears back to monitoring.
	s.SetSessionState(ctx, alice, "sess1", recording.SessionFinished)
	if doc = kv.doc(t, aliceDoc); doc.Activity != activityProcessing || doc.ActiveSessionID != "sess1" {
		t.Errorf("got activity=%q session=%q, want processing/sess1", doc.Activity, doc.ActiveSessionID)
	}
	s.SetProcessingState(ctx, alice, "sess1", recording.ProcessFinished)
	if doc = kv.doc(t, aliceDoc); doc.Activity != activityMonitoring || doc.ActiveSessionID != "" {
		t.Errorf("got activity=%q session=%q, want monitoring/none", doc.Activity, doc.ActiveSessionID)
	}
}

func TestRecordingStampsMissingLiveStart(t *testing.T) {
	ctx := context.Background()
	kv := newFakeKV()
	s := NewStore(kv)
	s.Reconcile(ctx, []DocSeed{monitoredSeed(alice)})

	// Nothing knows when the stream started: the recording's own start stands in,
	// so a recording row never reads as "last live unknown".
	s.SetSessionState(ctx, alice, "sess1", recording.SessionRecording)
	if got := kv.doc(t, aliceDoc).LastLiveStart; got == 0 {
		t.Error("recording streamer has no last-live time")
	}

	// A known start time is when the stream went live, and outranks the fallback.
	kv2 := newFakeKV()
	s2 := NewStore(kv2)
	s2.Reconcile(ctx, []DocSeed{monitoredSeed(alice)})
	live := liveStreamer(alice)
	live.LastLiveStart = 42
	s2.SetStatus(ctx, live, true)
	s2.SetSessionState(ctx, alice, "sess1", recording.SessionRecording)
	if got := kv2.doc(t, aliceDoc).LastLiveStart; got != 42 {
		t.Errorf("LastLiveStart = %d, want 42", got)
	}
}

func TestMoveActivity(t *testing.T) {
	ctx := context.Background()
	kv := newFakeKV()
	s := NewStore(kv)
	s.Reconcile(ctx, []DocSeed{monitoredSeed(alice)})

	// Recording outranks an active move.
	s.SetSessionState(ctx, alice, "sess1", recording.SessionRecording)
	s.SetMoveState(ctx, alice, "sess1", true, false)
	if doc := kv.doc(t, aliceDoc); doc.Activity != activityRecording {
		t.Errorf("activity = %q, want recording while session active", doc.Activity)
	}

	// Processing outranks moving.
	s.SetSessionState(ctx, alice, "sess1", recording.SessionFinished)
	s.SetProcessingState(ctx, alice, "sess1", recording.ProcessProcessing)
	if doc := kv.doc(t, aliceDoc); doc.Activity != activityProcessing {
		t.Errorf("activity = %q, want processing", doc.Activity)
	}

	// Processing clears: moving shows, pointing at the session being moved.
	s.SetProcessingState(ctx, alice, "sess1", recording.ProcessFinished)
	if doc := kv.doc(t, aliceDoc); doc.Activity != activityMoving || doc.ActiveSessionID != "sess1" {
		t.Errorf("got activity=%q session=%q, want moving/sess1", doc.Activity, doc.ActiveSessionID)
	}

	// A failed attempt flips to the retrying variant.
	s.SetMoveState(ctx, alice, "sess1", true, true)
	if doc := kv.doc(t, aliceDoc); doc.Activity != activityMovingRetry {
		t.Errorf("activity = %q, want moving_retrying", doc.Activity)
	}

	// Move completes: back to monitoring.
	s.SetMoveState(ctx, alice, "sess1", false, false)
	if doc := kv.doc(t, aliceDoc); doc.Activity != activityMonitoring || doc.ActiveSessionID != "" {
		t.Errorf("got activity=%q session=%q, want monitoring/none", doc.Activity, doc.ActiveSessionID)
	}
}

func TestMoveStateSecondSessionOverwrites(t *testing.T) {
	ctx := context.Background()
	kv := newFakeKV()
	s := NewStore(kv)
	s.Reconcile(ctx, []DocSeed{monitoredSeed(alice)})

	// sess1 is moving, then a newer session sess2 begins moving: latest wins,
	// mirroring how procSessionID is single-valued.
	s.SetMoveState(ctx, alice, "sess1", true, false)
	s.SetMoveState(ctx, alice, "sess2", true, false)
	if doc := kv.doc(t, aliceDoc); doc.ActiveSessionID != "sess2" {
		t.Errorf("active session = %q, want sess2", doc.ActiveSessionID)
	}

	// A clear for the stale sess1 must not wipe the current sess2 move.
	s.SetMoveState(ctx, alice, "sess1", false, false)
	if doc := kv.doc(t, aliceDoc); doc.Activity != activityMoving || doc.ActiveSessionID != "sess2" {
		t.Errorf("got activity=%q session=%q, want moving/sess2", doc.Activity, doc.ActiveSessionID)
	}
}

// TestUnresolvedURLGetsNoDoc pins that an entry the cloud never named produces
// no row at all, rather than one with no name.
func TestUnresolvedURLGetsNoDoc(t *testing.T) {
	ctx := context.Background()
	kv := newFakeKV()
	s := NewStore(kv)

	url := "https://t/alice"

	s.Reconcile(ctx, []DocSeed{{ID: streamer.StreamerRef{URL: url}, Opts: DocOptions{Monitor: true}}})
	if len(kv.data) != 0 {
		t.Errorf("url-only seed wrote %d docs, want none: %v", len(kv.data), kv.data)
	}

	// Once the cloud names it, the ordinary keyed doc appears.
	s.Reconcile(ctx, []DocSeed{monitoredSeed(alice)})
	doc := kv.doc(t, aliceDoc)
	if doc.Platform != "tiktok" || doc.Username != "alice" {
		t.Errorf("resolved doc: %+v", doc)
	}
}

func TestDeleteDropsDocAndEntry(t *testing.T) {
	ctx := context.Background()
	kv := newFakeKV()
	s := NewStore(kv)
	s.Reconcile(ctx, []DocSeed{monitoredSeed(alice)})

	s.Delete(ctx, alice)
	if _, ok := kv.data[aliceDoc]; ok {
		t.Fatal("doc not deleted")
	}
	// A later session write must not resurrect the doc from a stale entry.
	s.SetSessionState(ctx, alice, "sess1", recording.SessionRecording)
	if _, ok := kv.data[aliceDoc]; ok {
		t.Error("deleted streamer resurrected by session update")
	}
}

// fakeTracker hands out a fixed time per key, stamping unseen keys with now.
type fakeTracker struct {
	times map[streamer.StreamerKey]int64
	asked []streamer.StreamerKey
}

func (f *fakeTracker) Track(_ context.Context, keys []streamer.StreamerKey, now time.Time) map[streamer.StreamerKey]int64 {
	f.asked = append(f.asked, keys...)
	out := make(map[streamer.StreamerKey]int64, len(keys))
	for _, k := range keys {
		if _, ok := f.times[k]; !ok {
			f.times[k] = now.Unix()
		}
		out[k] = f.times[k]
	}
	return out
}

func TestReconcileCarriesAddedAt(t *testing.T) {
	ctx := context.Background()
	kv := newFakeKV()
	s := NewStore(kv)
	tracker := &fakeTracker{times: map[streamer.StreamerKey]int64{alice: 1700000000}}
	s.SetAddedTracker(tracker)

	url := "https://www.tiktok.com/@nobody/live"
	s.Reconcile(ctx, []DocSeed{
		monitoredSeed(alice),
		{ID: streamer.StreamerRef{URL: url}, Opts: DocOptions{Monitor: true}},
	})
	if got := kv.doc(t, aliceDoc).AddedAt; got != 1700000000 {
		t.Errorf("AddedAt = %d, want the tracked time", got)
	}
	if len(tracker.asked) != 1 || tracker.asked[0] != alice {
		t.Errorf("tracker asked for %v, want only the keyed seed", tracker.asked)
	}

	// An explicit re-add restarts the time while the doc is still tracked.
	tracker.times[alice] = 1800000000
	s.Reconcile(ctx, []DocSeed{monitoredSeed(alice)})
	if got := kv.doc(t, aliceDoc).AddedAt; got != 1800000000 {
		t.Errorf("AddedAt = %d after re-add, want 1800000000", got)
	}
}

func TestReconcileKeepsAddedAtWhenTrackerFails(t *testing.T) {
	ctx := context.Background()
	kv := newFakeKV()
	s := NewStore(kv)
	tracker := &fakeTracker{times: map[streamer.StreamerKey]int64{alice: 1700000000}}
	s.SetAddedTracker(tracker)
	s.Reconcile(ctx, []DocSeed{monitoredSeed(alice)})

	// A tracker that cannot answer leaves the time already known in place.
	s.SetAddedTracker(failingTracker{})
	s.Reconcile(ctx, []DocSeed{monitoredSeed(alice)})
	if got := kv.doc(t, aliceDoc).AddedAt; got != 1700000000 {
		t.Errorf("AddedAt = %d, want the earlier time kept", got)
	}
}

type failingTracker struct{}

func (failingTracker) Track(context.Context, []streamer.StreamerKey, time.Time) map[streamer.StreamerKey]int64 {
	return nil
}

package vlcli

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/urfave/cli/v3"
	"github.com/vodlabs-io/recorder/internal/progress"
	"github.com/vodlabs-io/recorder/internal/recording"
	"github.com/vodlabs-io/recorder/internal/streamer"
	"github.com/vodlabs-io/recorder/internal/vlapi"
	"github.com/vodlabs-io/recorder/internal/vlstate"
	"github.com/vodlabs-io/recorder/internal/vodindex"
)

var jobKey = streamer.StreamerKey{Platform: "twitch", Username: "someone"}

// fakeDaemon answers the requests a record job makes, over a real JetStream KV.
type fakeDaemon struct {
	t       *testing.T
	app     *App
	removed atomic.Int32
	added   atomic.Int32
	once    atomic.Int32
	// addErr is the error code streamer.add and streamer.record_once answer with.
	addErr error
	// vods is what vods.list answers with.
	vods atomic.Pointer[[]vodindex.VOD]
	// onRemove runs when streamer.remove arrives.
	onRemove func()
	// checkErr is the error code streamer.check answers with.
	checkErr atomic.Pointer[error]
}

func newFakeDaemon(t *testing.T) *fakeDaemon {
	t.Helper()
	ns, err := server.NewServer(&server.Options{
		Host: "127.0.0.1", Port: -1, NoLog: true, NoSigs: true,
		JetStream: true, StoreDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	ns.Start()
	t.Cleanup(ns.Shutdown)
	if !ns.ReadyForConnections(5 * time.Second) {
		t.Fatal("server not ready")
	}
	nc, err := nats.Connect(ns.ClientURL())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(nc.Close)
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	kv, err := js.CreateKeyValue(context.Background(), jetstream.KeyValueConfig{Bucket: "streamers"})
	if err != nil {
		t.Fatal(err)
	}

	d := &fakeDaemon{t: t, app: &App{nc: nc, kv: kv}}
	reply := func(m *nats.Msg, data any, code error) {
		var b []byte
		if code != nil {
			b, _ = json.Marshal(map[string]any{"ok": false,
				"error": map[string]string{"code": code.Error(), "message": "refused"}})
		} else {
			b, _ = json.Marshal(vlapi.OK(data))
		}
		if err := m.Respond(b); err != nil {
			t.Error(err)
		}
	}
	addHandler := func(counter *atomic.Int32) nats.MsgHandler {
		return func(m *nats.Msg) {
			counter.Add(1)
			reply(m, vlapi.AddResponse{StreamerRef: streamer.StreamerRef{StreamerKey: jobKey}}, d.addErr)
		}
	}
	subs := map[string]nats.MsgHandler{
		"streamer.add":         addHandler(&d.added),
		"streamer.record_once": addHandler(&d.once),
		"streamer.remove": func(m *nats.Msg) {
			d.removed.Add(1)
			if d.onRemove != nil {
				d.onRemove()
			}
			reply(m, "removed", nil)
		},
		"streamer.check": func(m *nats.Msg) {
			var code error
			if e := d.checkErr.Load(); e != nil {
				code = *e
			}
			reply(m, "offline", code)
		},
		"vods.list": func(m *nats.Msg) {
			page := vodListPage{}
			if v := d.vods.Load(); v != nil {
				page.Sessions = *v
			}
			reply(m, page, nil)
		},
	}
	for subject, h := range subs {
		if _, err := nc.Subscribe(subject, h); err != nil {
			t.Fatal(err)
		}
	}
	return d
}

func (d *fakeDaemon) job(wait, duration time.Duration) *recordJob {
	return &recordJob{
		app: d.app, ref: streamer.StreamerRef{StreamerKey: jobKey},
		wait: wait, duration: duration,
		finalizeTimeout:    time.Second,
		interruptedTimeout: time.Second,
		missingVODGrace:    300 * time.Millisecond,
		finalizePoll:       10 * time.Millisecond,
		checkDelays:        []time.Duration{0, 10 * time.Millisecond},
	}
}

func (d *fakeDaemon) setSession(id string, state recording.SessionState) {
	d.t.Helper()
	k := jobKey.KVSessionKey(id) + ".state"
	if _, err := d.app.kv.Put(context.Background(), k, []byte(state)); err != nil {
		d.t.Fatal(err)
	}
}

func (d *fakeDaemon) setStatus(status streamer.LiveStatus) {
	d.t.Helper()
	b, _ := json.Marshal(vlstate.StreamerDoc{Status: status})
	if _, err := d.app.kv.Put(context.Background(), vlstate.DocKVKey(jobKey), b); err != nil {
		d.t.Fatal(err)
	}
}

func (d *fakeDaemon) setVOD(id, state string) {
	d.vods.Store(&[]vodindex.VOD{{
		Dir: "/rec/" + id, SessionID: id, State: state, TotalBytes: 42, DurationSec: 7,
		Files: []vodindex.File{{Name: "vod.mp4", Bytes: 42}},
	}})
}

func runJob(t *testing.T, ctx context.Context, j *recordJob) recordResult {
	t.Helper()
	done := make(chan recordResult, 1)
	go func() { done <- j.run(ctx) }()
	select {
	case res := <-done:
		return res
	case <-time.After(10 * time.Second):
		t.Fatal("job did not finish")
		return recordResult{}
	}
}

// A stream that is live and ends by itself is reported with its final files.
func TestRecordJob_RecordsToTheEnd(t *testing.T) {
	d := newFakeDaemon(t)
	d.setVOD("s1", vodindex.StateReady)
	d.setSession("s1", recording.SessionRecording)
	time.AfterFunc(100*time.Millisecond, func() { d.setSession("s1", recording.SessionFinished) })

	res := runJob(t, context.Background(), d.job(5*time.Second, 0))
	if res.Outcome != outcomeRecorded || !res.Ready || res.Stopped {
		t.Fatalf("result = %+v", res)
	}
	if len(res.Paths) != 1 || res.Paths[0] != "/rec/s1/vod.mp4" {
		t.Fatalf("paths = %v", res.Paths)
	}
	if d.once.Load() != 1 || d.added.Load() != 0 {
		t.Fatalf("want a one-shot add, got once=%d add=%d", d.once.Load(), d.added.Load())
	}
	if d.removed.Load() != 0 {
		t.Fatal("a finished one-shot cleans up after itself")
	}
}

// A short wait that runs out leaves nothing behind and exits as not live.
func TestRecordJob_WaitRunsOut(t *testing.T) {
	d := newFakeDaemon(t)
	res := runJob(t, context.Background(), d.job(50*time.Millisecond, 0))
	if res.Outcome != outcomeNotLive || res.Stopped {
		t.Fatalf("result = %+v", res)
	}
	if d.removed.Load() != 1 {
		t.Fatalf("removed %d times, want 1", d.removed.Load())
	}
	var ec cli.ExitCoder
	if err := exitFor(res, ""); !errors.As(err, &ec) || ec.ExitCode() != exitNotLive {
		t.Fatalf("exit = %v", err)
	}
}

// A one-shot takes offline as final; a long wait sits through it until the
// stream starts, then removes the rule it added.
func TestRecordJob_LongWaitOutlastsOffline(t *testing.T) {
	d := newFakeDaemon(t)
	d.setStatus(streamer.StatusOffline)
	if res := runJob(t, context.Background(), d.job(time.Second, 0)); res.Outcome != outcomeNotLive {
		t.Fatalf("one-shot result = %+v", res)
	}

	d = newFakeDaemon(t)
	d.setStatus(streamer.StatusOffline)
	d.setVOD("s1", vodindex.StateReady)
	time.AfterFunc(100*time.Millisecond, func() { d.setSession("s1", recording.SessionRecording) })
	time.AfterFunc(200*time.Millisecond, func() { d.setSession("s1", recording.SessionFinished) })

	res := runJob(t, context.Background(), d.job(recordableTimeout+time.Second, 0))
	if res.Outcome != outcomeRecorded {
		t.Fatalf("result = %+v", res)
	}
	if d.added.Load() != 1 || d.once.Load() != 0 {
		t.Fatalf("want a monitored add, got once=%d add=%d", d.once.Load(), d.added.Load())
	}
	if d.removed.Load() != 1 {
		t.Fatalf("removed %d times, want 1", d.removed.Load())
	}
}

// --duration stops a running session, and its files are still collected even
// though stopping purges its state.
func TestRecordJob_DurationStopsAndCollects(t *testing.T) {
	d := newFakeDaemon(t)
	d.setSession("s1", recording.SessionRecording)
	d.onRemove = func() {
		if err := vlstate.DeleteStreamer(context.Background(), d.app.kv, jobKey); err != nil {
			t.Error(err)
		}
		d.setVOD("s1", vodindex.StateProcessing)
		time.AfterFunc(50*time.Millisecond, func() { d.setVOD("s1", vodindex.StateReady) })
	}

	res := runJob(t, context.Background(), d.job(5*time.Second, 50*time.Millisecond))
	if res.Outcome != outcomeRecorded || !res.Stopped || !res.Ready {
		t.Fatalf("result = %+v", res)
	}
	if d.removed.Load() != 1 {
		t.Fatalf("removed %d times, want 1", d.removed.Load())
	}
}

// A signal stops the recording rather than leaving it running unattended.
func TestRecordJob_InterruptStopsRecording(t *testing.T) {
	d := newFakeDaemon(t)
	d.setVOD("s1", vodindex.StateReady)
	d.setSession("s1", recording.SessionRecording)
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)

	res := runJob(t, ctx, d.job(5*time.Second, 0))
	if res.Outcome != outcomeRecorded || !res.Stopped {
		t.Fatalf("result = %+v", res)
	}
	if d.removed.Load() != 1 {
		t.Fatalf("removed %d times, want 1", d.removed.Load())
	}
}

// An end state left by an earlier session is not this job's result.
func TestRecordJob_IgnoresEarlierSession(t *testing.T) {
	d := newFakeDaemon(t)
	d.setSession("old", recording.SessionFinished)
	d.setVOD("new", vodindex.StateReady)
	time.AfterFunc(50*time.Millisecond, func() { d.setSession("new", recording.SessionRecording) })
	time.AfterFunc(100*time.Millisecond, func() { d.setSession("new", recording.SessionFinished) })

	res := runJob(t, context.Background(), d.job(5*time.Second, 0))
	if res.SessionID != "new" || res.Outcome != outcomeRecorded {
		t.Fatalf("result = %+v", res)
	}
}

func TestRecordJob_FailedSessionIsAFailure(t *testing.T) {
	d := newFakeDaemon(t)
	d.setVOD("s1", vodindex.StateReady)
	d.setSession("s1", recording.SessionRecording)
	time.AfterFunc(50*time.Millisecond, func() { d.setSession("s1", recording.SessionFailed) })

	res := runJob(t, context.Background(), d.job(5*time.Second, 0))
	if res.Outcome != outcomeFailed || res.State != string(recording.SessionFailed) {
		t.Fatalf("result = %+v", res)
	}
	if len(res.Paths) != 1 {
		t.Fatalf("a failed session still reports what it kept: %+v", res)
	}
}

// A session that left no files is a failure, reported without waiting out the
// whole finalize timeout.
func TestRecordJob_NoFiles(t *testing.T) {
	d := newFakeDaemon(t)
	d.setSession("s1", recording.SessionRecording)
	time.AfterFunc(50*time.Millisecond, func() { d.setSession("s1", recording.SessionFinished) })

	res := runJob(t, context.Background(), d.job(5*time.Second, 0))
	if res.Outcome != outcomeFailed || !strings.Contains(res.Error, "no files") {
		t.Fatalf("result = %+v", res)
	}
}

func TestRecordJob_RefusedAddIsReported(t *testing.T) {
	d := newFakeDaemon(t)
	d.addErr = vlapi.ErrConflict
	res := runJob(t, context.Background(), d.job(time.Second, 0))
	if res.Outcome != outcomeError || !strings.Contains(res.Error, "streamer remove") {
		t.Fatalf("result = %+v", res)
	}
}

// Sessions is always a list, so a caller can iterate it without a null check.
func TestStatusSnapshot_EmptySessionsIsAList(t *testing.T) {
	b, err := json.Marshal(newStatusSnapshot(progress.DisplayState{SubscriptionCount: 3}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"sessions":[]`) {
		t.Fatalf("json = %s", b)
	}
}

// A machine that cannot reach the platform is told so straight away, rather
// than after waiting out --wait.
func TestRecordJob_UnreachablePlatformEndsTheWait(t *testing.T) {
	d := newFakeDaemon(t)
	unreachable := vlapi.ErrPlatformUnreachable
	d.checkErr.Store(&unreachable)

	start := time.Now()
	res := runJob(t, context.Background(), d.job(recordableTimeout+time.Minute, 0))
	if res.Outcome != outcomeUnreachable || res.Error != "refused" {
		t.Fatalf("result = %+v", res)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("waited out --wait")
	}
	if d.removed.Load() != 1 {
		t.Fatalf("removed %d times, want 1", d.removed.Load())
	}
}

// Any other failed check is not a reason to give up waiting.
func TestRecordJob_OtherCheckFailureKeepsWaiting(t *testing.T) {
	d := newFakeDaemon(t)
	unavailable := errors.New("unavailable")
	d.checkErr.Store(&unavailable)
	d.setVOD("s1", vodindex.StateReady)
	time.AfterFunc(200*time.Millisecond, func() { d.setSession("s1", recording.SessionRecording) })
	time.AfterFunc(300*time.Millisecond, func() { d.setSession("s1", recording.SessionFinished) })

	res := runJob(t, context.Background(), d.job(5*time.Second, 0))
	if res.Outcome != outcomeRecorded {
		t.Fatalf("result = %+v", res)
	}
}

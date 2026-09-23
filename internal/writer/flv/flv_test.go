package flv

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vodlabs-io/recorder/internal/flvfix"
	"github.com/vodlabs-io/recorder/internal/vlerror"
)

// ---- synthetic FLV ----

func fileHeader() []byte { return []byte{'F', 'L', 'V', 0x01, 0x05, 0, 0, 0, 9, 0, 0, 0, 0} }

func buildTag(typ byte, ts int64, payload []byte) []byte {
	size := len(payload)
	b := []byte{typ,
		byte(size >> 16), byte(size >> 8), byte(size),
		byte(ts >> 16), byte(ts >> 8), byte(ts), byte(ts >> 24),
		0, 0, 0}
	b = append(b, payload...)
	prev := uint32(11 + size)
	return append(b, byte(prev>>24), byte(prev>>16), byte(prev>>8), byte(prev))
}

// avcSeqHdr builds a minimal AVC sequence header; res stands in for the SPS
// bits that a resolution change moves.
func avcSeqHdr(res byte) []byte {
	sps := []byte{0x67, 100, 0, 30, 0xac, res, 0x40, 0x51, 0xa6}
	pps := []byte{0x68, 0xeb, 0xec, 0xb2}
	p := []byte{0x17, 0x00, 0, 0, 0,
		0x01, 100, 0, 30, 0xff, 0xe1,
		byte(len(sps) >> 8), byte(len(sps))}
	p = append(p, sps...)
	p = append(p, 0x01, byte(len(pps)>>8), byte(len(pps)))
	return append(p, pps...)
}

func vCfg(ts int64, res byte) []byte { return buildTag(9, ts, avcSeqHdr(res)) }
func aCfg(ts int64) []byte           { return buildTag(8, ts, []byte{0xaf, 0x00, 0x12, 0x10}) }

// gop emits a keyframe at start then interleaved audio/video to end.
func gop(start, end int64) []byte {
	b := buildTag(9, start, []byte{0x17, 0x01, 0, 0, 0, 0xde, 0xad, 0xbe, 0xef})
	for ts := start; ts < end; ts += 20 {
		b = append(b, buildTag(8, ts, []byte{0xaf, 0x01, 0x21, 0x42})...)
		if v := ts + 20; v%40 == 0 && v > start && v < end {
			b = append(b, buildTag(9, v, []byte{0x27, 0x01, 0, 0, 0, 0xca, 0xfe})...)
		}
	}
	return b
}

// videoConfigs returns the res byte of every AVC sequence header in a
// sanitized output file, in order.
func videoConfigs(t *testing.T, path string) []byte {
	t.Helper()
	out, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if len(out) < 13 {
		t.Fatalf("%s: too short to be an FLV (%d bytes)", filepath.Base(path), len(out))
	}
	var res []byte
	for pos := 13; pos+15 <= len(out); {
		size := int(out[pos+1])<<16 | int(out[pos+2])<<8 | int(out[pos+3])
		end := pos + 11 + size
		if end+4 > len(out) {
			t.Fatalf("%s: tag at %d overruns file", filepath.Base(path), pos)
		}
		p := out[pos+11 : end]
		if out[pos] == 9 && len(p) > 18 && p[0] == 0x17 && p[1] == 0x00 {
			res = append(res, p[18]) // sps[5], the res byte above
		}
		pos = end + 4
	}
	return res
}

// recordPart runs one part against cfg and returns its error.
func recordPart(t *testing.T, ctx context.Context, cfg Config, name string) error {
	t.Helper()
	cfg.FileName = name
	progress := make(chan int64, 256)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range progress {
		}
	}()
	err := Record(ctx, cfg, http.DefaultClient, slog.New(slog.DiscardHandler), progress)
	close(progress)
	<-done
	return err
}

// TestHandoffKeepsStreamAcrossConfigChange checks that the sequence header
// announcing a new configuration, buffered in the sanitizer when the part ends,
// reaches the next part: dropping either the sanitizer or the connection would
// lose it.
func TestHandoffKeepsStreamAcrossConfigChange(t *testing.T) {
	src := append(fileHeader(), gop(0, 0)[:0]...)
	src = append(src, vCfg(0, 0x41)...)
	src = append(src, aCfg(0)...)
	src = append(src, gop(0, 2000)...)
	src = append(src, vCfg(2000, 0x99)...) // the switch
	src = append(src, gop(2000, 4000)...)

	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) > 1 {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Write(src)
	}))
	defer srv.Close()

	dir := t.TempDir()
	handoff := &Handoff{}
	defer handoff.Close()
	var keys []string
	cfg := Config{
		URL: srv.URL, OutDir: dir, Sanitize: true, Handoff: handoff,
		Retries: 0, ReconnectDelay: time.Millisecond,
		OnConfigKey: func(k string) { keys = append(keys, k) },
	}
	ctx := t.Context()

	// Part 1 ends on the configuration change and hands the stream on.
	if err := recordPart(t, ctx, cfg, "pt1.ts"); !errors.Is(err, flvfix.ErrConfigChange) {
		t.Fatalf("part 1: want ErrConfigChange, got %v", err)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("part 1 made %d requests, want 1 (a rotation must not reconnect)", got)
	}
	if handoff.sanitizer == nil || handoff.stream == nil {
		t.Fatal("part 1 did not hand the stream on")
	}

	// Part 2 reads on from the same response; it ends only because the server
	// refuses the re-open after EOF.
	if err := recordPart(t, ctx, cfg, "pt2.ts"); err == nil {
		t.Fatal("part 2: want the re-open to fail after EOF")
	}

	cfg1 := videoConfigs(t, filepath.Join(dir, "pt1.ts"))
	cfg2 := videoConfigs(t, filepath.Join(dir, "pt2.ts"))
	if len(cfg1) != 1 || cfg1[0] != 0x41 {
		t.Fatalf("part 1 video configs = %x, want one 0x41", cfg1)
	}
	if len(cfg2) != 1 || cfg2[0] != 0x99 {
		t.Fatalf("part 2 video configs = %x, want one 0x99 (the announced switch)", cfg2)
	}
	if len(keys) != 2 || keys[0] == "" || keys[0] == keys[1] {
		t.Fatalf("config keys = %q, want two distinct non-empty keys", keys)
	}
	// The second part must hold the media that followed the switch, not a
	// fresh Origin entry point.
	st2, err := os.Stat(filepath.Join(dir, "pt2.ts"))
	if err != nil {
		t.Fatal(err)
	}
	if st2.Size() < int64(len(gop(2000, 4000)))/2 {
		t.Fatalf("part 2 is %d bytes, too small to hold the post-switch GOP", st2.Size())
	}
}

// TestConfigChangeWithoutHandoffReconnects pins the fallback, and with it the
// reason the Handoff exists: without one the next part re-enters the stream
// from the Origin's current entry point, so it records the old configuration
// again and the announced switch is gone.
func TestConfigChangeWithoutHandoffReconnects(t *testing.T) {
	src := append(fileHeader(), vCfg(0, 0x41)...)
	src = append(src, aCfg(0)...)
	src = append(src, gop(0, 2000)...)
	src = append(src, vCfg(2000, 0x99)...)
	src = append(src, gop(2000, 4000)...)

	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Write(src)
	}))
	defer srv.Close()

	dir := t.TempDir()
	cfg := Config{
		URL: srv.URL, OutDir: dir, Sanitize: true,
		Retries: 0, ReconnectDelay: time.Millisecond,
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	if err := recordPart(t, ctx, cfg, "pt1.ts"); !errors.Is(err, flvfix.ErrConfigChange) {
		t.Fatalf("want ErrConfigChange, got %v", err)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("part 1 made %d requests, want 1", got)
	}
	go func() { time.Sleep(200 * time.Millisecond); cancel() }()
	_ = recordPart(t, ctx, cfg, "pt2.ts")
	if got := requests.Load(); got < 2 {
		t.Fatalf("part 2 made no new request, want a reconnect")
	}
	if got := videoConfigs(t, filepath.Join(dir, "pt2.ts")); len(got) == 0 || got[0] != 0x41 {
		t.Fatalf("part 2 video configs = %x, want the reconnect to replay 0x41", got)
	}
}

// TestRefusedReconnectIsConnectFailure pins the error the session worker keys
// its recovery on: a stream that drops and then refuses every reconnect ends
// the part with ErrConnectFailed, so the next attempt starts from a fresh URL.
func TestRefusedReconnectIsConnectFailure(t *testing.T) {
	src := append(fileHeader(), vCfg(0, 0x41)...)
	src = append(src, aCfg(0)...)
	src = append(src, gop(0, 2000)...)

	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) > 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Write(src)
	}))
	defer srv.Close()

	cfg := Config{
		URL: srv.URL, OutDir: t.TempDir(), Sanitize: true,
		Retries: 2, ReconnectDelay: time.Millisecond,
	}
	err := recordPart(t, t.Context(), cfg, "pt1.ts")
	if !errors.Is(err, vlerror.ErrConnectFailed) {
		t.Fatalf("want ErrConnectFailed, got %v", err)
	}
	if got := requests.Load(); got != 4 {
		t.Fatalf("made %d requests, want the stream plus 3 refused reconnects", got)
	}
}

// TestCancelledPartAbortsStream guards the detached request context: the
// response outlives the part only for a rotation, never for a part the session
// stopped (a stall, a split limit, a shutdown).
func TestCancelledPartAbortsStream(t *testing.T) {
	hang := make(chan struct{})
	defer close(hang)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(fileHeader())
		w.(http.Flusher).Flush()
		select {
		case <-hang:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()

	handoff := &Handoff{}
	defer handoff.Close()
	cfg := Config{
		URL: srv.URL, OutDir: t.TempDir(), Sanitize: true, Handoff: handoff,
		Retries: 0, ReconnectDelay: time.Millisecond,
	}
	ctx, cancel := context.WithCancel(t.Context())
	go func() { time.Sleep(100 * time.Millisecond); cancel() }()

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = recordPart(t, ctx, cfg, "pt1.ts")
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("cancelling the part did not abort the stream")
	}
	if handoff.stream != nil {
		t.Fatal("a cancelled part must not hand its stream on")
	}
}

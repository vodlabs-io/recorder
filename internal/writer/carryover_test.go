package writer

import (
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/vodlabs-io/recorder/internal/flvfix"
)

// twoConfigFLV is the smallest stream that changes codec configuration: a
// header, a config, a keyframe, then a different config and another keyframe.
func twoConfigFLV() []byte {
	tag := func(typ byte, ts int64, payload []byte) []byte {
		size := len(payload)
		b := []byte{typ,
			byte(size >> 16), byte(size >> 8), byte(size),
			byte(ts >> 16), byte(ts >> 8), byte(ts), byte(ts >> 24), 0, 0, 0}
		b = append(b, payload...)
		prev := uint32(11 + size)
		return append(b, byte(prev>>24), byte(prev>>16), byte(prev>>8), byte(prev))
	}
	cfg := func(res byte) []byte {
		sps := []byte{0x67, 100, 0, 30, 0xac, res, 0x40, 0x51, 0xa6}
		pps := []byte{0x68, 0xeb, 0xec, 0xb2}
		p := []byte{0x17, 0x00, 0, 0, 0, 0x01, 100, 0, 30, 0xff, 0xe1,
			byte(len(sps) >> 8), byte(len(sps))}
		p = append(p, sps...)
		p = append(p, 0x01, byte(len(pps)>>8), byte(len(pps)))
		return append(p, pps...)
	}
	key := func(ts int64) []byte {
		return tag(9, ts, []byte{0x17, 0x01, 0, 0, 0, 0xde, 0xad, 0xbe, 0xef})
	}
	out := []byte{'F', 'L', 'V', 0x01, 0x05, 0, 0, 0, 9, 0, 0, 0, 0}
	out = append(out, tag(9, 0, cfg(0x41))...)
	out = append(out, tag(8, 0, []byte{0xaf, 0x00, 0x12, 0x10})...)
	out = append(out, key(0)...)
	out = append(out, tag(8, 20, []byte{0xaf, 0x01, 0x21, 0x42})...)
	out = append(out, tag(9, 1000, cfg(0x99))...)
	out = append(out, key(1000)...)
	out = append(out, tag(8, 1020, []byte{0xaf, 0x01, 0x21, 0x42})...)
	return out
}

// TestCarryoverThreadsHandoffToTheFLVWriter closes the gap between the Session
// and the FLV writer. The Config is rebuilt for every Part, so the carry-over
// only works if the Session's Carryover reaches flv.Record — and when it does
// not, nothing fails: recording just reconnects at every configuration change
// and loses the sequence header that announced it.
func TestCarryoverThreadsHandoffToTheFLVWriter(t *testing.T) {
	src := twoConfigFLV()
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) > 1 {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Write(src)
	}))
	defer srv.Close()

	wr, ok := Get("flvwriter")
	if !ok {
		t.Fatal("flvwriter not registered")
	}
	carry := NewCarryover()
	defer carry.Close()
	dir := t.TempDir()

	record := func(stem string) error {
		// Exactly what the session does per part: a fresh Config carrying the
		// session's Carryover.
		part, err := wr.PreparePart(dir, stem)
		if err != nil {
			t.Fatal(err)
		}
		cfg := &Config{StreamURL: srv.URL, Sanitize: true, Carryover: carry}
		progress := make(chan int64, 256)
		go func() {
			for range progress {
			}
		}()
		defer close(progress)
		return wr.Record(t.Context(), part, cfg, http.DefaultClient,
			slog.New(slog.DiscardHandler), progress)
	}

	if err := record("pt1"); !errors.Is(err, flvfix.ErrConfigChange) {
		t.Fatalf("part 1: want ErrConfigChange, got %v", err)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("part 1 made %d requests, want 1", got)
	}
	if carry.flvHandoff.Parked() != true {
		t.Fatal("the Carryover holds no stream: it did not reach the FLV writer")
	}

	_ = record("pt2")
	if got := requests.Load(); got != 2 {
		t.Fatalf("total requests = %d; part 2 must read on from part 1's stream "+
			"and only re-open once it ends", got)
	}
}

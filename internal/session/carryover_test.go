package session

import (
	"testing"

	"github.com/vodlabs-io/recorder/internal/stream"
	"github.com/vodlabs-io/recorder/internal/streamer"
)

// TestInitPartKeepsCarryover pins the lifetime that makes the FLV writer's
// stream carry-over work at all: writerCfg is rebuilt for every part, so the
// state that has to span parts must come from the worker, not the config.
// Losing it is silent — recording still succeeds, it just reconnects at every
// codec-configuration change and drops the sequence header announcing it.
func TestInitPartKeepsCarryover(t *testing.T) {
	w, _ := emitWorker(t, "flvwriter")
	w.output.BaseOutDir = t.TempDir()
	w.selectedStream = &stream.Stream{QualityID: "hd", URL: "https://example.invalid/s.flv"}
	w.str.Store(&streamer.StreamerState{})

	if err := w.initPart(); err != nil {
		t.Fatalf("initPart: %v", err)
	}
	first := w.writerCfg
	if first.Carryover == nil {
		t.Fatal("the first part's config carries no Carryover")
	}

	w.output.PartIndex++
	if err := w.initPart(); err != nil {
		t.Fatalf("initPart: %v", err)
	}
	if w.writerCfg == first {
		t.Fatal("test assumes writerCfg is rebuilt per part; it was not")
	}
	if w.writerCfg.Carryover != first.Carryover {
		t.Fatal("each part got its own Carryover: nothing can span a part boundary")
	}

	if w.carryover != first.Carryover {
		t.Fatal("the Carryover must be the worker's, so it lives as long as the session")
	}
}

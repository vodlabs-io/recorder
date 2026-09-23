package encoding

import (
	"context"
	"testing"

	"github.com/vodlabs-io/recorder/internal/vodindex"
)

func TestActiveRecordingWorkStateMapping(t *testing.T) {
	s := newTestService(t)
	ctx := context.Background()

	insertJob(t, s, "/s/queued", statusQueued, "")
	insertJob(t, s, "/s/encoding", statusRunning, stageEncode)
	insertJob(t, s, "/s/copying", statusRunning, stageCopy)
	insertJob(t, s, "/s/finalizing", statusRunning, stageFinalize)
	insertJob(t, s, "/s/done", statusDone, "") // terminal — must not surface

	work := s.ActiveVODWork(ctx)
	byDir := map[string]string{}
	for _, w := range work {
		byDir[w.BaseOutDir] = w.State
	}

	if len(work) != 4 {
		t.Fatalf("got %d active items, want 4 (terminal excluded)", len(work))
	}
	if byDir["/s/queued"] != vodindex.StateEncoding {
		t.Errorf("queued -> %q, want %q", byDir["/s/queued"], vodindex.StateEncoding)
	}
	if byDir["/s/encoding"] != vodindex.StateEncoding {
		t.Errorf("running/encode -> %q, want %q", byDir["/s/encoding"], vodindex.StateEncoding)
	}
	// Copy stage reuses the existing "moving" badge.
	if byDir["/s/copying"] != vodindex.StateMoving {
		t.Errorf("running/copy -> %q, want %q", byDir["/s/copying"], vodindex.StateMoving)
	}
	if byDir["/s/finalizing"] != vodindex.StateEncoding {
		t.Errorf("running/finalize -> %q, want %q", byDir["/s/finalizing"], vodindex.StateEncoding)
	}
	if _, ok := byDir["/s/done"]; ok {
		t.Errorf("terminal job should not surface as active work")
	}
}

func TestSessionBusy(t *testing.T) {
	s := newTestService(t)
	ctx := context.Background()

	insertJob(t, s, "/s/busy", statusQueued, "")
	insertJob(t, s, "/s/idle", statusDone, "")

	if !s.SessionBusy(ctx, "/s/busy") {
		t.Error("queued session should be busy")
	}
	if s.SessionBusy(ctx, "/s/idle") {
		t.Error("session with only a terminal job should not be busy")
	}
	if s.SessionBusy(ctx, "/s/unknown") {
		t.Error("unknown session should not be busy")
	}
}

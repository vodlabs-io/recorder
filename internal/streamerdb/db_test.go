package streamerdb

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/vodlabs-io/recorder/internal/streamer"
	"github.com/vodlabs-io/recorder/internal/vlconfig"
	"github.com/vodlabs-io/recorder/internal/vllog"
)

var (
	alice = streamer.StreamerKey{Platform: "tiktok", Username: "alice"}
	bob   = streamer.StreamerKey{Platform: "twitch", Username: "bob"}
)

func openTestDB(t *testing.T, dir string) *DB {
	t.Helper()
	logger, err := vllog.NewFileLogger(vlconfig.LoggerConfig{Mode: "off", Level: "error"}, "test")
	if err != nil {
		t.Fatalf("logger: %v", err)
	}
	db, err := Open(context.Background(), logger, filepath.Join(dir, "processing.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestTrackStampsOnceAndSurvivesReopen(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	first := time.Unix(1700000000, 0)
	later := time.Unix(1800000000, 0)

	db := openTestDB(t, dir)
	got := db.Track(ctx, []streamer.StreamerKey{alice}, first)
	if got[alice] != first.Unix() {
		t.Fatalf("first track: got %v, want alice at %d", got, first.Unix())
	}
	db.Close()

	db = openTestDB(t, dir)
	got = db.Track(ctx, []streamer.StreamerKey{alice, bob}, later)
	if got[alice] != first.Unix() {
		t.Errorf("alice = %d after reopen, want the first time %d", got[alice], first.Unix())
	}
	if got[bob] != later.Unix() {
		t.Errorf("bob = %d, want newly stamped %d", got[bob], later.Unix())
	}
}

func TestTrackKeepsTimeOfStreamerThatDroppedOut(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t, t.TempDir())
	first := time.Unix(1700000000, 0)

	db.Track(ctx, []streamer.StreamerKey{alice, bob}, first)
	// A cycle without alice must not forget her.
	got := db.Track(ctx, []streamer.StreamerKey{bob}, first.Add(time.Hour))
	if _, ok := got[alice]; ok {
		t.Errorf("got %v, want only the keys asked for", got)
	}
	got = db.Track(ctx, []streamer.StreamerKey{alice, bob}, first.Add(2*time.Hour))
	if got[alice] != first.Unix() {
		t.Errorf("alice = %d, want the original %d", got[alice], first.Unix())
	}
}

func TestMarkAddedRestartsTime(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t, t.TempDir())
	first := time.Unix(1700000000, 0)
	readd := time.Unix(1800000000, 0)

	db.Track(ctx, []streamer.StreamerKey{alice}, first)
	db.MarkAdded(ctx, alice, readd)
	got := db.Track(ctx, []streamer.StreamerKey{alice}, readd.Add(time.Hour))
	if got[alice] != readd.Unix() {
		t.Errorf("alice = %d, want the re-add time %d", got[alice], readd.Unix())
	}
}

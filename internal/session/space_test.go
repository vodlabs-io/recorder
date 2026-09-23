package session

import (
	"context"
	"errors"
	"testing"

	"github.com/vodlabs-io/recorder/internal/diskspace"
	"github.com/vodlabs-io/recorder/internal/streamer"
	"github.com/vodlabs-io/recorder/internal/vlconfig"
	"github.com/vodlabs-io/recorder/internal/vllog"
)

const gib = 1 << 30

type fixedSampler struct {
	usage diskspace.Usage
	err   error
}

func (f fixedSampler) Sample(string) (diskspace.Usage, error) {
	if f.err != nil {
		return diskspace.Usage{}, f.err
	}
	return f.usage, nil
}

func testRegistry(t *testing.T, s diskspace.Sampler) *Registry {
	t.Helper()
	logger, err := vllog.NewFileLogger(vlconfig.LoggerConfig{Mode: "off", Level: "error"}, "test")
	if err != nil {
		t.Fatalf("logger: %v", err)
	}
	t.Cleanup(func() { logger.Close() })
	return &Registry{
		logger:  logger,
		cfg:     vlconfig.SessionConfig{DownloadDir: t.TempDir()},
		sampler: s,
	}
}

func TestCanStartSession(t *testing.T) {
	key := streamer.StreamerKey{Platform: "twitch", Username: "someone"}
	tests := []struct {
		name    string
		sampler diskspace.Sampler
		want    bool
	}{
		{"plenty of room", fixedSampler{usage: diskspace.Usage{FreeBytes: 500 * gib}}, true},
		{"at the start floor", fixedSampler{usage: diskspace.Usage{FreeBytes: diskspace.StartFloorBytes}}, true},
		{"below the start floor", fixedSampler{usage: diskspace.Usage{FreeBytes: diskspace.StartFloorBytes - 1}}, false},
		{"empty disk", fixedSampler{usage: diskspace.Usage{FreeBytes: 0}}, false},
		// A probe that fails is a reporting problem, not a full disk. Refusing
		// every recording because of it would turn it into lost Broadcasts.
		{"probe failed", fixedSampler{err: errors.New("cannot stat")}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := testRegistry(t, tt.sampler)
			if got := r.canStartSession(context.Background(), key); got != tt.want {
				t.Errorf("canStartSession = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestStopForSpaceIsANoOpWithNoSessions(t *testing.T) {
	r := testRegistry(t, fixedSampler{usage: diskspace.Usage{FreeBytes: 0}})
	r.workers = map[streamer.StreamerKey]*worker{}
	if got := r.StopForSpace(); got != 0 {
		t.Errorf("StopForSpace = %d, want 0", got)
	}
}

func TestStartAndStopFloorsLeaveAHysteresisGap(t *testing.T) {
	// The gap is what stops Sessions churning when free space hovers at the
	// limit: below the stop floor they are cancelled, but they do not restart
	// until space clears the higher start floor.
	if diskspace.StopFloorBytes >= diskspace.StartFloorBytes {
		t.Fatalf("stop floor %d must be below start floor %d",
			uint64(diskspace.StopFloorBytes), uint64(diskspace.StartFloorBytes))
	}
	mid := diskspace.Usage{FreeBytes: (diskspace.StartFloorBytes + diskspace.StopFloorBytes) / 2}
	if diskspace.CanStart(mid) {
		t.Error("a session may start inside the hysteresis gap")
	}
	if diskspace.MustStop(mid) {
		t.Error("a session is stopped inside the hysteresis gap")
	}
}

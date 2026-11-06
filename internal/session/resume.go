package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/vodlabs-io/recorder/internal/recording"
	"github.com/vodlabs-io/recorder/internal/streamer"
	"github.com/vodlabs-io/recorder/internal/writer/hlsingest"
)

// A session the recorder stops while its stream is still live is interrupted
// rather than finished: its state stays on disk as a checkpoint, and a session
// that starts for the same streamer within the resume window carries it on —
// same session ID, folder, part numbering, groups and clock — so the VOD comes
// out as one file with a gap where the recorder was down. The rule is that a
// resumed session produces what a recorder that never stopped would have,
// minus the footage it missed: a broadcast the VodLabs service saw end and
// start again in between is a new VOD, as it would have been anyway.
//
// While a session records, its checkpoint is rewritten at every part boundary.
// One still naming a part in progress when the daemon starts belongs to a
// daemon that stopped without warning (a crash, a power cut, a kill); the part
// it was recording is measured from disk and kept.

// checkpointVersion is bumped when the checkpoint changes incompatibly. A
// checkpoint of another version is finished rather than resumed.
const checkpointVersion = 1

// checkpointDirName is the folder under the config dir holding checkpoints.
const checkpointDirName = "sessions"

// stopReason is why a session's context was cancelled.
type stopReason int32

const (
	stopShutdown    stopReason = iota // the daemon is shutting down
	stopSwitchedOff                   // the streamer's record switch was turned off
	stopRemoved                       // the streamer was removed
)

func (r stopReason) String() string {
	switch r {
	case stopSwitchedOff:
		return "switched_off"
	case stopRemoved:
		return "removed"
	}
	return "shutdown"
}

// reasonCrashed marks a checkpoint recovered from a daemon that stopped without
// warning.
const reasonCrashed = "crashed"

// checkpoint is a session's state as kept on disk: enough to carry the session
// on after the recorder stops, or to finish it if it never resumes.
type checkpoint struct {
	Version    int    `json:"version"`
	Platform   string `json:"platform"`
	Username   string `json:"username"`
	SessionID  string `json:"session_id"`
	BaseOutDir string `json:"base_out_dir"`
	Writer     string `json:"writer"`
	// The session's own settings, kept across a resume.
	Variant       string `json:"variant"`
	SplitSize     string `json:"split_size,omitempty"`
	SplitDuration string `json:"split_duration,omitempty"`
	Sanitize      bool   `json:"sanitize,omitempty"`
	// Started anchors the recording clock.
	Started    time.Time `json:"started"`
	LastByteAt time.Time `json:"last_byte_at,omitzero"`
	// LiveStart is the VodLabs service's start time for the broadcast, as last
	// seen: a different one later means the service saw it end and a new one
	// begin. Zero when the service reported none.
	LiveStart  int64            `json:"live_start,omitempty"`
	TotalBytes int64            `json:"total_bytes"`
	Parts      []recording.Part `json:"parts,omitempty"`
	PartIndex  int              `json:"part_index"`
	// Current is the part recording when the checkpoint was written, nil
	// between parts.
	Current  *currentPart `json:"current,omitempty"`
	Group    *groupState  `json:"group,omitempty"`
	GroupSeq int          `json:"group_seq"`
	// HLSTail names the segments the session recorded last (see
	// hlsingest.Tail), so the part a resume starts does not repeat them.
	HLSTail []string `json:"hls_tail,omitempty"`
	// InterruptedAt is when the session stopped, zero while it records.
	InterruptedAt time.Time `json:"interrupted_at,omitzero"`
	Reason        string    `json:"reason,omitempty"`
}

// currentPart is a part in progress.
type currentPart struct {
	Stem  string    `json:"stem"`
	Path  string    `json:"path"`
	Index int       `json:"index"`
	Start time.Time `json:"start"`
	// AfterConfigChange marks a part started because the stream's codec
	// configuration changed, so it cannot join the group before it.
	AfterConfigChange bool `json:"after_config_change,omitempty"`
}

// groupState is the open group of a checkpoint (see partGroup).
type groupState struct {
	Index int       `json:"index"`
	Key   string    `json:"key"`
	Stems []string  `json:"stems"`
	Bytes int64     `json:"bytes"`
	Start time.Time `json:"start"`
}

func (cp *checkpoint) key() streamer.StreamerKey {
	return streamer.StreamerKey{Platform: cp.Platform, Username: cp.Username}
}

// lastByte is when the session last recorded anything, as best known.
func (cp *checkpoint) lastByte() time.Time {
	switch {
	case !cp.LastByteAt.IsZero():
		return cp.LastByteAt
	case !cp.InterruptedAt.IsZero():
		return cp.InterruptedAt
	}
	return cp.Started
}

// resumeBy is the deadline for the session to resume under window.
func (cp *checkpoint) resumeBy(window time.Duration) time.Time {
	return cp.lastByte().Add(window)
}

// resumeStore keeps checkpoints on disk, one file per session, and the
// interrupted ones in memory. A running worker owns its own checkpoint file;
// an interrupted one is owned by the store until take hands it out.
type resumeStore struct {
	dir string

	mu   sync.Mutex
	held map[streamer.StreamerKey]*checkpoint
}

// openResumeStore opens the checkpoint folder and returns every checkpoint in
// it. Nothing is held yet: the caller decides what each one is.
func openResumeStore(dir string) (*resumeStore, []*checkpoint, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, nil, fmt.Errorf("create checkpoint folder: %w", err)
	}
	s := &resumeStore{dir: dir, held: make(map[streamer.StreamerKey]*checkpoint)}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil, fmt.Errorf("read checkpoint folder: %w", err)
	}
	var cps []*checkpoint
	var errs []error
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") || strings.HasPrefix(name, ".") {
			continue
		}
		path := filepath.Join(dir, name)
		data, err := os.ReadFile(path)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		var cp checkpoint
		if err := json.Unmarshal(data, &cp); err != nil || cp.SessionID == "" || !cp.key().Valid() {
			// Unusable: there is nothing to resume or finish from it.
			errs = append(errs, fmt.Errorf("discarding unreadable checkpoint %s: %v", name, err))
			os.Remove(path)
			continue
		}
		cps = append(cps, &cp)
	}
	return s, cps, errors.Join(errs...)
}

func (s *resumeStore) path(sessionID string) string {
	return filepath.Join(s.dir, sessionID+".json")
}

// save atomically writes cp to its file.
func (s *resumeStore) save(cp *checkpoint) error {
	cp.Version = checkpointVersion
	data, err := json.Marshal(cp)
	if err != nil {
		return fmt.Errorf("marshal checkpoint: %w", err)
	}
	tmp, err := os.CreateTemp(s.dir, ".checkpoint-*")
	if err != nil {
		return fmt.Errorf("create checkpoint: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("write checkpoint: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("close checkpoint: %w", err)
	}
	if err := os.Rename(tmpName, s.path(cp.SessionID)); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("rename checkpoint into place: %w", err)
	}
	return nil
}

// remove deletes a session's checkpoint file.
func (s *resumeStore) remove(sessionID string) {
	// One left behind is finished again at the next start, which processing's
	// idempotent jobs make harmless.
	_ = os.Remove(s.path(sessionID))
}

// hold saves an interrupted session's checkpoint and keeps it in memory until
// it is taken.
func (s *resumeStore) hold(cp *checkpoint) error {
	if err := s.save(cp); err != nil {
		return err
	}
	s.mu.Lock()
	s.held[cp.key()] = cp
	s.mu.Unlock()
	return nil
}

// take hands out the interrupted session of key, if any. The caller owns it
// from then on: it resumes it or finishes it.
func (s *resumeStore) take(key streamer.StreamerKey) *checkpoint {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := s.held[key]
	delete(s.held, key)
	return cp
}

// peek returns the interrupted session of key without taking it.
func (s *resumeStore) peek(key streamer.StreamerKey) *checkpoint {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.held[key]
}

// takeExpired hands out every interrupted session past its resume deadline.
func (s *resumeStore) takeExpired(now time.Time, window time.Duration) []*checkpoint {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*checkpoint
	for key, cp := range s.held {
		if window <= 0 || !now.Before(cp.resumeBy(window)) {
			out = append(out, cp)
			delete(s.held, key)
		}
	}
	return out
}

// heldDirs returns the folders of the interrupted sessions.
func (s *resumeStore) heldDirs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	dirs := make([]string, 0, len(s.held))
	for _, cp := range s.held {
		dirs = append(dirs, cp.BaseOutDir)
	}
	return dirs
}

// canResume decides whether an interrupted session carries on as str's new
// session, recorded by writerName, and says why not when it does not.
func canResume(cp *checkpoint, str streamer.StreamerState, writerName string, window time.Duration, now time.Time) (bool, string) {
	switch {
	case window <= 0:
		return false, "resuming is off"
	case cp.Version != checkpointVersion:
		return false, "the checkpoint is from another version"
	case now.After(cp.resumeBy(window)):
		return false, "the gap is longer than the resume window"
	case newBroadcast(cp, str):
		return false, "the VodLabs service saw the broadcast end and a new one start"
	case writerName != cp.Writer:
		return false, "the stream is now served in another format"
	}
	if fi, err := os.Stat(cp.BaseOutDir); err != nil || !fi.IsDir() {
		return false, "the session folder is gone"
	}
	return true, ""
}

// newBroadcast reports whether the VodLabs service has seen the broadcast cp
// recorded end and another begin. The service stamps a start time each time
// it sees a streamer go live, and keeps checking while this recorder is down,
// so a start time that has changed is a new broadcast. One that has not is
// the same broadcast, up to an offline spell shorter than the service's check
// interval. With no start time on either side nothing is known, and the gap
// alone decides.
func newBroadcast(cp *checkpoint, str streamer.StreamerState) bool {
	return cp.LiveStart != 0 && str.LastLiveStart != 0 && str.LastLiveStart != cp.LiveStart
}

// broadcastOver reports whether a status update shows that an interrupted
// session's broadcast has ended, so it can be finished without waiting out the
// resume window.
func broadcastOver(cp *checkpoint, str streamer.StreamerState) bool {
	switch str.Status {
	case streamer.StatusLive, streamer.StatusPaused:
		return newBroadcast(cp, str)
	case streamer.StatusUnknown:
		return false
	}
	return true
}

// hlsPartHashes names the segments of an HLS part on disk, in recording order:
// the order its playlist lists them, or by file name where the playlist is
// unreadable.
func hlsPartHashes(dir string) []string {
	var names []string
	if data, err := os.ReadFile(filepath.Join(dir, "index.m3u8")); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line != "" && !strings.HasPrefix(line, "#") && !strings.Contains(line, "/") {
				names = append(names, line)
			}
		}
	}
	if len(names) == 0 {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return nil
		}
		for _, e := range entries {
			if !e.IsDir() && !strings.HasSuffix(e.Name(), ".m3u8") && !strings.HasPrefix(e.Name(), "init") {
				names = append(names, e.Name())
			}
		}
		sort.Strings(names)
	}
	hashes := make([]string, 0, len(names))
	for _, name := range names {
		body, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		hashes = append(hashes, hlsingest.SegmentHash(body))
	}
	return hashes
}

// measurePart returns the bytes a part holds on disk — a file, or a folder of
// files — and when it was last written.
func measurePart(path string) (int64, time.Time) {
	fi, err := os.Stat(path)
	if err != nil {
		return 0, time.Time{}
	}
	if !fi.IsDir() {
		return fi.Size(), fi.ModTime()
	}
	var size int64
	var mod time.Time
	_ = filepath.WalkDir(path, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil || !info.Mode().IsRegular() {
			return nil
		}
		size += info.Size()
		if info.ModTime().After(mod) {
			mod = info.ModTime()
		}
		return nil
	})
	return size, mod
}

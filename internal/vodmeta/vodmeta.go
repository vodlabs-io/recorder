// Package vodmeta records what a platform said about a broadcast while it was
// being recorded, as a metadata.json sidecar in the session folder.
//
// A broadcast is not static. Streamers retitle and re-categorise mid-stream,
// several times over a long one, so a single snapshot taken at either end of a
// session would misdescribe most of it. The sidecar keeps every distinct
// reading the recorder saw, each stamped with when it saw it.
//
// What a platform reports varies wildly, and most of it has no home in the UI
// yet. It is archived anyway: the broadcast is gone by the time anyone decides
// a field was worth having, and the recording is the only place it could have
// been kept.
package vodmeta

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/vodlabs-io/recorder/internal/streamer"
)

// FileName is the sidecar written into a session folder. It travels with the
// folder when the VOD is moved to an Archive folder, so the readings stay with
// the recording they describe.
const FileName = "metadata.json"

// fileVersion is bumped when the document schema changes incompatibly.
const fileVersion = 1

// Observation is one reading of the platform's description of the broadcast.
type Observation struct {
	// At is when the recorder first saw this reading, not when the streamer
	// made the change. Readings arrive on the cloud's polling cadence, so a
	// retitle is stamped up to one poll interval late.
	At time.Time `json:"at"`
	// LiveMetadata is inlined rather than nested: the reading IS the
	// observation, and a wrapper object would only add a level to every entry.
	streamer.LiveMetadata
}

// Document is the JSON stored as metadata.json. It is a wire contract — the
// VOD Archive scanner and, through it, GUI clients read it — so fields are only
// ever added, never repurposed.
type Document struct {
	Version   int    `json:"version"`
	Platform  string `json:"platform"`
	Username  string `json:"username"`
	SessionID string `json:"session_id"`
	// Observations are in the order they were seen, oldest first.
	Observations []Observation `json:"observations"`
}

// TimedString is one value the platform reported, with when it started being
// reported.
type TimedString struct {
	Text string    `json:"text"`
	At   time.Time `json:"at,omitzero"`
}

// Titles returns the stream titles the session saw, oldest first, collapsing
// runs of the same title into one entry. A platform that reports no title
// contributes nothing.
func (d Document) Titles() []TimedString {
	return d.field(func(m streamer.LiveMetadata) string { return m.Title })
}

// Categories returns the categories the session saw, in the same shape as
// Titles.
func (d Document) Categories() []TimedString {
	return d.field(func(m streamer.LiveMetadata) string { return m.Category })
}

// field collapses one string field across the observations into the sequence of
// distinct values it took. A reading that omits the field is skipped rather
// than recorded as a change to empty: a partial reply that dropped a title
// should not read as the streamer having cleared it.
func (d Document) field(pick func(streamer.LiveMetadata) string) []TimedString {
	var out []TimedString
	for _, o := range d.Observations {
		v := pick(o.LiveMetadata)
		if v == "" {
			continue
		}
		if len(out) > 0 && out[len(out)-1].Text == v {
			continue
		}
		out = append(out, TimedString{Text: v, At: o.At})
	}
	return out
}

// Read loads dir/metadata.json.
func Read(dir string) (Document, error) {
	var d Document
	data, err := os.ReadFile(filepath.Join(dir, FileName))
	if err != nil {
		return d, err
	}
	if err := json.Unmarshal(data, &d); err != nil {
		return d, fmt.Errorf("parse %s: %w", FileName, err)
	}
	return d, nil
}

// Write atomically writes d as dir/metadata.json.
func Write(dir string, d Document) error {
	d.Version = fileVersion
	data, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal metadata: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".vod-metadata-*")
	if err != nil {
		return fmt.Errorf("create metadata temp file: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("write metadata: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("close metadata: %w", err)
	}
	if err := os.Rename(tmpName, filepath.Join(dir, FileName)); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("rename metadata into place: %w", err)
	}
	return nil
}

// A Log accumulates one session's readings and rewrites the sidecar whenever it
// sees a new one.
//
// It writes as it goes rather than once at the end because the readings are the
// only record of a broadcast that has already happened: a session killed
// mid-stream still leaves everything it had learned by then.
type Log struct {
	dir string

	mu   sync.Mutex
	doc  Document
	last streamer.LiveMetadata
}

// NewLog starts a metadata log for a session recording into dir. Nothing is
// written until the first reading arrives, so a session on a platform that
// reports no metadata leaves no sidecar.
func NewLog(dir, platform, username, sessionID string) *Log {
	return &Log{
		dir: dir,
		doc: Document{
			Version:   fileVersion,
			Platform:  platform,
			Username:  username,
			SessionID: sessionID,
		},
	}
}

// Observe records a reading and returns whether it was new. Repeats of the
// reading already held are dropped, so callers are free to offer every status
// update they receive — most of a session's updates say nothing new.
//
// The error is from the sidecar write, and is worth logging but not worth
// failing a recording over.
func (l *Log) Observe(m streamer.LiveMetadata, at time.Time) (recorded bool, err error) {
	if l == nil || m.IsZero() {
		return false, nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.doc.Observations) > 0 && !l.last.Differs(m) {
		return false, nil
	}
	l.last = m
	l.doc.Observations = append(l.doc.Observations, Observation{At: at, LiveMetadata: m})
	return true, Write(l.dir, l.doc)
}

// Snapshot returns the document as it stands.
func (l *Log) Snapshot() Document {
	if l == nil {
		return Document{}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.doc
}

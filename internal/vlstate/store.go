package vlstate

import (
	"bytes"
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/vodlabs-io/recorder/internal/recording"
	"github.com/vodlabs-io/recorder/internal/streamer"
)

// DocOptions are the effective per-streamer rule options carried into a doc.
// Mirrors vlconfig's streamerOptions without importing vlconfig.
type DocOptions struct {
	Monitor         bool
	Record          bool
	Variant         string
	OutputTemplate  string
	DeletePartFiles *bool
	SplitSize       string
	SplitDuration   string
	ArchiveDir      string
}

// DocSeed is one streamer's membership entry for Reconcile: its identity from
// the current subscription plus its effective options. Monitor=false marks a
// one-shot (whether from a rule or the KV one-shot list).
type DocSeed struct {
	ID   streamer.StreamerRef
	Opts DocOptions
}

// docState is the store's internal record for one doc. visible gates the KV
// write: a one-shot (Monitor=false) entry is tracked from the moment it is
// subscribed but only earns a doc — and therefore a dashboard row — once it is
// recordable or recording. Visibility is never revoked; the row lives until the
// streamer is unsubscribed and the entry deleted.
type docState struct {
	doc           StreamerDoc
	visible       bool
	sessionID     string // active Session ID ("" when none)
	sessionState  string // recording|lagging|paused ("" when none)
	procSessionID string // session in post-processing ("" when none)
	moveSessionID string // session whose files are being moved ("" when none)
	moveRetrying  bool   // last move attempt failed and is backing off
	// blockedNoSpace: live and recordable, but the download folder has no room.
	blockedNoSpace bool
	lastWritten    []byte // last JSON written to KV
}

// AddedTracker persists when each streamer was first tracked. The KV bucket
// does not outlive the daemon, so the time is kept somewhere that does.
type AddedTracker interface {
	// Track returns when each key was first tracked (Unix seconds), recording
	// now for any key without a time. A key it cannot answer for is absent.
	Track(ctx context.Context, keys []streamer.StreamerKey, now time.Time) map[streamer.StreamerKey]int64
}

// Store owns the daemon's authoritative per-streamer view state and is the
// sole writer of view-doc KV keys. Every state source (cloud status updates,
// config reloads, session workers, post-processing) funnels partial updates into
// it; the store merges them and writes the consolidated doc whenever the
// serialized form actually changes.
type Store struct {
	mu    sync.Mutex
	kv    jetstream.KeyValue
	docs  map[string]*docState // keyed by doc KV key
	added AddedTracker         // nil leaves AddedAt unset
}

func NewStore(kv jetstream.KeyValue) *Store {
	return &Store{kv: kv, docs: make(map[string]*docState)}
}

// SetAddedTracker installs the source of the docs' AddedAt. Call it before the
// first Reconcile.
func (s *Store) SetAddedTracker(t AddedTracker) {
	s.added = t
}

// KV returns the underlying bucket, for callers that also read/write non-doc
// keys (session stats, one-shot markers).
func (s *Store) KV() jetstream.KeyValue {
	return s.kv
}

// seedKVKey returns the doc key for an ID, or "" for one that names no
// streamer.
//
// An ID carrying only a URL gets no doc: an add resolves its URL before writing
// anything, so a URL entry is one the cloud declined to decode, and its doc
// would render as a row with no name.
func seedKVKey(id streamer.StreamerRef) string {
	if id.StreamerKey.Valid() {
		return DocKVKey(id.StreamerKey)
	}
	return ""
}

// DocsForPlatform returns a copy of the view doc for every tracked streamer on
// one platform, with ActiveSessionID set for any Session in flight. It is the
// store's one reader, served from memory rather than the KV bucket. The
// returned docs are copies, except that DeletePartFiles is a pointer shared
// with the live state: never write through it.
func (s *Store) DocsForPlatform(platform string) []StreamerDoc {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []StreamerDoc
	for _, st := range s.docs {
		if st.doc.Platform != platform {
			continue
		}
		doc := st.doc
		// The doc's own ActiveSessionID is only written once a session reports
		// in; st.sessionID is set the moment one starts, which is what a caller
		// asking "is this streamer busy?" means.
		if doc.ActiveSessionID == "" {
			doc.ActiveSessionID = st.sessionID
		}
		out = append(out, doc)
	}
	return out
}

// Reconcile aligns the doc set with the current subscription: it upserts a doc
// for every seed and deletes docs (and internal entries) for streamers no
// longer subscribed. Called after each successful (re)subscription, when the
// streamers file has just been reloaded — so it is also how option changes
// (enable/disable, variant, monitor, ...) reach the docs.
func (s *Store) Reconcile(ctx context.Context, seeds []DocSeed) {
	// Before taking the lock: the tracker does disk I/O, and every status
	// update would queue behind it.
	added := s.trackAdded(ctx, seeds)

	s.mu.Lock()
	defer s.mu.Unlock()

	keep := make(map[string]bool, len(seeds))
	for _, seed := range seeds {
		kvKey := seedKVKey(seed.ID)
		if kvKey == "" {
			continue
		}
		keep[kvKey] = true
		st, ok := s.docs[kvKey]
		if !ok {
			st = &docState{doc: StreamerDoc{Schema: DocSchema, Status: streamer.StatusUnknown}}
			s.docs[kvKey] = st
		}
		st.doc.Platform = seed.ID.StreamerKey.Platform
		st.doc.Username = seed.ID.StreamerKey.Username
		if at, ok := added[seed.ID.StreamerKey]; ok {
			st.doc.AddedAt = at
		}
		st.doc.Monitor = seed.Opts.Monitor
		st.doc.Record = seed.Opts.Record
		st.doc.Variant = seed.Opts.Variant
		st.doc.OutputTemplate = seed.Opts.OutputTemplate
		st.doc.DeletePartFiles = seed.Opts.DeletePartFiles
		st.doc.SplitSize = seed.Opts.SplitSize
		st.doc.SplitDuration = seed.Opts.SplitDuration
		st.doc.ArchiveDir = seed.Opts.ArchiveDir
		if seed.Opts.Monitor {
			st.visible = true
		}
		s.flushLocked(ctx, kvKey, st)
	}

	for kvKey := range s.docs {
		if keep[kvKey] {
			continue
		}
		delete(s.docs, kvKey)
		s.kv.Delete(ctx, kvKey) // no-op error if the doc was never written
	}
}

// trackAdded asks the tracker for the added time of every seed that names a
// streamer. Every seed, not just new ones: an explicit re-add restarts the time
// while the old doc may still be in the map.
func (s *Store) trackAdded(ctx context.Context, seeds []DocSeed) map[streamer.StreamerKey]int64 {
	if s.added == nil {
		return nil
	}
	keys := make([]streamer.StreamerKey, 0, len(seeds))
	for _, seed := range seeds {
		if seedKVKey(seed.ID) != "" {
			keys = append(keys, seed.ID.StreamerKey)
		}
	}
	if len(keys) == 0 {
		return nil
	}
	return s.added.Track(ctx, keys, time.Now())
}

// SetStatus merges a status update into the streamer's doc. Only non-zero
// fields overwrite, so a status-only notification doesn't wipe previously known
// metadata. monitor is consulted only when the streamer isn't tracked yet
// (normally Reconcile has already seeded it).
func (s *Store) SetStatus(ctx context.Context, str streamer.StreamerState, monitor bool) {
	if !str.StreamerKey.Valid() {
		return
	}
	kvKey := DocKVKey(str.StreamerKey)

	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.docs[kvKey]
	if !ok {
		st = &docState{doc: StreamerDoc{
			Schema:   DocSchema,
			Platform: str.StreamerKey.Platform,
			Username: str.StreamerKey.Username,
			Monitor:  monitor,
		}}
		s.docs[kvKey] = st
	}
	// A monitored streamer always has a row; a one-shot earns its row when it
	// becomes recordable (and keeps it from then on, through teardown).
	if st.doc.Monitor || str.Recordable() {
		st.visible = true
	}
	if str.Status != "" {
		st.doc.Status = str.Status
	}
	if str.LastLiveStart != 0 {
		st.doc.LastLiveStart = str.LastLiveStart
	}
	if str.LastLiveDuration != 0 {
		st.doc.LastLiveDuration = str.LastLiveDuration
	}
	if !str.LiveMetadata.IsZero() {
		st.doc.LiveMetadata = docMetadata(str.LiveMetadata)
	}
	if str.AvatarURL != "" {
		st.doc.AvatarURL = str.AvatarURL
	}
	if str.DisplayName != "" {
		st.doc.DisplayName = str.DisplayName
	}
	s.flushLocked(ctx, kvKey, st)
}

// SetAvatarURL merges just the avatar into an existing streamer's doc. Live and
// paused notifications bypass SetStatus (they trigger a local re-fetch instead),
// so this carries the avatar into the doc immediately without waiting on that
// re-fetch. It never resurrects an untracked streamer.
func (s *Store) SetAvatarURL(ctx context.Context, key streamer.StreamerKey, url string) {
	if !key.Valid() || url == "" {
		return
	}
	kvKey := DocKVKey(key)

	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.docs[kvKey]
	if !ok {
		return
	}
	if st.doc.AvatarURL == url {
		return
	}
	st.doc.AvatarURL = url
	s.flushLocked(ctx, kvKey, st)
}

// SetDisplayName merges just the display name into an existing streamer's doc,
// for the same reason SetAvatarURL exists: live and paused notifications bypass
// SetStatus, so without this the new label waits on the next re-fetch. It never
// resurrects an untracked streamer, and an empty name never clears a good one.
func (s *Store) SetDisplayName(ctx context.Context, key streamer.StreamerKey, name string) {
	if !key.Valid() || name == "" {
		return
	}
	kvKey := DocKVKey(key)

	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.docs[kvKey]
	if !ok {
		return
	}
	if st.doc.DisplayName == name {
		return
	}
	st.doc.DisplayName = name
	s.flushLocked(ctx, kvKey, st)
}

// SetSessionState writes the session-state KV key (which the CLI reads) and
// folds the transition into the doc's activity.
func (s *Store) SetSessionState(ctx context.Context, key streamer.StreamerKey, sessionID string, state recording.SessionState) {
	putSessionState(ctx, s.kv, key, sessionID, state)
	if !key.Valid() || sessionID == "" {
		return
	}
	kvKey := DocKVKey(key)

	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.docs[kvKey]
	if !ok {
		return
	}
	switch state {
	case recording.SessionRecording, recording.SessionLagging, recording.SessionPaused:
		st.sessionID = sessionID
		st.sessionState = string(state)
		st.visible = true // an active recording always occupies a row
		// Recording proves the streamer is live, so the row always has a last-live
		// time. When none is known, this session's start is when we first saw it.
		if st.doc.LastLiveStart == 0 {
			st.doc.LastLiveStart = time.Now().Unix()
		}
	default: // terminal states
		if st.sessionID == sessionID {
			st.sessionID = ""
			st.sessionState = ""
		}
	}
	s.flushLocked(ctx, kvKey, st)
}

// SetProcessingState writes the session proc-state KV key (which the CLI
// reads) and folds it into the doc's activity. Activity becomes
// "processing" only while no Session is active (part conversions
// overlap recording).
func (s *Store) SetProcessingState(ctx context.Context, key streamer.StreamerKey, sessionID string, state recording.ProcessState) {
	putProcessingState(ctx, s.kv, key, sessionID, state)
	s.applyProcessing(ctx, key, sessionID, state == recording.ProcessProcessing)
}

// ClearProcessingState removes the session proc-state KV key and clears the
// doc's processing activity.
func (s *Store) ClearProcessingState(ctx context.Context, key streamer.StreamerKey, sessionID string) {
	deleteProcessingState(ctx, s.kv, key, sessionID)
	s.applyProcessing(ctx, key, sessionID, false)
}

func (s *Store) applyProcessing(ctx context.Context, key streamer.StreamerKey, sessionID string, active bool) {
	if !key.Valid() || sessionID == "" {
		return
	}
	kvKey := DocKVKey(key)

	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.docs[kvKey]
	if !ok {
		return
	}
	if active {
		st.procSessionID = sessionID
	} else if st.procSessionID == sessionID {
		st.procSessionID = ""
	}
	s.flushLocked(ctx, kvKey, st)
}

// SetMoveState folds a session's file-move progress into the doc's activity.
// active marks the session as having files in flight to its destination;
// retrying flips the badge to the retrying variant after a failed attempt. Like
// applyProcessing it is doc-only and never resurrects a purged row: an update for
// an untracked (e.g. removed one-shot) streamer is dropped, so a background move
// can't re-create a dashboard row for a streamer that is gone.
func (s *Store) SetMoveState(ctx context.Context, key streamer.StreamerKey, sessionID string, active, retrying bool) {
	if !key.Valid() || sessionID == "" {
		return
	}
	kvKey := DocKVKey(key)

	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.docs[kvKey]
	if !ok {
		return
	}
	if active {
		st.moveSessionID = sessionID
		st.moveRetrying = retrying
	} else if st.moveSessionID == sessionID {
		st.moveSessionID = ""
		st.moveRetrying = false
	}
	s.flushLocked(ctx, kvKey, st)
}

// SetBlockedNoSpace marks a Streamer as live and recordable but not being
// recorded because the download folder is out of space. Doc-only, like the other
// activity setters, so it never resurrects a purged row.
func (s *Store) SetBlockedNoSpace(ctx context.Context, key streamer.StreamerKey, blocked bool) {
	if !key.Valid() {
		return
	}
	kvKey := DocKVKey(key)

	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.docs[kvKey]
	if !ok {
		return
	}
	st.blockedNoSpace = blocked
	s.flushLocked(ctx, kvKey, st)
}

// Delete drops the streamer's doc, in memory and in KV. The caller's broader
// purge (DeleteStreamer) also covers the doc key; this keeps the store's map
// in sync so a later update can't resurrect the row.
func (s *Store) Delete(ctx context.Context, key streamer.StreamerKey) {
	if !key.Valid() {
		return
	}
	kvKey := DocKVKey(key)

	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.docs, kvKey)
	s.kv.Delete(ctx, kvKey)
}

// flushLocked derives the doc's activity, marshals it, and writes it to KV if
// the serialized form changed since the last write. Must be called with s.mu
// held.
func (s *Store) flushLocked(ctx context.Context, kvKey string, st *docState) {
	if !st.visible {
		return
	}
	switch {
	case st.blockedNoSpace:
		st.doc.Activity = activityBlockedSpace
		st.doc.ActiveSessionID = ""
	case st.sessionState != "":
		st.doc.Activity = st.sessionState
		st.doc.ActiveSessionID = st.sessionID
	case st.procSessionID != "":
		st.doc.Activity = activityProcessing
		st.doc.ActiveSessionID = st.procSessionID
	case st.moveSessionID != "":
		if st.moveRetrying {
			st.doc.Activity = activityMovingRetry
		} else {
			st.doc.Activity = activityMoving
		}
		st.doc.ActiveSessionID = st.moveSessionID
	case st.doc.Monitor:
		st.doc.Activity = activityMonitoring
		st.doc.ActiveSessionID = ""
	default:
		st.doc.Activity = activityIdle
		st.doc.ActiveSessionID = ""
	}
	data, err := json.Marshal(st.doc)
	if err != nil || bytes.Equal(data, st.lastWritten) {
		return
	}
	if _, err := s.kv.Put(ctx, kvKey, data); err == nil {
		st.lastWritten = data
	}
}

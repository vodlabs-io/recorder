package vlstate

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/vodlabs-io/recorder/internal/streamer"
)

// DocSchema is the current StreamerDoc schema version.
const DocSchema = 1

// Activity values carried in StreamerDoc.Activity. An active session state
// (recording/lagging/paused) takes precedence over processing, which takes
// precedence over moving the finished VOD to its destination, which takes
// precedence over the resting state (monitoring when Monitor, else idle).
//
// blocked_no_space outranks all of them: a Streamer that would be recording
// but for a full download folder is losing a Broadcast right now.
const (
	activityMonitoring   = "monitoring"
	activityIdle         = "idle"
	activityRecording    = "recording"
	activityLagging      = "lagging"
	activityPaused       = "paused"
	activityProcessing   = "processing"
	activityMoving       = "moving"
	activityMovingRetry  = "moving_retrying"
	activityBlockedSpace = "blocked_no_space"
)

// StreamerDoc is the consolidated per-streamer view document: identity, live
// state, effective rule options, and derived activity, serialized as JSON under
// a single KV key (see DocKVKey). It is the complete contract between the
// daemon and GUI clients — a doc's existence is what gives a streamer a
// dashboard row. High-frequency recording stats stay in the separate
// "session.<id>.stats" keys; ActiveSessionID points clients at the right one.
type StreamerDoc struct {
	Schema   int    `json:"schema"`
	Platform string `json:"platform,omitempty"`
	Username string `json:"username,omitempty"`
	// DisplayName is the label to render in place of Username, for platforms
	// whose username is an opaque id. Presentation only — Username remains the
	// identity, the KV key and the output path. Empty means show Username.
	DisplayName string `json:"display_name,omitempty"`
	// AddedAt is when the streamer was first tracked (Unix timestamp). Zero when
	// unknown.
	AddedAt          int64               `json:"added_at,omitempty"`
	Status           streamer.LiveStatus `json:"status"`
	LastLiveStart    int64               `json:"last_live_start,omitempty"`    // Unix timestamp
	LastLiveDuration int64               `json:"last_live_duration,omitempty"` // Duration in seconds
	LiveMetadata     docLiveMetadata     `json:"live_metadata,omitzero"`
	AvatarURL        string              `json:"avatar_url,omitempty"` // Re-hosted avatar image URL
	Monitor          bool                `json:"monitor"`
	Record           bool                `json:"record"`
	Variant          string              `json:"variant,omitempty"`
	OutputTemplate   string              `json:"output_template,omitempty"`
	DeletePartFiles  *bool               `json:"delete_part_files,omitempty"`
	SplitSize        string              `json:"split_size,omitempty"`
	SplitDuration    string              `json:"split_duration,omitempty"`
	ArchiveDir       string              `json:"archive_dir,omitempty"`
	Activity         string              `json:"activity"`
	ActiveSessionID  string              `json:"active_session_id,omitempty"`
}

// docLiveMetadata mirrors streamer.LiveMetadata, pinning the doc's field names
// to this package so the view-doc contract with GUI clients stays independent of
// the cloud API's wire format.
type docLiveMetadata struct {
	Platform       string `json:"platform,omitempty"`
	Title          string `json:"title,omitempty"`
	DisplayName    string `json:"display_name,omitempty"`
	PlatformUserID string `json:"platform_user_id,omitempty"`
	LiveType       string `json:"live_type,omitempty"`
}

func docMetadata(m streamer.LiveMetadata) docLiveMetadata {
	return docLiveMetadata{
		Platform:       m.Platform,
		Title:          m.Title,
		DisplayName:    m.DisplayName,
		PlatformUserID: m.PlatformUserID,
		LiveType:       m.LiveType,
	}
}

// DocKVKey returns the view-doc KV key for a resolved streamer:
// "streamer.<platform>.<encoded_username>.state".
func DocKVKey(key streamer.StreamerKey) string {
	return key.KVKey() + ".state"
}

// IsDocKVKey reports whether kvKey names a streamer view doc
// ("streamer.<x>.<y>.state", as opposed to e.g. a 6-token session key).
func IsDocKVKey(kvKey string) bool {
	parts := strings.Split(kvKey, ".")
	return len(parts) == 4 &&
		parts[0] == "streamer" &&
		parts[1] != "" &&
		parts[2] != "" &&
		parts[3] == "state"
}

// GetStreamerDoc reads and decodes the view doc for a resolved streamer.
func GetStreamerDoc(ctx context.Context, kv jetstream.KeyValue, key streamer.StreamerKey) (StreamerDoc, error) {
	if !key.Valid() {
		return StreamerDoc{}, fmt.Errorf("streamer key is invalid: %s", key)
	}
	entry, err := kv.Get(ctx, DocKVKey(key))
	if err != nil {
		return StreamerDoc{}, err
	}
	var doc StreamerDoc
	if err := json.Unmarshal(entry.Value(), &doc); err != nil {
		return StreamerDoc{}, fmt.Errorf("failed to unmarshal streamer doc: %w", err)
	}
	return doc, nil
}

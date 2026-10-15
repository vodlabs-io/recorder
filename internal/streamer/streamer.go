package streamer

import (
	"log/slog"

	"github.com/vodlabs-io/recorder/internal/cookiemint"
	"github.com/vodlabs-io/recorder/internal/recording"
	"github.com/vodlabs-io/recorder/internal/stream"
)

// StreamerState represents streamer ID and live state
type StreamerState struct {
	StreamerRef
	// DisplayName is the streamer's human-facing name, supplied by the cloud API
	// for platforms whose username is an opaque id. It is mutable and not
	// unique, so it is a label only: clients show it in place of the username,
	// and everything that identifies, keys or names a file uses the username.
	// Empty means there is nothing better to show than the username.
	DisplayName string      `json:"display_name,omitempty"`
	Status      LiveStatus  `json:"status"`                // Current state of the streamer
	PrevStatus  LiveStatus  `json:"prev_status,omitempty"` // Previous status, for detecting changes
	Streams     stream.List `json:"streams,omitempty"`     // Available stream variants (e.g. different qualities)
	// StreamsTTL is how many seconds the URLs in Streams stay usable, zero when
	// they outlive a recording. A platform that signs a short-lived grant sets
	// it, and the session re-resolves on that cadence instead of waiting for the
	// stream to start failing.
	StreamsTTL int64             `json:"streams_ttl,omitempty"`
	Headers    map[string]string `json:"headers,omitempty"` // Optional HTTP headers to use when accessing stream URLs
	// CookieMint is set by a platform whose stream URLs answer only to cookies
	// that expire within seconds; the session mints a fresh set itself as each
	// one ages. Nil on every platform whose Headers suffice.
	CookieMint *cookiemint.Mint `json:"cookie_mint,omitempty"`
	// MouflonKeys decrypts the segment names of a platform whose playlists
	// publish them encrypted, by the key id the playlist names. Nil on every
	// other platform.
	MouflonKeys map[string]string `json:"mouflon_keys,omitempty"`
	// SegmentRename rewrites the segment URLs of a platform whose playlists
	// name a lower quality than the one recorded. Nil on every other platform.
	SegmentRename    *stream.SegmentRename `json:"segment_rename,omitempty"`
	LiveMetadata     LiveMetadata          `json:"live_metadata"`                // Metadata about the live stream (title, author, etc.)
	AvatarURL        string                `json:"avatar_url,omitempty"`         // Re-hosted avatar image URL (from the cloud backend)
	LastLiveStart    int64                 `json:"last_live_start,omitempty"`    // Unix timestamp
	LastLiveDuration int64                 `json:"last_live_duration,omitempty"` // Duration in seconds
	LastStatusChange int64                 `json:"last_status_change,omitempty"` // Unix timestamp
	SessionStats     recording.Stats       `json:"session_stats"`                // Bytes recorded, duration, speed
	Monitor          *bool                 `json:"monitor,omitempty"`            // Whether the streamer's status will be monitored while not live
}

func (s StreamerState) Recordable() bool {
	switch s.Status {
	case StatusLive:
	case StatusPaused:
		if s.PrevStatus == StatusRestricted {
			return false
		}
	default:
		return false
	}
	if len(s.Streams) == 0 {
		return false
	}
	return true
}

func (s StreamerState) LogValue() slog.Value {
	attrs := []slog.Attr{
		slog.String("id", s.StreamerRef.String()),
		slog.String("status", string(s.Status)),
	}

	if s.PrevStatus != "" {
		attrs = append(attrs, slog.String("prev_status", string(s.PrevStatus)))
	}
	attrs = append(attrs, slog.Int("streams", len(s.Streams)))
	if len(s.Headers) > 0 {
		// Header values may carry auth tokens/cookies, so only log the count.
		attrs = append(attrs, slog.Int("headers", len(s.Headers)))
	}
	attrs = append(attrs, slog.Group("live_metadata", s.LiveMetadata.Attrs()...))
	if s.DisplayName != "" {
		attrs = append(attrs, slog.String("display_name", s.DisplayName))
	}
	if s.AvatarURL != "" {
		attrs = append(attrs, slog.String("avatar_url", s.AvatarURL))
	}
	if s.LastLiveStart != 0 {
		attrs = append(attrs, slog.Int64("last_live_start", s.LastLiveStart))
	}
	if s.LastLiveDuration != 0 {
		attrs = append(attrs, slog.Int64("last_live_duration", s.LastLiveDuration))
	}
	if s.LastStatusChange != 0 {
		attrs = append(attrs, slog.Int64("last_status_change", s.LastStatusChange))
	}
	attrs = append(attrs, slog.Group("session_stats",
		slog.String("session_id", s.SessionStats.SessionID),
		slog.Int64("total_bytes", s.SessionStats.TotalBytes),
		slog.Duration("duration", s.SessionStats.Duration),
		slog.Float64("speed_bps", s.SessionStats.Speed),
	))
	if s.Monitor != nil {
		attrs = append(attrs, slog.Bool("monitor", *s.Monitor))
	}

	return slog.GroupValue(attrs...)
}

type List []StreamerState

// addNew adds new streamers to the list, ignoring duplicates based on Platform and Username
func (s *List) addNew(newIDs StreamerRefs) {
	type key struct {
		Platform string
		Username string
	}

	// Track seen (Platform, Username) pairs
	seen := make(map[key]bool)
	for _, f := range *s {
		seen[key{f.Platform, f.Username}] = true
	}

	// Append unique elements from newIDs
	for _, id := range newIDs {
		k := key{id.Platform, id.Username}
		if !seen[k] {
			seen[k] = true
			*s = append(*s, StreamerState{StreamerRef: id, Status: StatusUnknown})
		}
	}
}

// IDs returns a slice of IDs for all streamers in the list
func (s List) IDs() StreamerRefs {
	ids := make(StreamerRefs, len(s))
	for i, streamer := range s {
		ids[i] = streamer.StreamerRef
	}
	return ids
}

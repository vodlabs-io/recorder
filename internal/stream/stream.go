package stream

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/vodlabs-io/recorder/internal/cameraprive"
	"github.com/vodlabs-io/recorder/internal/fansly"
	"github.com/vodlabs-io/recorder/internal/joystick"
)

// Stream represents a single stream variant with quality information
type Stream struct {
	URL       string // The raw stream URL
	QualityID string // Quality identifier (e.g., "1080p", "720p", "hd", "audio_only")
	ResWidth  int    // Video width in pixels
	ResHeight int    // Video height in pixels
	// Format names the container when the URL may not show it: FormatHLS or
	// FormatFLV. Empty means the URL decides (see IsHLS).
	Format string `json:",omitempty"`
}

// Stream formats the cloud may name in Stream.Format.
const (
	FormatHLS = "hls"
	FormatFLV = "flv"
)

// IsHLS reports whether the stream is an HLS playlist rather than an FLV
// stream: as Format says, or from the URL when Format is empty.
func (s Stream) IsHLS() bool {
	switch s.Format {
	case FormatHLS:
		return true
	case FormatFLV:
		return false
	}
	// A Niconico program (nicolive://), an FC2 Live channel (fc2live://), a
	// Peeks broadcast (peeks://), a Fansly channel (fansly://), a CameraPrive
	// room (cameraprive://) or a Joystick channel (joystick://) is recorded from
	// the HLS its seat is granted.
	return strings.Contains(s.URL, ".m3u8") || strings.HasPrefix(s.URL, "nicolive://") ||
		strings.HasPrefix(s.URL, "fc2live://") || strings.HasPrefix(s.URL, "peeks://") ||
		fansly.IsURL(s.URL) || cameraprive.IsURL(s.URL) || joystick.IsURL(s.URL)
}

// SeatLoginHost names the platform host a stream's seat reads it from as the
// recorder's user, where a login for the platform may sign the seat in: a
// Fansly channel's API, a CameraPrive room's page. Empty for every other
// stream.
func (s Stream) SeatLoginHost() string {
	switch {
	case fansly.IsURL(s.URL):
		return fansly.APIHost
	case cameraprive.IsURL(s.URL):
		return cameraprive.SiteHost
	case joystick.IsURL(s.URL):
		return joystick.SiteHost
	}
	return ""
}

// IsVP8 reports whether the stream is known, before it is opened, to carry VP8
// video: an Agora channel (agora://) joined with codec=vp8.
func (s Stream) IsVP8() bool {
	if !strings.HasPrefix(s.URL, "agora://") {
		return false
	}
	u, err := url.Parse(s.URL)
	return err == nil && u.Query().Get("codec") == "vp8"
}

// SegmentRename rewrites a media playlist's segment URLs before they are
// fetched: the first From in each is replaced by To. The cloud sends one for
// a platform whose playlists name a lower quality than the CDN serves under a
// sibling name.
type SegmentRename struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// Apply returns uri with the rename made, or uri unchanged when r is nil.
func (r *SegmentRename) Apply(uri string) string {
	if r == nil || r.From == "" {
		return uri
	}
	return strings.Replace(uri, r.From, r.To, 1)
}

// Resolution returns the resolution of the stream as a string in the format "WIDTHxHEIGHT"
func (s Stream) Resolution() string {
	return fmt.Sprintf("%dx%d", s.ResWidth, s.ResHeight)
}

// hasResolution reports whether the platform told us this variant's pixel size.
//
// Plenty of platforms don't: Bigo serves a plain media playlist with no variant
// list at all, and Douyin names its qualities (ld/sd/hd/or4) without ever
// publishing a resolution. Those variants arrive as 0x0, which means "unknown",
// NOT "audio only" — see audioLabelled.
func (s Stream) hasResolution() bool {
	return s.ResWidth > 0 && s.ResHeight > 0
}

// audioLabelled reports whether the variant's quality id names it as an
// audio-only rendition. That label is the only reliable audio-only signal: a
// missing resolution is a hint at best, since it also describes every variant
// on a platform that publishes no resolutions.
func (s Stream) audioLabelled() bool {
	q := strings.ToLower(s.QualityID)
	return q == "ao" || strings.Contains(q, "audio")
}

type List []Stream

// anyResolution reports whether any variant in the list carries a resolution.
func (streams List) anyResolution() bool {
	for i := range streams {
		if streams[i].hasResolution() {
			return true
		}
	}
	return false
}

// IsAudioOnlyVariant reports whether a variant preference asks for a stream with
// no video. A session recording one produces files with no video track by
// design, which is otherwise the signature of a failed remux.
func IsAudioOnlyVariant(variant string) bool {
	switch strings.ToLower(strings.TrimSpace(variant)) {
	case "audio", "audio_only", "ao":
		return true
	}
	return false
}

// Select selects a stream from the list based on the variant preference.
// Variant can be: "best", "worst", resolution like "1920x1080" or "1080p".
// The list is ordered worst to best by the platform plugin.
func (streams List) Select(variant string) *Stream {
	if len(streams) == 0 {
		return nil
	}

	pref := strings.ToLower(strings.TrimSpace(variant))

	if IsAudioOnlyVariant(pref) {
		return streams.selectAudio()
	}

	switch pref {
	case "best", "b", "":
		return &streams[len(streams)-1]
	case "worst", "w":
		return streams.selectWorst()
	default:
	}

	// Try matching WIDTHxHEIGHT format
	if strings.Contains(pref, "x") {
		parts := strings.Split(pref, "x")
		if len(parts) == 2 {
			targetW, err1 := strconv.Atoi(parts[0])
			targetH, err2 := strconv.Atoi(parts[1])
			if err1 == nil && err2 == nil {
				// Find the stream with resolution >= target
				// Prefer lowest bandwidth that meets requirement
				for i := range streams {
					if streams[i].ResWidth >= targetW && streams[i].ResHeight >= targetH {
						return &streams[i]
					}
				}
			}
		}
	}

	// Named quality. Matched in passes so an exact quality id anywhere in the
	// list beats a loose numeric match on an earlier entry, and so platforms
	// that publish no resolutions (their ids are all we have to go on) can still
	// be addressed by name — "sd1", "or4", "live".
	for i := range streams {
		if strings.ToLower(streams[i].QualityID) == pref {
			return &streams[i]
		}
	}
	for i := range streams {
		// Try matching "1080p" against "1080p60"
		if strings.HasPrefix(strings.ToLower(streams[i].QualityID), pref) {
			return &streams[i]
		}
	}
	for i := range streams {
		// Try matching pref against resolution width or height. Only meaningful
		// for a variant that has one: "0" would otherwise match any pref
		// containing a zero ("1080p").
		if !streams[i].hasResolution() {
			continue
		}
		widthStr := strconv.Itoa(streams[i].ResWidth)
		heightStr := strconv.Itoa(streams[i].ResHeight)
		if strings.Contains(pref, widthStr) || strings.Contains(pref, heightStr) {
			return &streams[i]
		}
	}

	// Default to best if no match
	return &streams[len(streams)-1]
}

// selectWorst returns the first variant that isn't audio-only, preferring one
// with a known resolution so an unlabelled audio track can't win. Where no
// resolutions are published (Bigo, Douyin), list order is the only ordering.
func (streams List) selectWorst() *Stream {
	for i := range streams {
		if !streams[i].audioLabelled() && streams[i].hasResolution() {
			return &streams[i]
		}
	}
	for i := range streams {
		if !streams[i].audioLabelled() {
			return &streams[i]
		}
	}
	return nil // Every variant is audio-only
}

// selectAudio returns the audio-only variant, or nil when the list has none.
func (streams List) selectAudio() *Stream {
	for i := range streams {
		if streams[i].audioLabelled() {
			return &streams[i]
		}
	}
	// Unlabelled fallback: a variant with no resolution is audio-only only when
	// some other variant in the list does report one. When the platform
	// publishes no resolutions at all, 0x0 says nothing about the content.
	if !streams.anyResolution() {
		return nil
	}
	for i := range streams {
		if !streams[i].hasResolution() {
			return &streams[i]
		}
	}
	return nil
}

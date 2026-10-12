package hlsingest

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	m3u8 "github.com/Eyevinn/hls-m3u8/m3u8"
	"github.com/vodlabs-io/recorder/internal/stream"
)

// maxMasterHops bounds how many master playlists resolveMediaPlaylist will
// follow. One hop is what the real cases need; the limit exists so a
// misconfigured Origin that points a master at itself cannot spin forever.
const maxMasterHops = 3

// resolveMediaPlaylist follows master playlists until a media playlist is
// reached, returning the URL to poll. A separate audio rendition the chosen
// variant names is not followed; see resolveRenditions.
func (o *ingest) resolveMediaPlaylist(playlistURL string) (string, error) {
	media, _, err := o.resolveRenditions(playlistURL)
	return media, err
}

// resolveRenditions follows master playlists until a media playlist is
// reached, returning the URL to poll, and the URL of the audio rendition the
// chosen variant names when the master publishes its audio separately (empty
// otherwise).
//
// Usually the URL is already a media playlist selected by the Plugin, but it
// cannot be assumed: some Origins answer a per-quality pull URL with a
// one-variant master that redirects to a session-tokenised media playlist on
// another host (Douyin rooms fronted by Wangsu do exactly this). Reading those
// needs the indirection followed, not rejected.
//
// When a master offers several variants, the session's own preference
// decides, so "worst" stays "worst" wherever the choice happens to be made.
func (o *ingest) resolveRenditions(playlistURL string) (string, string, error) {
	audioURL := ""
	for hop := 0; ; hop++ {
		body, err := o.fetch(playlistURL)
		if err != nil {
			return "", "", fmt.Errorf("fetch playlist: %w", err)
		}
		playlist, _, err := m3u8.Decode(*bytes.NewBuffer(o.sanitize(body)), false)
		if err != nil {
			return "", "", fmt.Errorf("parse playlist: %w", err)
		}

		master, ok := playlist.(*m3u8.MasterPlaylist)
		if !ok {
			return playlistURL, audioURL, nil // already a media playlist: the common case
		}
		if hop >= maxMasterHops {
			return "", "", fmt.Errorf("master playlist nested more than %d deep", maxMasterHops)
		}

		next, v, err := selectRendition(master, playlistURL, o.cfg.Variant)
		if err != nil {
			return "", "", err
		}
		audioURL = ""
		if uri := audioRendition(v); uri != "" {
			if audioURL, err = absoluteURL(playlistURL, uri); err != nil {
				return "", "", fmt.Errorf("resolve audio rendition URL %q: %w", uri, err)
			}
		}
		o.logger.Info("Followed master playlist to a media playlist",
			"variants", len(master.Variants), "variant_pref", o.cfg.Variant, "url", next,
			"separate_audio", audioURL != "")
		playlistURL = next
	}
}

// selectVariant picks a variant from a master playlist and returns its absolute
// URL, honouring the session's quality preference.
func selectVariant(master *m3u8.MasterPlaylist, baseURL, variant string) (string, error) {
	u, _, err := selectRendition(master, baseURL, variant)
	return u, err
}

// selectRendition is selectVariant, also returning the variant chosen.
func selectRendition(master *m3u8.MasterPlaylist, baseURL, variant string) (string, *m3u8.Variant, error) {
	streams := make(stream.List, 0, len(master.Variants))
	byURI := make(map[string]*m3u8.Variant, len(master.Variants))
	for _, v := range master.Variants {
		// An I-frame playlist is keyframes only, for scrubbing: never a stream
		// to record.
		if v == nil || v.URI == "" || v.Iframe {
			continue
		}
		width, height := 0, 0
		if v.Resolution != "" {
			fmt.Sscanf(v.Resolution, "%dx%d", &width, &height)
		}
		streams = append(streams, stream.Stream{
			URL:       v.URI,
			QualityID: variantQualityID(v),
			ResWidth:  width,
			ResHeight: height,
		})
		byURI[v.URI] = v
	}
	if len(streams) == 0 {
		return "", nil, errors.New("master playlist has no variants")
	}

	// Select reads the list as worst-to-best, which a master playlist does not
	// promise; sort so "worst" and "best" mean what they say.
	sort.SliceStable(streams, func(i, j int) bool {
		if streams[i].ResHeight != streams[j].ResHeight {
			return streams[i].ResHeight < streams[j].ResHeight
		}
		return streams[i].ResWidth < streams[j].ResWidth
	})

	selected := streams.Select(variant)
	if selected == nil {
		return "", nil, fmt.Errorf("no variant matches %q among %d in the master playlist",
			variant, len(streams))
	}
	u, err := absoluteURL(baseURL, selected.URL)
	return u, byURI[selected.URL], err
}

// audioRendition returns the URI of the audio rendition a variant plays with
// when it is a playlist of its own, preferring the group's default, and ""
// when the variant's audio (if any) travels in its own segments.
func audioRendition(v *m3u8.Variant) string {
	if v == nil || v.Audio == "" {
		return ""
	}
	uri := ""
	for _, a := range v.Alternatives {
		if a == nil || a.Type != "AUDIO" || a.GroupId != v.Audio || a.URI == "" {
			continue
		}
		if a.Default {
			return a.URI
		}
		if uri == "" {
			uri = a.URI
		}
	}
	return uri
}

// variantQualityID names a master-playlist variant the way the Plugins do, so a
// preference like "720p" can match one.
func variantQualityID(v *m3u8.Variant) string {
	if v.Resolution != "" {
		if _, h, ok := strings.Cut(v.Resolution, "x"); ok && h != "" {
			if v.FrameRate >= 50 {
				return fmt.Sprintf("%sp%d", h, int(v.FrameRate))
			}
			return h + "p"
		}
	}
	if v.Bandwidth > 0 {
		return fmt.Sprintf("%dk", v.Bandwidth/1000)
	}
	return "live"
}

// pollPlaylist fetches and parses the Origin's media playlist once.
func (o *ingest) pollPlaylist(playlistURL string) (*m3u8.MediaPlaylist, error) {
	body, err := o.fetch(playlistURL)
	if err != nil {
		return nil, fmt.Errorf("fetch media playlist: %w", err)
	}
	// BIGO-SPECIFIC (see bigo_webprotect.go) — delete this line with that file.
	// Read from the body, not the parsed playlist: the tag is BIGO's own and the
	// parser keeps only the ones HLS defines. Reset every poll, so a playlist
	// that stops carrying the tag stops unmasking.
	o.bigo.observe(body)
	body, err = restoreMouflon(body, o.cfg.MouflonKeys)
	if err != nil {
		return nil, fmt.Errorf("restore media playlist: %w", err)
	}
	playlist, _, err := m3u8.Decode(*bytes.NewBuffer(o.sanitize(body)), false)
	if err != nil {
		return nil, fmt.Errorf("parse media playlist: %w", err)
	}
	media, ok := playlist.(*m3u8.MediaPlaylist)
	if !ok {
		return nil, errNotMediaPlaylist
	}
	return media, nil
}

func (o *ingest) sanitize(b []byte) []byte {
	if !o.cfg.SanitizeTimestamps {
		return b
	}
	return sanitizePlaylistTimestamps(b)
}

// userAgent is the User-Agent requests carry: a header's, else the
// configured one.
func (o *ingest) userAgent() string {
	for k, v := range o.cfg.Headers {
		if strings.EqualFold(strings.TrimSpace(k), "User-Agent") && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return o.cfg.UserAgent
}

func (o *ingest) buildRequest(u string) (*http.Request, error) {
	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return nil, err
	}
	if o.cfg.UserAgent != "" {
		req.Header.Set("User-Agent", o.cfg.UserAgent)
	}
	for k, v := range o.cfg.Headers {
		if k != "" {
			req.Header.Set(strings.TrimSpace(k), strings.TrimSpace(v))
		}
	}
	return req, nil
}

func (o *ingest) fetch(u string) ([]byte, error) {
	req, err := o.buildRequest(u)
	if err != nil {
		return nil, err
	}
	resp, err := o.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", u, resp.Status)
	}
	return io.ReadAll(resp.Body)
}

// fetchRetry fetches u, retrying on failure with a widening pause.
//
// budget bounds the wall time the attempts may take together; zero leaves them
// unbounded. It is what keeps a failure short enough to be retried: the
// poll-level retry (segmentPollRetries) only reaches a segment the Origin still
// publishes, so attempts here have to finish before it is dropped.
func (o *ingest) fetchRetry(ctx context.Context, u string, budget time.Duration) ([]byte, error) {
	var last error
	// Always make one attempt. A caller that asks for none would otherwise get
	// no request at all — and, since that returns no error either, a silent
	// stream of empty segments.
	attempts := o.cfg.FetchAttempts
	if attempts <= 0 {
		attempts = max(o.cfg.Retries, 1)
	}
	var deadline time.Time
	if budget > 0 {
		deadline = time.Now().Add(budget)
	}
	for i := 0; i < attempts; i++ {
		b, err := o.fetch(u)
		if err == nil {
			return b, nil
		}
		last = err
		// The pause before an attempt that will not happen is dead time the
		// window cannot spare.
		if i == attempts-1 {
			break
		}
		pause := time.Duration(i+1) * 300 * time.Millisecond
		if !deadline.IsZero() && !time.Now().Add(pause).Before(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(pause):
		}
	}
	return nil, last
}

func absoluteURL(base, ref string) (string, error) {
	bu, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	ru, err := url.Parse(ref)
	if err != nil {
		return "", err
	}
	return bu.ResolveReference(ru).String(), nil
}

// segRetry counts consecutive polls spent fetching what one segment needs —
// the segment itself, or the init map it cannot be decoded without.
//
// It counts against the media sequence rather than the URL because a
// refreshable Origin re-signs every URL it publishes: keyed on the URL, the
// budget would start over at every refresh and never bound anything.
type segRetry struct {
	seq   uint64
	held  bool
	polls int
}

// spend records another poll spent on the segment at seq and reports the
// running count, starting over whenever the segment being waited on changes.
func (r *segRetry) spend(seq uint64) int {
	if !r.held || r.seq != seq {
		r.seq, r.held, r.polls = seq, true, 0
	}
	r.polls++
	return r.polls
}

func (r *segRetry) clear() { r.seq, r.held, r.polls = 0, false, 0 }

// parseIV decodes an EXT-X-KEY IV attribute into 16 bytes, returning nil when
// it cannot be read.
func parseIV(s string) []byte {
	t := strings.TrimSpace(s)
	t = strings.TrimPrefix(t, "0x")
	t = strings.TrimPrefix(t, "0X")
	if len(t)%2 == 1 {
		t = "0" + t
	}
	b, err := hex.DecodeString(t)
	if err != nil {
		return nil
	}
	if len(b) < 16 {
		p := make([]byte, 16)
		copy(p[16-len(b):], b)
		return p
	}
	if len(b) > 16 {
		return b[len(b)-16:]
	}
	return b
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

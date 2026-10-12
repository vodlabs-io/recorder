// Package hlsingest reads a Broadcast from an Origin's HLS playlist and hands
// the bytes to a Sink.
//
// It owns everything that is true of any Origin: following a master playlist to
// a media one, polling on a ticker with a retry budget, skipping the history a
// DVR playlist opens with, fetching init maps and segments in playlist order,
// and stopping at a failed fetch so the segments behind it are not recorded
// first. What is done with the bytes — a file on disk, a playlist someone is
// watching, decryption, wall-clock derivation — belongs to the Sink.
package hlsingest

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"time"

	m3u8 "github.com/Eyevinn/hls-m3u8/m3u8"
)

// defaultPollInterval is the cadence a media playlist is re-fetched at when
// Config leaves it unset.
const defaultPollInterval = 2 * time.Second

// refreshLead is the fraction of Config.URLTTL at which the playlist URL is
// re-resolved. Refreshing early costs one extra resolve; refreshing late costs
// a hole in the recording, so the margin is deliberately generous.
const refreshLead = 0.6

// maxAdaptivePoll bounds the cadence adaptivePoll may slow a poll down to. It
// is also how far behind the live edge a slowed-down ingest can be, which is
// what keeps this from being a free win on a Session someone is watching.
const maxAdaptivePoll = 5 * time.Second

// segmentPollRetries bounds how many polls a failed download is retried over.
// Segments are handed over in playlist order, so a failure cannot be skipped
// and collected later; the poll stops there and resumes from it. The bound
// keeps a segment that is genuinely gone from stalling everything behind it.
//
// The retry only reaches a segment the Origin still publishes, so it is worth
// something only while a failure costs less than the Origin takes to drop one —
// see segmentFetchShare.
const segmentPollRetries = 3

// segmentFetchShare is the fraction of one segment's duration that a segment's
// fetch attempts may take before the ingest goes back to polling. An Origin
// drops about one segment per segment-duration, and the poll retry only
// reaches a segment still published, so slower attempts would never be retried
// (e.g. Douyin publishes 4s segments three at a time).
const segmentFetchShare = 0.5

// minSegmentFetchBudget floors the budget, so an Origin cutting short segments
// cannot squeeze the attempts into a time no honest fetch could finish in.
const minSegmentFetchBudget = time.Second

// deepWindowFetch is the published window above which the budget is dropped
// altogether. A DVR Origin holds a segment for hours after the live edge passes
// it, so a slow failure costs nothing there and the attempts are worth more.
const deepWindowFetch = time.Minute

// Config describes one Origin and how to read it.
type Config struct {
	// URL is the playlist to read. A master playlist is followed to a media one.
	URL string
	// Variant is the session's quality preference ("best", "worst", "720p", …),
	// used only when URL turns out to be a master playlist. Empty means best.
	Variant string
	// UserAgent and Headers are applied to every request.
	UserAgent string
	Headers   map[string]string
	// Retries is the consecutive-failure budget for playlist polls.
	Retries int
	// FetchAttempts is the total attempts made for one init map or segment.
	// Zero means max(Retries, 1).
	FetchAttempts int
	// PollInterval is the media playlist poll cadence. Zero means
	// defaultPollInterval.
	PollInterval time.Duration
	// LiveEdgeWindow bounds how much already-published content is adopted from
	// the first playlist fetched. Zero means defaultLiveEdgeWindow.
	LiveEdgeWindow time.Duration
	// SanitizeTimestamps rewrites EXT-X-PROGRAM-DATE-TIME and EXT-X-DATERANGE
	// values that carry no timezone designator, before the playlist is parsed.
	// Some Origins emit them, and the parser rejects the playlist outright.
	SanitizeTimestamps bool
	// URLTTL is how long URL — and every segment URL the playlist behind it
	// hands out — stays usable. Zero means indefinitely, which is every Origin
	// but one: YouTube signs a grant that answers 403 for all of its segments
	// about half a minute after it was minted, whatever their own expiry says.
	//
	// Re-fetching the playlist does not renew it; the same signed segment URLs
	// come back. Only RefreshURL does, so the two are set together.
	URLTTL time.Duration
	// RefreshURL re-resolves the Origin's playlist URL and returns the new one,
	// for the Origins that need it. Nil means the URL is good for the whole
	// Broadcast, and no refresh is attempted.
	RefreshURL func(context.Context) (string, error)
	// MouflonKeys decrypts the segment names of an Origin that publishes them
	// encrypted, by the key id its playlist names (see mouflon.go). Nil for
	// every other Origin.
	MouflonKeys map[string]string
}

// Map is an fMP4 initialisation segment, named by EXT-X-MAP. Every Segment
// handed over after it depends on it.
type Map struct {
	// URI is the map's absolute URL.
	URI string
}

// Key is the EXT-X-KEY in effect for a Segment.
//
// The bytes are fetched here so a Sink needs no client of its own, but nothing
// is decrypted: a Sink that cares reads Body, and one that does not is free to
// refuse the Segment instead.
type Key struct {
	// Method is the METHOD attribute verbatim ("AES-128", "SAMPLE-AES", …).
	Method string
	// URI is the key's absolute URL, empty when the playlist gave none.
	URI string
	// IV is the parsed IV attribute, nil when absent. IVAttr is that attribute
	// verbatim, for a Sink that re-emits the tag rather than decrypting.
	IV     []byte
	IVAttr string
	// Body is the fetched key. Nil when the method needs none, when the
	// playlist gave no URI, or when the fetch failed — a failure that is
	// logged and not fatal, since a Sink may still record the Segment
	// undecrypted.
	Body []byte
}

// Segment is one media segment, with the playlist facts a Sink might need.
type Segment struct {
	// URI is the segment's absolute URL.
	URI string
	// Duration is the EXTINF value in seconds.
	Duration float64
	// Sequence counts the Segments handed to the Sink, from zero.
	Sequence uint64
	// ProgramDateTime is the segment's EXT-X-PROGRAM-DATE-TIME as parsed, zero
	// when it carries none. Note that the parser applies a tag to the one
	// segment following it and no further, so on an Origin that stamps only the
	// head of its playlist this is zero for every later segment.
	ProgramDateTime time.Time
	// Discontinuity reports EXT-X-DISCONTINUITY ahead of this segment.
	Discontinuity bool
	// IsFMP4 reports that this playlist carries an EXT-X-MAP, so its segments
	// are fragmented MP4 rather than transport stream.
	IsFMP4 bool
	// Key is the encryption in effect, nil when the segment is unencrypted.
	Key *Key
}

// Sink receives what one ingest reads, in playlist order.
//
// Every method runs on the ingest goroutine and must not block for long. An
// error from any of them stops the ingest and is returned from Run wrapped in
// SinkError; for Segment, it also counts against the segment's poll budget the
// same way a failed fetch does, so a Sink that cannot take a Segment now gets
// the same stop-and-resume the Origin gets.
type Sink interface {
	// Init receives a fetched initialisation segment, before the first Segment
	// that depends on it, and again whenever the Origin changes it.
	Init(m Map, body []byte) error
	// Segment receives one fetched media segment.
	Segment(s Segment, body []byte) error
	// Finish is called exactly once, however the ingest ends. err is what the
	// ingest is about to return, nil on a clean end of stream.
	Finish(err error) error
}

// SinkError wraps an error a Sink returned, so a caller can tell its own
// failure from the Origin's.
type SinkError struct{ err error }

func (e *SinkError) Error() string { return e.err.Error() }
func (e *SinkError) Unwrap() error { return e.err }

// Run reads cfg.URL until the Origin ends the playlist, ctx is cancelled, or a
// failure exhausts its budget. It always calls sink.Finish.
func Run(
	ctx context.Context,
	cfg Config,
	client *http.Client,
	logger *slog.Logger,
	sink Sink,
) (err error) {
	o := &ingest{cfg: cfg, client: withCookieJar(client), logger: logger, sink: sink}

	defer func() {
		if ferr := sink.Finish(err); ferr != nil {
			logger.Error("Sink failed to finish", "error", ferr)
			if err == nil {
				err = &SinkError{err: ferr}
			}
		}
	}()

	playlistURL, audioURL, err := o.resolveRenditions(cfg.URL)
	if err != nil {
		return err
	}
	if audioURL != "" {
		return o.runDemuxed(ctx, playlistURL, audioURL)
	}
	return o.run(ctx, playlistURL)
}

// withCookieJar returns client with a cookie jar of this ingest's own, unless
// it already carries one.
//
// Some Origins hand out a session cookie with the playlist and refuse segments
// without it (TwitCasting answers 401). The jar is per ingest because the
// client is shared by every Session: a jar on it would carry one Session's
// cookies into another's requests.
func withCookieJar(client *http.Client) *http.Client {
	if client == nil {
		client = http.DefaultClient
	}
	if client.Jar != nil {
		return client
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		return client
	}
	c := *client
	c.Jar = jar
	return &c
}

type ingest struct {
	cfg    Config
	client *http.Client
	logger *slog.Logger
	sink   Sink

	// BIGO-SPECIFIC (see bigo_webprotect.go) — delete this field with that file.
	bigo bigoState
}

func (o *ingest) pollInterval() time.Duration {
	if o.cfg.PollInterval > 0 {
		return o.cfg.PollInterval
	}
	return defaultPollInterval
}

// refreshes reports whether this Origin's playlist URL has to be re-resolved
// while the Broadcast runs.
func (o *ingest) refreshes() bool {
	return o.cfg.RefreshURL != nil && o.cfg.URLTTL > 0
}

// refreshInterval is how long a freshly resolved playlist URL is used before
// the next one is asked for.
func (o *ingest) refreshInterval() time.Duration {
	return time.Duration(float64(o.cfg.URLTTL) * refreshLead)
}

// adaptivePoll returns the cadence to re-fetch a playlist that publishes the
// given window of content.
//
// The configured interval is a floor sized for the Origins that publish a few
// seconds of history, where a slow poll loses segments. A DVR Origin publishes
// hours of it and can lose nothing, and re-fetching the whole thing costs real
// bandwidth: YouTube's window is 7200 one-second segments, ~9.6 MB of playlist
// (~130 KB gzipped) per poll, which at the floor outweighs the video itself.
func (o *ingest) adaptivePoll(window time.Duration) time.Duration {
	base := o.pollInterval()
	// A tenth of the window leaves nine tenths of slack before the oldest
	// segment not yet taken could age out.
	slowed := window / 10
	if slowed > maxAdaptivePoll {
		slowed = maxAdaptivePoll
	}
	if slowed <= base {
		return base
	}
	return slowed
}

// playlistWindow is how much content a playlist publishes, and over how many
// segments. The decoder pads its slice with nils to the playlist's capacity, so
// neither can be read off len(segments).
func playlistWindow(segments []*m3u8.MediaSegment) (time.Duration, int) {
	var acc time.Duration
	count := 0
	for _, seg := range segments {
		if seg == nil || seg.URI == "" {
			continue
		}
		acc += time.Duration(seg.Duration * float64(time.Second))
		count++
	}
	return acc, count
}

// segmentFetchBudget is how long one segment's fetch attempts may take against
// an Origin publishing the given window in segments of the given length. Zero
// leaves them unbounded: nothing measured yet, or a window deep enough that
// what fails now is still there to retry later.
func segmentFetchBudget(window, segment time.Duration) time.Duration {
	if segment <= 0 || window >= deepWindowFetch {
		return 0
	}
	return max(time.Duration(float64(segment)*segmentFetchShare), minSegmentFetchBudget)
}

// refreshPlaylistURL re-resolves the Origin's playlist URL, returning the URL
// to poll from now on and when to ask again. A refresh that fails is not fatal
// — the current URL may have life left in it — so it keeps the old URL and
// comes back on the next poll.
func (o *ingest) refreshPlaylistURL(ctx context.Context, current string) (string, time.Duration) {
	next, err := o.cfg.RefreshURL(ctx)
	if err != nil || next == "" {
		o.logger.Warn("Playlist URL refresh failed, keeping the current one", "error", err)
		return current, o.pollInterval()
	}
	resolved, err := o.resolveMediaPlaylist(next)
	if err != nil {
		o.logger.Warn("Refreshed playlist URL did not resolve", "error", err)
		return current, o.pollInterval()
	}
	o.logger.Debug("Refreshed the Origin playlist URL")
	return resolved, o.refreshInterval()
}

func (o *ingest) run(ctx context.Context, playlistURL string) error {
	ticker := time.NewTicker(o.pollInterval())
	defer ticker.Stop()

	// A refreshable Origin re-signs every segment URL, so what identifies a
	// segment across a refresh is its media sequence number, not its URL.
	seen := newSegSeen(o.refreshes())
	lastMapURI := ""
	isFMP4 := false
	firstPoll := true
	var seq uint64

	// Encryption state carried across segments: an EXT-X-KEY applies until the
	// next one, so the key body is fetched when the tag changes and reused
	// until it does.
	var keyBody, lastIV []byte
	var lastKeyMethod, lastKeyURI string

	// Consecutive failed polls. A poll far more often fails for reasons that
	// have nothing to do with the Broadcast — a brief 5xx, a connection dropped
	// mid-body — than because it is over, so it gets a retry budget.
	pollFailures := 0
	// Polls spent on the segment the last one stopped at. See segmentPollRetries.
	var retry segRetry

	// When the playlist URL is next re-resolved; zero when it never is.
	var refreshAt time.Time
	if o.refreshes() {
		refreshAt = time.Now().Add(o.refreshInterval())
	}

	for {
		if !refreshAt.IsZero() && !time.Now().Before(refreshAt) {
			var wait time.Duration
			playlistURL, wait = o.refreshPlaylistURL(ctx, playlistURL)
			refreshAt = time.Now().Add(wait)
			if ctx.Err() != nil {
				return nil
			}
		}

		// Set when this poll stops early on a failed segment: the ones after it
		// are still to come, so the Origin's playlist is not finished yet.
		stalled := false

		media, err := o.pollPlaylist(playlistURL)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			pollFailures++
			if pollFailures > o.cfg.Retries {
				return fmt.Errorf("media playlist poll failed %d times: %w", pollFailures, err)
			}
			o.logger.Warn("Media playlist poll failed, retrying",
				"error", err, "attempt", pollFailures, "retries", o.cfg.Retries)
			if !o.wait(ctx, ticker) {
				return nil
			}
			continue
		}
		pollFailures = 0

		if media.Segments == nil {
			o.logger.Warn("Playlist has no segments, waiting")
			if !o.wait(ctx, ticker) {
				return nil
			}
			continue
		}

		// What the Origin publishes right now. A Broadcast can change it
		// mid-Session, so the segment fetch budget is re-derived every poll.
		pubWindow, pubCount := playlistWindow(media.Segments)
		var meanSegment time.Duration
		if pubCount > 0 {
			meanSegment = pubWindow / time.Duration(pubCount)
		}
		fetchBudget := segmentFetchBudget(pubWindow, meanSegment)

		// Begin at the live edge rather than adopting the Origin's whole
		// sliding window, which on a DVR playlist is hours of the past.
		if firstPoll {
			firstPoll = false
			window := o.cfg.LiveEdgeWindow
			if window <= 0 {
				window = defaultLiveEdgeWindow
			}
			if skipped := o.seedFirstPoll(seen, media.Segments, playlistURL, window); skipped > 0 {
				o.logger.Info("Skipped pre-existing segments to start at the live edge",
					"skipped", skipped, "window", window)
			}
			if cadence := o.adaptivePoll(pubWindow); cadence != o.pollInterval() {
				o.logger.Info("Slowed the playlist poll to suit its window",
					"poll_interval", cadence.Round(time.Millisecond),
					"window", pubWindow.Round(time.Second), "segments", pubCount)
				ticker.Reset(cadence)
			}
		}

		pre := o.prefetch(ctx, o.unseenURLs(media.Segments, playlistURL, seen), fetchBudget)
		for _, seg := range media.Segments {
			if seg == nil || seg.URI == "" {
				continue
			}

			absSeg, err := absoluteURL(playlistURL, seg.URI)
			if err != nil {
				o.logger.Error("Failed to resolve segment URL", "error", err, "segment_uri", seg.URI)
				continue
			}
			if seen.has(seg.SeqId, absSeg) {
				continue
			}

			// The EXT-X-KEY in effect: the segment's own overrides the
			// playlist's, and the last of either wins.
			var effKey *m3u8.Key
			if len(seg.Keys) > 0 {
				effKey = &seg.Keys[len(seg.Keys)-1]
			} else if len(media.Keys) > 0 {
				effKey = &media.Keys[len(media.Keys)-1]
			}
			var key *Key
			if effKey != nil {
				var ivBytes []byte
				if effKey.IV != "" {
					ivBytes = parseIV(effKey.IV)
				}
				absKeyURI := ""
				if effKey.URI != "" {
					if abs, err := absoluteURL(playlistURL, effKey.URI); err == nil {
						absKeyURI = abs
					} else {
						o.logger.Error("Failed to resolve key URL",
							"error", err, "key_uri", effKey.URI)
					}
				}
				// Refetch only when the tag changed; it applies until the next.
				if effKey.Method != lastKeyMethod ||
					effKey.URI != lastKeyURI ||
					!bytesEqual(ivBytes, lastIV) {
					keyBody = o.fetchKey(effKey.Method, absKeyURI)
					lastKeyMethod, lastKeyURI, lastIV = effKey.Method, effKey.URI, ivBytes
				}
				key = &Key{
					Method: effKey.Method,
					URI:    absKeyURI,
					IV:     ivBytes,
					IVAttr: effKey.IV,
					Body:   keyBody,
				}
			}

			// An init map initialises every segment that follows it, so a
			// fragment handed over without one is undecodable: the map is a
			// prerequisite for this segment, and a failure holds the segment
			// back rather than skipping ahead.
			segMap := seg.Map
			if segMap == nil {
				segMap = media.Map
			}
			if segMap != nil && segMap.URI != "" {
				isFMP4 = true
				absMap, err := absoluteURL(playlistURL, segMap.URI)
				if err != nil {
					pre.close()
					return fmt.Errorf("resolve map URL %q: %w", segMap.URI, err)
				}
				if absMap != lastMapURI {
					body, err := o.fetchRetry(ctx, absMap, fetchBudget)
					if err != nil {
						if poll := retry.spend(seg.SeqId); poll <= segmentPollRetries {
							o.logger.Warn("Init map download failed, retrying on the next poll",
								"error", err, "map_url", absMap, "poll", poll)
							stalled = true
							break
						}
						// Nothing after this can be used; the caller resolves
						// the stream afresh on its next attempt.
						pre.close()
						return fmt.Errorf("download init map %s: %w", absMap, err)
					}
					if err := o.sink.Init(Map{URI: absMap}, body); err != nil {
						pre.close()
						return &SinkError{err: err}
					}
					lastMapURI = absMap
				}
			}

			body, ok, err := pre.get(ctx, absSeg)
			if !ok {
				body, err = o.fetchRetry(ctx, absSeg, fetchBudget)
			}
			if err != nil {
				if poll := retry.spend(seg.SeqId); poll <= segmentPollRetries {
					// Leave it unseen and stop: the segments after it must not
					// be handed over first, and the Origin still holds it.
					o.logger.Warn("Segment download failed, retrying on the next poll",
						"error", err, "segment_url", absSeg, "poll", poll)
					stalled = true
					break
				}
				o.logger.Error("Dropping segment after repeated download failures",
					"error", err, "segment_url", absSeg, "polls", retry.polls)
				seen.mark(seg.SeqId, absSeg)
				retry.clear()
				continue
			}

			// BIGO-SPECIFIC (see bigo_webprotect.go) — delete this block with
			// that file.
			//
			// Ahead of the Sink, so that every Sink archives a stream that
			// demuxes rather than each undoing the mask for itself. Transport
			// stream only: an fMP4 fragment carries no PAT or PMT to mask. An
			// encrypted segment is still ciphertext here, and its plaintext is
			// the Sink's to produce, so the mask is left to travel with it.
			if o.bigo.active && !isFMP4 && key == nil {
				o.unmaskBigoSegment(body, absSeg)
			}

			s := Segment{
				URI:             absSeg,
				Duration:        seg.Duration,
				Sequence:        seq,
				ProgramDateTime: seg.ProgramDateTime,
				Discontinuity:   seg.Discontinuity,
				IsFMP4:          isFMP4,
				Key:             key,
			}
			if err := o.sink.Segment(s, body); err != nil {
				// A Sink that cannot take this segment gets the same
				// stop-and-resume a failed fetch does, so the segment is not
				// lost and the ones behind it do not overtake it.
				if poll := retry.spend(seg.SeqId); poll <= segmentPollRetries {
					o.logger.Warn("Sink rejected segment, retrying on the next poll",
						"error", err, "segment_url", absSeg, "poll", poll)
					stalled = true
					break
				}
				pre.close()
				return &SinkError{err: err}
			}
			seen.mark(seg.SeqId, absSeg)
			retry.clear()
			seq++

			if ctx.Err() != nil {
				return nil
			}
		}

		pre.close()

		// The URL set only has to cover what the Origin still publishes; a
		// Session that runs for hours would otherwise remember every segment of
		// it. Rebuilding costs a walk of the playlist, so it waits until the set
		// has grown well past one window.
		if _, published := playlistWindow(media.Segments); seen.len() > 4*published {
			seen.prune(o.currentURLs(media.Segments, playlistURL))
		}

		if media.Closed && !stalled {
			o.logger.Debug("Origin playlist ended")
			return nil
		}
		if !o.wait(ctx, ticker) {
			return nil
		}
	}
}

// seedFirstPoll starts the ingest at the live edge of the first playlist it
// fetched, and reports how many segments it skipped to get there.
func (o *ingest) seedFirstPoll(
	seen *segSeen,
	segments []*m3u8.MediaSegment,
	playlistURL string,
	window time.Duration,
) int {
	if seen.bySeq {
		cutoff := liveEdgeCutoff(segments, window)
		if cutoff < 0 || cutoff+1 >= len(segments) {
			return 0
		}
		first := segments[cutoff+1]
		if first == nil {
			return 0
		}
		seen.startAt(first.SeqId)
		return cutoff + 1
	}
	resolve := func(uri string) (string, bool) {
		abs, err := absoluteURL(playlistURL, uri)
		return abs, err == nil
	}
	return seedLiveEdge(segments, window, resolve, seen.urls)
}

// unseenURLs lists, in playlist order, the segments a poll has yet to hand
// over.
func (o *ingest) unseenURLs(segments []*m3u8.MediaSegment, playlistURL string, seen *segSeen) []string {
	var urls []string
	for _, seg := range segments {
		if seg == nil || seg.URI == "" {
			continue
		}
		abs, err := absoluteURL(playlistURL, seg.URI)
		if err != nil || seen.has(seg.SeqId, abs) {
			continue
		}
		urls = append(urls, abs)
	}
	return urls
}

// currentURLs is the set of segment URLs a playlist publishes right now.
func (o *ingest) currentURLs(segments []*m3u8.MediaSegment, playlistURL string) map[string]bool {
	current := make(map[string]bool, len(segments))
	for _, seg := range segments {
		if seg == nil || seg.URI == "" {
			continue
		}
		if abs, err := absoluteURL(playlistURL, seg.URI); err == nil {
			current[abs] = true
		}
	}
	return current
}

// wait blocks for the next poll, reporting false when ctx ended first.
func (o *ingest) wait(ctx context.Context, ticker *time.Ticker) bool {
	select {
	case <-ctx.Done():
		return false
	case <-ticker.C:
		return true
	}
}

// fetchKey returns the key bytes for an EXT-X-KEY, or nil when the method needs
// none or the fetch failed. A failure is not fatal: a Sink may still record the
// segment undecrypted, and refusing is its decision to make.
func (o *ingest) fetchKey(method, absURI string) []byte {
	switch method {
	case "", "NONE":
		return nil
	}
	if absURI == "" {
		o.logger.Warn("Encryption with empty key URI, no key fetched", "method", method)
		return nil
	}
	body, err := o.fetch(absURI)
	if err != nil {
		o.logger.Error("Failed to fetch encryption key", "error", err, "key_url", absURI)
		return nil
	}
	return body
}

var errNotMediaPlaylist = errors.New("expected media playlist")

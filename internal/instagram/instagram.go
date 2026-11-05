// Package instagram records an Instagram Live broadcast, which is published as
// DASH, through the recorder's HLS ingest.
//
// A broadcast is named by an instagram://<broadcast id>?mpd=<manifest> URL. The
// manifest URL takes no cookies, and nor does the media. The manifest lists the
// last twenty seconds or so, but every segment since the broadcast began stays
// available by number, and each number after the first is the next segment. So
// the seat reads the manifest for the renditions and for how far the broadcast
// has got, and publishes the numbered segments as HLS playlists that it serves
// itself, starting from the first segment: a broadcast noticed minutes after it
// began is recorded from its beginning.
package instagram

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/vodlabs-io/recorder/internal/useragent"
)

// Scheme names a broadcast to record.
const Scheme = "instagram"

const (
	// virtualHost is the host of the playlists the seat serves itself. It is
	// reserved, so nothing real can ever answer to it.
	virtualHost = "instagram-seat.invalid"
	masterPath  = "/master.m3u8"
	// firstNumber is the number of a broadcast's first segment.
	firstNumber = 100
	// nominalSegment is a segment's length, in milliseconds, when the manifest no
	// longer lists it.
	nominalSegment = 2000
	// CatchUpBatch is how many segments a playlist poll may publish at once, which
	// paces reading a broadcast's past at several times its running speed.
	CatchUpBatch = 10
	// playlistDepth is how many segments a playlist keeps listing behind its head.
	playlistDepth = 40
	maxManifest   = 1 << 20
)

// Timings; variables so tests can shorten them.
var (
	// refreshEvery is the least time between two reads of the manifest. The
	// renditions' playlists are polled apart and share what it says.
	refreshEvery = time.Second
	// goneAfter is how long the manifest may keep failing, with nothing new,
	// before the broadcast is taken to be over.
	goneAfter      = 45 * time.Second
	openTimeout    = 30 * time.Second
	requestTimeout = 15 * time.Second
)

// ErrEnded reports a broadcast that is over.
var ErrEnded = errors.New("instagram broadcast ended")

var broadcastID = regexp.MustCompile(`^[0-9]{1,24}$`)

// IsURL reports whether u names an Instagram broadcast.
func IsURL(u string) bool {
	return strings.HasPrefix(u, Scheme+"://")
}

// Parse returns the broadcast id and the manifest URL an instagram:// URL names.
func Parse(raw string) (id, manifest string, err error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != Scheme {
		return "", "", fmt.Errorf("not an instagram URL: %q", raw)
	}
	if !broadcastID.MatchString(u.Host) {
		return "", "", fmt.Errorf("instagram URL %q names no broadcast", raw)
	}
	manifest = u.Query().Get("mpd")
	m, err := url.Parse(manifest)
	if err != nil || (m.Scheme != "https" && m.Scheme != "http") || m.Host == "" {
		return "", "", fmt.Errorf("instagram URL %q names no manifest", raw)
	}
	return u.Host, manifest, nil
}

// mpd is the part of the manifest the seat reads.
type mpd struct {
	Type string `xml:"type,attr"`
	// FirstAvTimeMs is the wall clock at timeline zero, in Unix milliseconds.
	FirstAvTimeMs int64 `xml:"firstAvTimeMs,attr"`
	// PublishFrameTime is the timeline position of the first segment.
	PublishFrameTime int64 `xml:"publishFrameTime,attr"`
	Periods          []struct {
		Sets []struct {
			Reps []mpdRep `xml:"Representation"`
		} `xml:"AdaptationSet"`
	} `xml:"Period"`
}

type mpdRep struct {
	ID        string  `xml:"id,attr"`
	MimeType  string  `xml:"mimeType,attr"`
	Codecs    string  `xml:"codecs,attr"`
	Width     int     `xml:"width,attr"`
	Height    int     `xml:"height,attr"`
	FrameRate float64 `xml:"frameRate,attr"`
	Bandwidth int64   `xml:"bandwidth,attr"`
	BaseURL   string  `xml:"BaseURL"`
	Template  struct {
		Initialization string `xml:"initialization,attr"`
		Media          string `xml:"media,attr"`
		Timeline       struct {
			// Predicted addresses a segment by number, and EndNumber is the
			// number of the last segment listed.
			Predicted string `xml:"FBPredictedMedia,attr"`
			EndNumber int64  `xml:"FBPredictedMediaEndNumber,attr"`
			Segments  []struct {
				T int64 `xml:"t,attr"`
				D int64 `xml:"d,attr"`
			} `xml:"S"`
		} `xml:"SegmentTimeline"`
	} `xml:"SegmentTemplate"`
}

func (r *mpdRep) audio() bool { return strings.HasPrefix(r.MimeType, "audio/") }

// segment is one published segment.
type segment struct {
	number   int64
	url      string
	init     string
	duration float64
	start    time.Time
	// discontinuity reports a new encoder generation starting here.
	discontinuity bool
}

// rendition is one quality's list of segments, in order. It only grows.
type rendition struct {
	id        string
	audio     bool
	width     int
	height    int
	frameRate float64
	bandwidth int64
	codecs    string
	media     string // the numbered segment template in use
	next      int64  // the next number to list; zero until the first read
	segments  []segment
	// published is how many of segments the playlist has offered so far, and
	// fetched how many of them have been asked for.
	published int
	fetched   int
	// polled reports that something is reading this rendition's playlist.
	polled bool
}

// segmentPlace is where a segment sits in its rendition.
type segmentPlace struct {
	rendition *rendition
	index     int
}

// Seat reads one broadcast's manifest while its playlists are polled.
type Seat struct {
	// MasterURL is the master playlist, which Client serves.
	MasterURL string

	id        string
	manifest  string
	client    *http.Client
	userAgent string
	log       *slog.Logger
	cancel    context.CancelFunc
	ctx       context.Context
	done      chan struct{}
	closeOnce sync.Once

	mu         sync.Mutex
	renditions []*rendition
	byID       map[string]*rendition
	// bySegment finds a segment's place from its address.
	bySegment map[string]segmentPlace
	readAt    time.Time // when the manifest was last read
	lastGood  time.Time // when it last answered with something to record
	first     int64     // the number the broadcast's segments start from
	ended     bool
}

// Open reads the broadcast's manifest and holds a seat on it until ctx ends or
// Close is called. client makes every request but the playlists Client serves.
func Open(ctx context.Context, raw string, client *http.Client, userAgent string, logger *slog.Logger) (*Seat, error) {
	id, manifest, err := Parse(raw)
	if err != nil {
		return nil, err
	}
	if client == nil {
		client = http.DefaultClient
	}
	if userAgent == "" {
		userAgent = useragent.Fallback
	}
	if logger == nil {
		logger = slog.Default()
	}
	seatCtx, cancel := context.WithCancel(ctx)
	s := &Seat{
		MasterURL: "https://" + virtualHost + masterPath,
		id:        id,
		manifest:  manifest,
		client:    client,
		userAgent: userAgent,
		log:       logger,
		ctx:       seatCtx,
		cancel:    cancel,
		done:      make(chan struct{}),
		byID:      map[string]*rendition{},
		bySegment: map[string]segmentPlace{},
		lastGood:  time.Now(),
	}
	// Read to its end by this process already: there is nothing left to take a
	// seat on, whatever a status check that has not caught up still says.
	if n, ok := resumePoint(id); ok && n == resumeFinished {
		cancel()
		return nil, ErrEnded
	}
	openCtx, cancelOpen := context.WithTimeout(ctx, openTimeout)
	defer cancelOpen()
	m, err := s.readManifest(openCtx)
	if err != nil {
		cancel()
		return nil, err
	}
	if m.Type != "" && m.Type != "dynamic" && totalSegments(m) == 0 {
		cancel()
		return nil, ErrEnded
	}
	if n, ok := resumePoint(id); ok {
		s.first = n
	} else {
		s.first = s.firstAvailable(openCtx, m)
	}
	s.apply(m, time.Now())
	if len(s.renditions) == 0 {
		cancel()
		return nil, fmt.Errorf("instagram manifest of %s lists no renditions", id)
	}
	context.AfterFunc(seatCtx, func() { s.closeOnce.Do(func() { close(s.done) }) })
	logger.Info("Watching an Instagram broadcast", "broadcast", id,
		"renditions", len(s.renditions), "first_segment", s.first)
	return s, nil
}

// resumePoints remembers how far this process has read each broadcast, so that
// a seat taken again after a failure carries on rather than reading the whole
// broadcast over.
var resumePoints = struct {
	sync.Mutex
	numbers map[string]int64
	order   []string
}{numbers: map[string]int64{}}

const (
	// maxResumePoints bounds what is remembered. A broadcast's point is only of
	// use for the few seconds between two attempts.
	maxResumePoints = 256
	// resumeOverlap goes back over what may have been fetched and not yet handed
	// on: reading a segment twice beats leaving a hole.
	resumeOverlap = 5
	// resumeFinished is the resume point of a broadcast read to its end.
	resumeFinished = -1
)

func resumePoint(id string) (int64, bool) {
	resumePoints.Lock()
	defer resumePoints.Unlock()
	n, ok := resumePoints.numbers[id]
	return n, ok
}

func setResumePoint(id string, n int64) {
	resumePoints.Lock()
	defer resumePoints.Unlock()
	if _, known := resumePoints.numbers[id]; !known {
		resumePoints.order = append(resumePoints.order, id)
		if len(resumePoints.order) > maxResumePoints {
			delete(resumePoints.numbers, resumePoints.order[0])
			resumePoints.order = resumePoints.order[1:]
		}
	}
	resumePoints.numbers[id] = n
}

// noteResumePoint remembers where the renditions being read have all reached.
func (s *Seat) noteResumePoint() {
	s.mu.Lock()
	defer s.mu.Unlock()
	reached := int64(0)
	finished := s.ended
	for _, r := range s.renditions {
		if !r.polled {
			continue
		}
		if r.fetched == 0 {
			return
		}
		if n := r.segments[r.fetched-1].number; reached == 0 || n < reached {
			reached = n
		}
		finished = finished && r.fetched == len(r.segments)
	}
	switch {
	case reached > 0 && finished:
		setResumePoint(s.id, resumeFinished)
	case reached > 0:
		setResumePoint(s.id, max(reached-resumeOverlap, s.first))
	}
}

// Client returns base serving the seat's playlists itself.
func (s *Seat) Client(base *http.Client) *http.Client {
	c := *base
	next := base.Transport
	if next == nil {
		next = http.DefaultTransport
	}
	c.Transport = &transport{seat: s, next: next}
	return &c
}

// Done is closed when the seat is released.
func (s *Seat) Done() <-chan struct{} { return s.done }

// Err always reports nil: the broadcast's end closes the playlists rather than
// the seat, so that every segment already published is read.
func (s *Seat) Err() error { return nil }

// Close gives the seat up.
func (s *Seat) Close() {
	s.noteResumePoint()
	s.cancel()
	<-s.done
}

type transport struct {
	seat *Seat
	next http.RoundTripper
}

func (t *transport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Host != virtualHost {
		t.seat.noteFetch(req.URL.String())
		return t.next.RoundTrip(req)
	}
	body, err := t.seat.playlist(req.Context(), req.URL.Path)
	if err != nil {
		return nil, err
	}
	return &http.Response{
		Status:        "200 OK",
		StatusCode:    http.StatusOK,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        http.Header{"Content-Type": {"application/vnd.apple.mpegurl"}},
		Body:          io.NopCloser(strings.NewReader(body)),
		ContentLength: int64(len(body)),
		Request:       req,
	}, nil
}

// playlist answers one playlist poll.
func (s *Seat) playlist(ctx context.Context, path string) (string, error) {
	if path != masterPath {
		s.refresh(ctx)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if path == masterPath {
		return s.renderMaster(), nil
	}
	id := strings.TrimSuffix(strings.TrimPrefix(path, "/"), ".m3u8")
	r := s.byID[id]
	if r == nil {
		return "", fmt.Errorf("no such rendition %q", id)
	}
	return s.renderMedia(r), nil
}

// noteFetch records how far a rendition has been read.
func (s *Seat) noteFetch(u string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p, ok := s.bySegment[u]; ok && p.index >= p.rendition.fetched {
		p.rendition.fetched = p.index + 1
	}
}

func playlistURL(id string) string {
	return "https://" + virtualHost + "/" + url.PathEscape(id) + ".m3u8"
}

func (s *Seat) renderMaster() string {
	var b strings.Builder
	b.WriteString("#EXTM3U\n#EXT-X-VERSION:7\n")
	var audio *rendition
	for _, r := range s.renditions {
		if r.audio {
			audio = r
			fmt.Fprintf(&b, "#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"audio\",NAME=\"audio\",DEFAULT=YES,AUTOSELECT=YES,URI=%q\n",
				playlistURL(r.id))
			break
		}
	}
	wrote := false
	for _, r := range s.renditions {
		if r.audio {
			continue
		}
		wrote = true
		fmt.Fprintf(&b, "#EXT-X-STREAM-INF:BANDWIDTH=%d", max(r.bandwidth, 1))
		if r.width > 0 && r.height > 0 {
			fmt.Fprintf(&b, ",RESOLUTION=%dx%d", r.width, r.height)
		}
		if r.frameRate > 0 {
			fmt.Fprintf(&b, ",FRAME-RATE=%.3f", r.frameRate)
		}
		if audio != nil {
			b.WriteString(",AUDIO=\"audio\"")
		}
		fmt.Fprintf(&b, "\n%s\n", playlistURL(r.id))
	}
	// A broadcast with no picture is still a broadcast.
	if !wrote && audio != nil {
		fmt.Fprintf(&b, "#EXT-X-STREAM-INF:BANDWIDTH=%d\n%s\n", max(audio.bandwidth, 1), playlistURL(audio.id))
	}
	return b.String()
}

// renderMedia publishes the next batch of r's segments.
//
// The ingest skips whatever is more than half a minute behind the head of the
// first playlist it reads, so a batch is what keeps a broadcast's past from
// being skipped: the first playlist holds only the broadcast's first segments.
//
// The ingest joins the video and the audio by time and holds only so much of
// either for the other, so while it reads the past no rendition gets more than
// a batch ahead of another that is being read.
func (s *Seat) renderMedia(r *rendition) string {
	r.polled = true
	limit := len(r.segments)
	for _, other := range s.renditions {
		if other != r && other.polled {
			limit = min(limit, other.fetched+CatchUpBatch)
		}
	}
	r.published = max(r.published, min(r.published+CatchUpBatch, limit))
	from := max(0, r.published-playlistDepth)
	window := r.segments[from:r.published]

	var b strings.Builder
	seq := int64(0)
	if len(window) > 0 {
		seq = window[0].number
	}
	fmt.Fprintf(&b, "#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-TARGETDURATION:%d\n#EXT-X-MEDIA-SEQUENCE:%d\n",
		nominalSegment/1000+1, seq)
	mapped := ""
	for i, seg := range window {
		if seg.discontinuity && i > 0 {
			b.WriteString("#EXT-X-DISCONTINUITY\n")
		}
		if seg.init != mapped {
			fmt.Fprintf(&b, "#EXT-X-MAP:URI=%q\n", seg.init)
			mapped = seg.init
		}
		if !seg.start.IsZero() {
			fmt.Fprintf(&b, "#EXT-X-PROGRAM-DATE-TIME:%s\n", seg.start.UTC().Format("2006-01-02T15:04:05.000Z"))
		}
		fmt.Fprintf(&b, "#EXTINF:%.3f,\n%s\n", seg.duration, seg.url)
	}
	if s.ended && r.published == len(r.segments) {
		b.WriteString("#EXT-X-ENDLIST\n")
	}
	return b.String()
}

// refresh reads the manifest again unless it was read a moment ago. A failed
// read keeps what is known: only a long run of them ends the broadcast.
func (s *Seat) refresh(ctx context.Context) {
	s.mu.Lock()
	if s.ended || time.Since(s.readAt) < refreshEvery {
		s.mu.Unlock()
		return
	}
	s.readAt = time.Now()
	s.mu.Unlock()

	readCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	m, err := s.readManifest(readCtx)
	now := time.Now()

	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case errors.Is(err, ErrEnded):
		s.log.Info("The Instagram broadcast's manifest is gone", "broadcast", s.id)
		s.ended = true
	case err != nil:
		if now.Sub(s.lastGood) > goneAfter {
			s.log.Info("Giving up on the Instagram broadcast's manifest", "broadcast", s.id, "error", err)
			s.ended = true
			return
		}
		s.log.Debug("Failed to read the Instagram manifest", "broadcast", s.id, "error", err)
	default:
		s.lastGood = now
		s.applyLocked(m, now)
		if m.Type != "" && m.Type != "dynamic" {
			s.log.Info("The Instagram broadcast has ended", "broadcast", s.id)
			s.ended = true
		}
	}
}

func (s *Seat) readManifest(ctx context.Context) (*mpd, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.manifest, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", s.userAgent)
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound, resp.StatusCode == http.StatusGone:
		return nil, ErrEnded
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("instagram manifest: status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxManifest))
	if err != nil {
		return nil, err
	}
	var m mpd
	if err := xml.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("instagram manifest: %w", err)
	}
	return &m, nil
}

func totalSegments(m *mpd) int {
	n := 0
	for _, p := range m.Periods {
		for _, set := range p.Sets {
			for _, r := range set.Reps {
				n += len(r.Template.Timeline.Segments)
			}
		}
	}
	return n
}

// firstAvailable finds where the broadcast's segments start: its first number,
// when that is still there to read, and otherwise what the manifest lists.
func (s *Seat) firstAvailable(ctx context.Context, m *mpd) int64 {
	for _, p := range m.Periods {
		for _, set := range p.Sets {
			for i := range set.Reps {
				r := &set.Reps[i]
				tl := &r.Template.Timeline
				if tl.Predicted == "" || tl.EndNumber == 0 {
					continue
				}
				base, err := resolve(s.manifest, r.BaseURL)
				if err != nil {
					continue
				}
				u, err := resolve(base, numbered(tl.Predicted, firstNumber))
				if err != nil {
					continue
				}
				if s.segmentThere(ctx, u) {
					return firstNumber
				}
				return tl.EndNumber - int64(len(tl.Segments)) + 1
			}
		}
	}
	return firstNumber
}

func (s *Seat) segmentThere(ctx context.Context, u string) bool {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, u, nil)
	if err != nil {
		return false
	}
	req.Header.Set("User-Agent", s.userAgent)
	resp, err := s.client.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func (s *Seat) apply(m *mpd, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.applyLocked(m, now)
}

// applyLocked adds what a manifest says to the renditions. mu is held.
func (s *Seat) applyLocked(m *mpd, _ time.Time) {
	for _, p := range m.Periods {
		for _, set := range p.Sets {
			for i := range set.Reps {
				s.applyRep(m, &set.Reps[i])
			}
		}
	}
}

func (s *Seat) applyRep(m *mpd, rep *mpdRep) {
	tl := &rep.Template.Timeline
	if rep.ID == "" || tl.Predicted == "" || tl.EndNumber == 0 || len(tl.Segments) == 0 {
		return
	}
	r := s.byID[rep.ID]
	if r == nil {
		r = &rendition{id: rep.ID, audio: rep.audio()}
		s.byID[rep.ID] = r
		s.renditions = append(s.renditions, r)
	}
	r.width, r.height, r.frameRate = rep.Width, rep.Height, rep.FrameRate
	r.bandwidth, r.codecs = rep.Bandwidth, rep.Codecs

	// A manifest's addresses are relative to its BaseURL, and that to the manifest.
	base, err := resolve(s.manifest, rep.BaseURL)
	if err != nil {
		s.log.Warn("Unreadable Instagram base address", "error", err)
		return
	}
	init, err := resolve(base, rep.Template.Initialization)
	if err != nil {
		s.log.Warn("Unreadable Instagram init segment address", "error", err)
		return
	}
	listed := int64(len(tl.Segments))
	firstListed := tl.EndNumber - listed + 1
	discontinuity := false
	switch {
	case r.next == 0:
		r.next = s.first
		if r.next == 0 {
			r.next = firstListed
		}
	case r.media != tl.Predicted:
		// The encoder restarted: a new run of numbers, under a new name.
		discontinuity = true
		r.next = firstListed
	}
	r.media = tl.Predicted

	// What is no longer listed is placed between the broadcast's start and the
	// segments the manifest does list.
	origin := m.PublishFrameTime
	lastListedT := tl.Segments[listed-1].T
	msPerSegment := float64(nominalSegment)
	if span := tl.EndNumber - firstNumber; span > 0 && lastListedT > origin {
		msPerSegment = float64(lastListedT-origin) / float64(span)
	}
	for n := r.next; n <= tl.EndNumber; n++ {
		u, err := resolve(base, numbered(tl.Predicted, n))
		if err != nil {
			s.log.Warn("Unreadable Instagram segment address", "error", err)
			return
		}
		seg := segment{number: n, url: u, init: init, duration: nominalSegment / 1000.0, discontinuity: discontinuity}
		discontinuity = false
		position := origin + int64(float64(n-firstNumber)*msPerSegment)
		if i := n - firstListed; i >= 0 {
			position = tl.Segments[i].T
			seg.duration = float64(tl.Segments[i].D) / 1000
		}
		if m.FirstAvTimeMs > 0 {
			seg.start = time.UnixMilli(m.FirstAvTimeMs + position)
		}
		s.bySegment[u] = segmentPlace{rendition: r, index: len(r.segments)}
		r.segments = append(r.segments, seg)
	}
	if tl.EndNumber >= r.next {
		r.next = tl.EndNumber + 1
	}
}

func numbered(template string, n int64) string {
	return strings.ReplaceAll(template, "$Number$", strconv.FormatInt(n, 10))
}

// resolve makes a manifest's address absolute against its BaseURL.
func resolve(base, ref string) (string, error) {
	b, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	r, err := url.Parse(ref)
	if err != nil {
		return "", err
	}
	return b.ResolveReference(r).String(), nil
}

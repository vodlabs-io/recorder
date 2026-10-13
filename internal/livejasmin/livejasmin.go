// Package livejasmin records a LiveJasmin (livejasmin.com) room as an FLV
// stream.
//
// The site offers a guest a room's video only while the guest holds a seat
// in the room: a socket.io connection that joined it. The room then names a
// progressive fragmented MP4 (H.264 and AAC, one sample per fragment) that
// runs for as long as the seat holds. A guest's seat lasts two minutes, so a
// recording takes a fresh guest's seat shortly before each one runs out and
// splices the new seat's stream in at its first keyframe, keeping one
// continuous FLV timeline across them.
package livejasmin

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/vodlabs-io/recorder/internal/useragent"
)

// Scheme names a room the recorder joins itself.
const Scheme = "livejasmin"

// defaultHeight is the rendition a seat asks for when the URL names none; the
// room serves the nearest it has.
const defaultHeight = 1080

var (
	// rotateAfter is how long after a seat joined the next one is taken. The
	// site lets a guest's seat go two minutes after it joined.
	rotateAfter = 95 * time.Second
	// mediaTimeout ends a recording whose stream has carried nothing for
	// this long.
	mediaTimeout = 20 * time.Second
	// seatAttempts bounds the tries at a seat when the stream has run out.
	seatAttempts = 3
	seatRetry    = 2 * time.Second
	// frameGap is the step placed between the last frame of one seat's stream
	// and the first of the next.
	frameGap int64 = 33
)

// ErrEnded reports a room with no broadcast a guest may watch: the performer
// left, or is in a show guests are not admitted to.
var ErrEnded = errors.New("livejasmin: broadcast ended")

var namePattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)

// Room is a livejasmin:// URL: the performer, as the site's room URLs name
// them, and the rendition height to ask for.
type Room struct {
	Name   string
	Height int
}

// IsURL reports whether u is a livejasmin:// URL.
func IsURL(u string) bool {
	return strings.HasPrefix(u, Scheme+"://")
}

// Parse decodes a livejasmin:// URL.
func Parse(raw string) (Room, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return Room{}, fmt.Errorf("parse livejasmin url: %w", err)
	}
	if u.Scheme != Scheme {
		return Room{}, fmt.Errorf("not a livejasmin url: scheme %q", u.Scheme)
	}
	r := Room{Name: u.Host}
	if !namePattern.MatchString(r.Name) {
		return Room{}, fmt.Errorf("livejasmin url names no performer")
	}
	if h := u.Query().Get("height"); h != "" {
		if r.Height, err = strconv.Atoi(h); err != nil || r.Height <= 0 {
			return Room{}, fmt.Errorf("livejasmin url has a bad height %q", h)
		}
	}
	return r, nil
}

// URL encodes r as a livejasmin:// URL.
func (r Room) URL() string {
	u := Scheme + "://" + r.Name
	if r.Height > 0 {
		u += "?height=" + strconv.Itoa(r.Height)
	}
	return u
}

// openSeatFunc takes a seat; a variable so tests can stand in for the site.
var openSeatFunc = func(ctx context.Context, r Room, client *http.Client, userAgent string, logger *slog.Logger) (seatStream, error) {
	return openSeat(ctx, r, client, userAgent, logger)
}

// seatStream is what a recording needs of a seat.
type seatStream interface {
	Stream() io.Reader
	Done() <-chan struct{}
	Close()
}

// Stream is the seat's fragmented MP4.
func (s *seat) Stream() io.Reader { return s.body }

// Open takes a seat in the room u names and returns its broadcast as an FLV
// stream on a timeline starting at zero. Reading ends with io.EOF when the
// broadcast ends or turns to a show guests may not watch, or with the error
// that ended the recording. Close, or the end of ctx, leaves the room.
func Open(ctx context.Context, u string, client *http.Client, userAgent string, logger *slog.Logger) (io.ReadCloser, error) {
	r, err := Parse(u)
	if err != nil {
		return nil, err
	}
	if client == nil {
		client = &http.Client{}
	}
	logger = logger.With("livejasmin_room", r.Name)
	first, err := openSeatFunc(ctx, r, client, userAgent, logger)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	pr, pw := io.Pipe()
	rec := &recording{
		room: r, client: client, userAgent: userAgent, logger: logger,
		mux: newFLVMuxer(pw), lastVideo: -1, lastAudio: -1,
	}
	go func() {
		err := rec.run(ctx, first)
		cancel()
		pw.CloseWithError(err)
	}()
	return &stream{PipeReader: pr, cancel: cancel}, nil
}

type stream struct {
	*io.PipeReader
	cancel context.CancelFunc
}

func (s *stream) Close() error {
	// Closing the reader first releases a muxer blocked writing to it.
	err := s.PipeReader.Close()
	s.cancel()
	return err
}

// feed is one seat's stream, demuxed.
type feed struct {
	seat    seatStream
	joined  time.Time
	samples chan sample
	err     error // why samples closed; read after it has
	avcC    []byte
	asc     []byte
	started bool // its first keyframe has been written
}

func startFeed(s seatStream) *feed {
	f := &feed{seat: s, joined: time.Now(), samples: make(chan sample, 256)}
	go func() {
		defer close(f.samples)
		d := newDemuxer(s.Stream())
		for {
			smp, err := d.next()
			if err != nil {
				f.err = err
				return
			}
			smp.avcC, smp.asc = d.videoConfig(), d.audioConfig()
			select {
			case f.samples <- smp:
			case <-s.Done():
				// The seat was let go; what the stream still holds is
				// dropped with it.
				f.err = io.EOF
				return
			}
		}
	}()
	return f
}

// recording muxes a succession of seats' streams onto one FLV timeline.
type recording struct {
	room      Room
	client    *http.Client
	userAgent string
	logger    *slog.Logger
	mux       *flvMuxer

	// base maps the current feed's timestamps onto the output timeline.
	base                 int64
	lastVideo, lastAudio int64
}

type seatResult struct {
	seat seatStream
	err  error
}

func (rec *recording) run(ctx context.Context, first seatStream) error {
	cur := startFeed(first)
	var next *feed
	defer func() {
		if cur != nil {
			cur.seat.Close()
		}
		if next != nil {
			next.seat.Close()
		}
	}()

	var opening chan seatResult
	attempts := 0
	takeSeat := func(delay time.Duration) {
		opening = make(chan seatResult, 1)
		go func(out chan<- seatResult) {
			if delay > 0 {
				select {
				case <-time.After(delay):
				case <-ctx.Done():
					out <- seatResult{err: ctx.Err()}
					return
				}
			}
			s, err := openSeatFunc(ctx, rec.room, rec.client, rec.userAgent, rec.logger)
			out <- seatResult{s, err}
		}(opening)
	}

	rotate := time.NewTimer(rotateAfter)
	defer rotate.Stop()
	stall := time.NewTimer(mediaTimeout)
	defer stall.Stop()

	for {
		var curSamples, nextSamples chan sample
		if cur != nil {
			curSamples = cur.samples
		}
		if next != nil {
			nextSamples = next.samples
		}
		select {
		case <-ctx.Done():
			if opening != nil {
				go func(c chan seatResult) {
					if r := <-c; r.seat != nil {
						r.seat.Close()
					}
				}(opening)
			}
			return nil

		case smp, ok := <-curSamples:
			if !ok {
				rec.logger.Debug("LiveJasmin seat's stream ended", "error", cur.err)
				cur.seat.Close()
				cur = nil
				if next == nil && opening == nil {
					takeSeat(0)
				}
				continue
			}
			if err := rec.write(cur, smp); err != nil {
				return err
			}
			resetTimer(stall, mediaTimeout)

		case smp, ok := <-nextSamples:
			if !ok {
				rec.logger.Debug("LiveJasmin next seat's stream ended before it was spliced in", "error", next.err)
				next.seat.Close()
				next = nil
				if opening == nil {
					takeSeat(seatRetry)
				}
				continue
			}
			if !smp.video || !smp.key {
				continue
			}
			if cur != nil {
				cur.seat.Close()
			}
			cur, next = next, nil
			attempts = 0
			resetTimer(rotate, rotateAfter-time.Since(cur.joined))
			if err := rec.write(cur, smp); err != nil {
				return err
			}
			resetTimer(stall, mediaTimeout)

		case res := <-opening:
			opening = nil
			if res.err != nil {
				if ctx.Err() != nil {
					return nil
				}
				if errors.Is(res.err, ErrEnded) || errors.Is(res.err, errNoVideo) {
					if cur == nil {
						rec.logger.Info("LiveJasmin room has no stream for guests; ending", "error", res.err)
						return nil
					}
					// The current seat still streams; it settles the
					// question when it runs out.
					continue
				}
				attempts++
				rec.logger.Warn("Failed to take a LiveJasmin seat", "attempt", attempts, "error", res.err)
				if cur == nil && attempts >= seatAttempts {
					return fmt.Errorf("livejasmin: take a seat: %w", res.err)
				}
				if cur == nil {
					takeSeat(seatRetry)
				}
				continue
			}
			next = startFeed(res.seat)
			if cur != nil {
				rec.logger.Debug("Took the next LiveJasmin seat")
			}

		case <-rotate.C:
			if next == nil && opening == nil {
				takeSeat(0)
			}
			rotate.Reset(rotateAfter)

		case <-stall.C:
			return fmt.Errorf("livejasmin: no media for %s", mediaTimeout)
		}
	}
}

// write puts one sample of f on the output timeline. A feed starts at its
// first keyframe, placed one frame after the last frame written.
func (rec *recording) write(f *feed, smp sample) error {
	if !f.started {
		if !smp.video || !smp.key {
			return nil
		}
		f.started = true
		if rec.lastVideo < 0 {
			rec.base = -smp.dts
		} else {
			rec.base = rec.lastVideo + frameGap - smp.dts
		}
		if err := rec.mux.setConfig(rec.base+smp.dts, smp.avcC, smp.asc); err != nil {
			return err
		}
	}
	ts := rec.base + smp.dts
	if smp.video {
		if ts <= rec.lastVideo {
			return nil
		}
		rec.lastVideo = ts
		return rec.mux.writeVideo(ts, smp.cts, smp.key, smp.data)
	}
	if ts < 0 || ts <= rec.lastAudio {
		return nil
	}
	rec.lastAudio = ts
	return rec.mux.writeAudio(ts, smp.data)
}

func resetTimer(t *time.Timer, d time.Duration) {
	if !t.Stop() {
		select {
		case <-t.C:
		default:
		}
	}
	t.Reset(max(d, 0))
}

// UserAgent picks the User-Agent a seat presents: the stream's own header if
// it carries one, else the writer's.
func UserAgent(fallback string, headers map[string]string) string {
	for k, v := range headers {
		if strings.EqualFold(k, "User-Agent") && v != "" {
			return v
		}
	}
	if fallback != "" {
		return fallback
	}
	return useragent.Fallback
}

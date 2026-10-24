// Package cameraprive takes a seat in a CameraPrive room, for recording its
// performer's free chat.
//
// CameraPrive plays a room as LL-HLS of fMP4 from Flashphoner servers
// (*.livecamcdn.com:8445), under an auth query that names a viewer seated in
// the room's chat: a Faye client on the room's chat server holding the keys
// the room page hands that viewer. A guest's free time is three minutes, after
// which the media server answers 401. The free time is the viewer's in that
// room, not the seat's: a guest who leaves early and joins again resumes it,
// and is refused near its end, but one who joins again once it has run out
// gets three minutes more, and the segment sequence runs on across seats. So a
// room is named by a cameraprive://<slug> URL, and whoever records it holds a
// seat itself until the room says its time is up, then takes the next at once;
// the client it hands the recorder signs every media request with the current
// seat's auth, and holds requests back while a new seat is being taken. The
// live playlist holds about six seconds, which covers the change.
//
// A room admits one guest seat per viewer, so a seat is left before the next is
// taken. Some rooms admit only signed-in viewers, and a room refuses some joins
// for no reason it gives. Where the recorder holds the user's CameraPrive login,
// a member with credits takes a member seat, which has no time limit; a member
// without credits has a few minutes a day, so takes guest seats, and a member
// seat only when the room turns guests away.
package cameraprive

import (
	"bytes"
	"context"
	"encoding/json"
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

// Scheme names a room to record.
const Scheme = "cameraprive"

// SiteHost is the site, and the one host a login's cookies may go to.
const SiteHost = "cameraprive.com"

// siteOrigin is the site; a variable so tests can point it at a fake.
var siteOrigin = "https://" + SiteHost

// onSiteCDN reports whether a chat or media server is the site's; a variable
// so tests can point it at a fake.
var onSiteCDN = func(host string) bool { return strings.HasSuffix(host, ".livecamcdn.com") }

var (
	// seatGrace is how long past its free time a seat is held when the room
	// sends no timeout.
	seatGrace = 10 * time.Second
	// pauseWait is how long a performer who stepped away is waited for, and
	// pauseCheck how often the room is read meanwhile.
	pauseWait  = 3 * time.Minute
	pauseCheck = 5 * time.Second
	// joinGap is the pause after the first refused join; it doubles with each
	// refusal after, up to maxJoinGap. The live playlist holds only the last
	// six seconds or so, so the first retries come at once, but a quick run of
	// joins from one address gets every join refused for a while.
	joinGap    = 300 * time.Millisecond
	maxJoinGap = 5 * time.Second
)

const (
	// openTimeout bounds taking a seat, retries included.
	openTimeout = 60 * time.Second
	// requestTimeout bounds one page read or socket write.
	requestTimeout = 15 * time.Second
	// joinTimeout bounds one join, from the page read to the chat's welcome.
	joinTimeout = 20 * time.Second
	// joinRetries is how many joins in a row a room may refuse before the
	// seat is given up; a room refuses a good share of joins for no reason it
	// gives.
	joinRetries = 20
	// memberAfter is how many refused guest joins a member without credits
	// waits before taking a member seat.
	memberAfter = 6
	// freshSeat is how long after a join a media 401 is taken for the media
	// server catching up rather than a lost seat.
	freshSeat = 10 * time.Second
	// guestStay is a guest's free time, where the room does not name it.
	guestStay = 180 * time.Second
	maxPage   = 4 << 20
)

// mediaPort is the media servers' HLS port; a variable so tests can point it
// at a fake.
var mediaPort = "8445"

var (
	// ErrEnded reports a performer who left free chat — stepped away, went
	// offline — while a seat was held or taken.
	ErrEnded = errors.New("cameraprive performer left free chat")
	// ErrPaid reports a performer who started a paid show.
	ErrPaid = errors.New("cameraprive performer started a paid show")
	// ErrLocked reports a room that turned every seat the recorder could take
	// away: one that admits only signed-in viewers, as a rule.
	ErrLocked = errors.New("cameraprive room turned the recorder's seats away")

	// errRefused is one join the room refused without saying why.
	errRefused = errors.New("cameraprive room refused the seat")
	// errPaused is a performer who stepped away from the camera, which ends
	// every viewer's seat; most are back within a minute.
	errPaused = errors.New("cameraprive performer stepped away")
)

var slugPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,59}$`)

// IsURL reports whether u names a CameraPrive room.
func IsURL(u string) bool {
	return strings.HasPrefix(u, Scheme+"://")
}

// Parse returns the slug a cameraprive:// URL names.
func Parse(raw string) (string, error) {
	if !IsURL(raw) {
		return "", fmt.Errorf("not a cameraprive URL: %q", raw)
	}
	slug := strings.TrimSuffix(strings.TrimPrefix(raw, Scheme+"://"), "/")
	if !slugPattern.MatchString(slug) {
		return "", fmt.Errorf("cameraprive URL %q names no room", raw)
	}
	return slug, nil
}

// room is what a room page hands its viewer.
type room struct {
	ChatURL   string // https://chat<n>.livecamcdn.com/<port>
	Instance  string // channel prefix
	Hashkey   string // the viewer's key
	HTSP      string
	HTRP      string
	ChatInfo  string // signed viewer descriptor
	Performer string // performer's hashkey, which names the stream
	Media     string // media server host
	Server    string // chat server number
	Port      string // chat server port
	// Member is a page read signed in; Credits is the member's balance.
	Member  bool
	Credits float64
}

var (
	roomVarRe = regexp.MustCompile(`var (chat_url|cp_chat_instance|cp_init_hashkey|cp_htsp|cp_htrp|cp_chat_info|cp_chat_performer_hashkey|cp_chat_media_url|cp_chat_server|cp_chat_port|cp_user_is_logged|cp_user_credits) = ['"]?([^'";\s]*)`)
	sessionRe = regexp.MustCompile(`[?&]sessionId=([0-9]+)`)
)

func parseRoom(page string) (*room, error) {
	r := &room{}
	for _, m := range roomVarRe.FindAllStringSubmatch(page, -1) {
		v := m[2]
		switch m[1] {
		case "chat_url":
			r.ChatURL = v
		case "cp_chat_instance":
			r.Instance = v
		case "cp_init_hashkey":
			r.Hashkey = v
		case "cp_htsp":
			r.HTSP = v
		case "cp_htrp":
			r.HTRP = v
		case "cp_chat_info":
			r.ChatInfo = v
		case "cp_chat_performer_hashkey":
			r.Performer = v
		case "cp_chat_media_url":
			r.Media = v
		case "cp_chat_server":
			r.Server = v
		case "cp_chat_port":
			r.Port = v
		case "cp_user_is_logged":
			r.Member = v == "true"
		case "cp_user_credits":
			r.Credits, _ = strconv.ParseFloat(v, 64)
		}
	}
	u, err := url.Parse(r.ChatURL)
	if err != nil || u.Scheme != "https" || !onSiteCDN(u.Hostname()) {
		return nil, fmt.Errorf("cameraprive room names no chat server (%q)", r.ChatURL)
	}
	if !onSiteCDN(r.Media) {
		return nil, fmt.Errorf("cameraprive room names no media server (%q)", r.Media)
	}
	if r.Instance == "" || r.Hashkey == "" || r.HTSP == "" || r.HTRP == "" || r.ChatInfo == "" || r.Performer == "" {
		return nil, errors.New("cameraprive room page lacks the seat's keys")
	}
	return r, nil
}

// auth is the media server's auth query value for the room's viewer.
func (r *room) auth() string {
	return strings.Join([]string{r.Hashkey, r.HTSP, r.Performer, r.Port, r.Instance, r.Server}, "-")
}

// master is the HLS master playlist the viewer may play.
func (r *room) master() string {
	return "https://" + r.Media + ":" + mediaPort + "/" + r.Performer + "/" + r.Performer + ".m3u8?auth=" + url.QueryEscape(r.auth())
}

// viewer is one seat held: the chat client and what it signs media with.
type viewer struct {
	f         *faye
	r         *room
	sessionID string
	// stay is the free time the room granted, zero for none to watch.
	stay   time.Duration
	joined time.Time
}

// Seat is the room held through one viewer after another. Its playlist,
// read through Client, stays usable until Done is closed.
type Seat struct {
	// MasterURL is the HLS master playlist of the first seat.
	MasterURL string

	slug      string
	api       *http.Client
	cookie    string
	userAgent string
	logger    *slog.Logger
	cancel    context.CancelFunc
	done      chan struct{}

	// noCredits is set once the login's member page shows no credits, so
	// later seats go straight to guest joins.
	noCredits bool

	// first is the first seat, whose auth and HLS session every playlist
	// handed on names, so the URIs the writer sees stay the same across seats.
	first *viewer

	mu    sync.Mutex
	cur   *viewer
	ready chan struct{} // closed while cur may sign requests
	err   error
}

// Open takes a seat in the room raw names and holds it, and the seats after
// it, until ctx ends or Close is called. client reads the site's pages and
// dials its chat server. login, the user's CameraPrive login when the recorder
// holds one, carries its cookies ("Cookie"), which go to the site alone.
func Open(ctx context.Context, raw string, client *http.Client, userAgent string, login map[string]string, logger *slog.Logger) (*Seat, error) {
	slug, err := Parse(raw)
	if err != nil {
		return nil, err
	}
	if client == nil {
		client = http.DefaultClient
	}
	if userAgent == "" {
		userAgent = useragent.Fallback
	}
	api := *client
	api.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	s := &Seat{
		slug: slug, api: &api, cookie: login["Cookie"], userAgent: userAgent,
		logger: logger, done: make(chan struct{}), ready: make(chan struct{}),
	}
	openCtx, cancelOpen := context.WithTimeout(ctx, openTimeout)
	defer cancelOpen()
	v, err := s.take(openCtx)
	if err != nil {
		logger.Info("Could not take a CameraPrive seat", "room", slug, "reason", err)
		return nil, err
	}
	s.cur, s.first = v, v
	close(s.ready)
	s.MasterURL = v.r.master()
	holdCtx, cancel := context.WithCancel(ctx)
	s.cancel = cancel
	go s.hold(holdCtx)
	return s, nil
}

// Done is closed when the seat is lost or released.
func (s *Seat) Done() <-chan struct{} { return s.done }

// Err reports why the seat was lost: ErrEnded, ErrPaid or ErrLocked when the
// performer left free chat (or stepped away and did not come back within
// pauseWait), started a paid show, or the room turned every seat away; nil
// while it is held or after Close.
func (s *Seat) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

// Close gives the seat up.
func (s *Seat) Close() {
	s.cancel()
	<-s.done
}

// Client returns base, its media requests signed with the current seat's auth.
func (s *Seat) Client(base *http.Client) *http.Client {
	c := *base
	rt := base.Transport
	if rt == nil {
		rt = http.DefaultTransport
	}
	c.Transport = &signer{s: s, base: rt}
	return &c
}

// signer rewrites a media request's auth and sessionId to the current seat's.
type signer struct {
	s    *Seat
	base http.RoundTripper
}

func (t *signer) RoundTrip(req *http.Request) (*http.Response, error) {
	if !onSiteCDN(req.URL.Hostname()) || req.URL.Port() != mediaPort {
		return t.base.RoundTrip(req)
	}
	for try := 0; ; try++ {
		v, err := t.s.current(req.Context())
		if err != nil {
			return nil, err
		}
		r := req.Clone(req.Context())
		q := r.URL.Query()
		q.Set("auth", v.r.auth())
		if q.Has("sessionId") && v.sessionID != "" {
			q.Set("sessionId", v.sessionID)
		}
		r.URL.RawQuery = q.Encode()
		r.URL.Host = v.r.Media + ":" + mediaPort
		r.Host = ""
		resp, err := t.base.RoundTrip(r)
		// A seat taken moments ago can be refused once before the media
		// server knows it.
		if err == nil && resp.StatusCode == http.StatusUnauthorized && try == 0 && time.Since(v.joined) <= freshSeat {
			resp.Body.Close()
			select {
			case <-req.Context().Done():
				return nil, req.Context().Err()
			case <-time.After(time.Second):
			}
			continue
		}
		if err != nil || resp.StatusCode != http.StatusOK || !strings.HasSuffix(r.URL.Path, ".m3u8") {
			return resp, err
		}
		return t.s.restamp(resp, v)
	}
}

// restamp rewrites a later seat's playlist to name the first seat's auth and
// HLS session, as the requests that follow it are signed afresh anyway: the
// same segment then has the same URI whichever seat listed it, and the writer
// sees one stream rather than a new one at each change of seat. It drops
// EXT-X-ENDLIST from every seat's playlist: a seat's playlist can close as its
// free time runs out, and the seat, not the playlist, tells when the
// broadcast ends.
func (s *Seat) restamp(resp *http.Response, v *viewer) (*http.Response, error) {
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxPage))
	resp.Body.Close()
	if err != nil {
		return nil, err
	}
	pairs := []string{"#EXT-X-ENDLIST\r\n", "", "#EXT-X-ENDLIST\n", "", "#EXT-X-ENDLIST", ""}
	if v != s.first {
		pairs = append(pairs, "auth="+v.r.auth(), "auth="+s.first.r.auth())
		if v.sessionID != "" && s.first.sessionID != "" {
			pairs = append(pairs, "sessionId="+v.sessionID, "sessionId="+s.first.sessionID)
		}
	}
	body = []byte(strings.NewReplacer(pairs...).Replace(string(body)))
	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))
	resp.Header.Del("Content-Length")
	return resp, nil
}

// current waits out a change of seat and returns the seat held.
func (s *Seat) current(ctx context.Context) (*viewer, error) {
	for {
		s.mu.Lock()
		ready, v, err := s.ready, s.cur, s.err
		s.mu.Unlock()
		select {
		case <-ready:
			if v == nil {
				return nil, errors.New("cameraprive seat is gone")
			}
			return v, nil
		case <-s.done:
			if err == nil {
				err = errors.New("cameraprive seat is gone")
			}
			return nil, err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// hold watches the seat, takes the next one shortly before its free time
// runs out or when the room lets it go, and stops once the performer leaves
// free chat or no seat can be had.
func (s *Seat) hold(ctx context.Context) {
	defer close(s.done)
	defer func() {
		s.mu.Lock()
		v := s.cur
		s.mu.Unlock()
		if v != nil {
			v.f.leave()
		}
	}()
	for {
		s.mu.Lock()
		v := s.cur
		s.mu.Unlock()
		why, err := s.watch(ctx, v)
		if ctx.Err() != nil {
			return
		}
		paused := errors.Is(err, errPaused)
		if err != nil && !paused {
			s.logger.Info("Leaving the CameraPrive room", "room", s.slug, "reason", err)
			s.fail(err)
			return
		}
		if paused {
			why = "performer stepped away"
			s.logger.Info("CameraPrive performer stepped away; waiting for them", "room", s.slug)
		}
		s.mu.Lock()
		s.ready = make(chan struct{})
		s.mu.Unlock()
		left := time.Now()
		v.f.leave()
		nv, err := s.retake(ctx, paused)
		if ctx.Err() != nil {
			s.mu.Lock()
			s.cur = nil
			s.mu.Unlock()
			return
		}
		if err != nil {
			s.logger.Info("Leaving the CameraPrive room", "room", s.slug, "reason", err)
			s.mu.Lock()
			s.cur = nil
			s.mu.Unlock()
			s.fail(err)
			return
		}
		s.mu.Lock()
		s.cur = nv
		close(s.ready)
		s.mu.Unlock()
		s.logger.Info("Changed CameraPrive seats", "room", s.slug, "reason", why,
			"held", time.Since(v.joined).Round(time.Second).String(),
			"gap", time.Since(left).Round(100*time.Millisecond).String())
	}
}

func (s *Seat) fail(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.err = err
}

// notice is a /notificate message's fields the seat reads.
type notice struct {
	Code                   string `json:"code"`
	Message                string `json:"message"`
	Timeout                bool   `json:"timeout"`
	FreeTimeEnd            bool   `json:"free_time_end"`
	FinalizeSession        bool   `json:"finalize_session"`
	FromDisconnect         bool   `json:"finalize_session_from_disconnect"`
	FromExclusive          bool   `json:"finalize_session_from_exclusive_session"`
	FromKickban            bool   `json:"finalize_session_from_kickban"`
	SessionPaused          bool   `json:"session_paused"`
	InsufficientForSession bool   `json:"insufficient_credits_for_new_session"`
	ConfigUser             bool   `json:"config_user"`
	UserConfig             struct {
		Stay json.RawMessage `json:"time_that_the_user_can_stay"`
	} `json:"user_configurations"`
}

// outcome reads a notice that ends a seat. rotate is a seat whose free time
// is up, or that the room let go, when the next may be taken.
func (n notice) outcome(initialized bool) (rotate bool, err error) {
	switch {
	case n.Timeout || n.FreeTimeEnd:
		return true, nil
	case n.SessionPaused:
		return false, fmt.Errorf("%w (%s)", errPaused, n.Code)
	case n.FromDisconnect:
		return false, fmt.Errorf("%w (%s)", ErrEnded, n.Code)
	case n.FromExclusive, n.InsufficientForSession:
		return false, fmt.Errorf("%w (%s)", ErrPaid, n.Code)
	case n.FromKickban:
		return false, fmt.Errorf("cameraprive room removed the viewer (%s)", n.Code)
	case n.FinalizeSession:
		if strings.Contains(strings.ToLower(n.Message), "paid") {
			return false, ErrPaid
		}
		if !initialized {
			return false, fmt.Errorf("%w (%s %s)", errRefused, n.Code, n.Message)
		}
		return true, nil
	}
	return false, nil
}

// watch follows a held seat until it should be changed (returning why) or
// the room ends it (returning the error).
func (s *Seat) watch(ctx context.Context, v *viewer) (string, error) {
	wctx := ctx
	if v.stay > 0 {
		var cancel context.CancelFunc
		wctx, cancel = context.WithDeadline(ctx, v.joined.Add(v.stay+seatGrace))
		defer cancel()
	}
	for {
		ch, data, err := v.f.next(wctx)
		switch {
		case ctx.Err() != nil:
			return "", nil
		case wctx.Err() != nil:
			return "free time ran out unannounced", nil
		case err != nil:
			return "chat socket lost: " + err.Error(), nil
		}
		switch {
		case strings.HasPrefix(ch, chDisconnected):
			return "the room let the seat go", nil
		case strings.HasPrefix(ch, chNotificate):
			var n notice
			if json.Unmarshal(data, &n) != nil {
				continue
			}
			rotate, err := n.outcome(true)
			if err != nil {
				return "", err
			}
			if rotate {
				return "free time is up", nil
			}
		}
	}
}

// retake takes the next seat. After the performer stepped away, the room is
// read every pauseCheck until they are back in free chat, for up to pauseWait.
func (s *Seat) retake(ctx context.Context, paused bool) (*viewer, error) {
	deadline := time.Now().Add(pauseWait)
	for {
		tctx, cancel := context.WithTimeout(ctx, openTimeout)
		v, err := s.take(tctx)
		cancel()
		if !paused || !errors.Is(err, ErrEnded) || time.Now().After(deadline) {
			return v, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(pauseCheck):
		}
	}
}

// take takes a seat: a member seat for a member with credits, else guest
// seats, joined again while the room refuses them, and a member seat for a
// member without credits once the room has refused a few.
func (s *Seat) take(ctx context.Context) (*viewer, error) {
	memberOK := s.cookie != "" && s.noCredits
	if s.cookie != "" && !s.noCredits {
		r, err := s.fetchRoom(ctx, true)
		if err != nil {
			return nil, err
		}
		if r.Member && r.Credits > 0 {
			v, err := s.join(ctx, r)
			if err == nil {
				s.logger.Info("Took a CameraPrive member seat", "room", s.slug)
				return v, nil
			}
			if !errors.Is(err, errRefused) {
				return nil, err
			}
			s.logger.Debug("CameraPrive room refused the member seat", "room", s.slug, "error", err)
		}
		memberOK = r.Member
		s.noCredits = r.Member && r.Credits <= 0
	}
	var last error
	gap := joinGap
	for i := 0; i < joinRetries; i++ {
		if i > 0 {
			select {
			case <-ctx.Done():
				return nil, fmt.Errorf("%w: %v", ErrLocked, last)
			case <-time.After(gap):
			}
			gap = min(2*gap, maxJoinGap)
		}
		member := memberOK && i >= memberAfter && i%2 == 1
		if ctx.Err() != nil {
			return nil, fmt.Errorf("%w: %v", ErrLocked, last)
		}
		r, err := s.fetchRoom(ctx, member)
		if err != nil {
			return nil, err
		}
		v, err := s.join(ctx, r)
		if err == nil {
			s.logger.Debug("Took a CameraPrive seat", "room", s.slug, "member", r.Member,
				"free_time", v.stay.String(), "attempt", i+1)
			return v, nil
		}
		if !errors.Is(err, errRefused) {
			return nil, err
		}
		last = err
	}
	s.logger.Debug("CameraPrive room refused every seat", "room", s.slug, "error", last)
	return nil, ErrLocked
}

// fetchRoom reads the room page, signed in when member is set. A page the
// site redirects away from is a performer out of free chat.
func (s *Seat) fetchRoom(ctx context.Context, member bool) (*room, error) {
	rctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, http.MethodGet, siteOrigin+"/us/room/"+url.PathEscape(s.slug), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", s.userAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	if member {
		req.Header.Set("Cookie", s.cookie)
	}
	resp, err := s.api.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cameraprive room: %w", err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode/100 == 3:
		return nil, fmt.Errorf("%w (the room page redirects away)", ErrEnded)
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("cameraprive room: status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxPage))
	if err != nil {
		return nil, fmt.Errorf("cameraprive room: %w", err)
	}
	return parseRoom(string(body))
}

// join takes a viewer's seat in the room's chat and opens its playlist.
func (s *Seat) join(ctx context.Context, r *room) (*viewer, error) {
	jctx, cancel := context.WithTimeout(ctx, joinTimeout)
	defer cancel()
	f, err := dialFaye(jctx, s.api, r, s.userAgent)
	if err != nil {
		return nil, err
	}
	// The room says how long the viewer may stay (config_user) before it
	// welcomes them (chat_initialized). A guest has three minutes; a member
	// with credits is taken to have no limit unless the room names one.
	v := &viewer{f: f, r: r, joined: time.Now()}
	if !r.Member || r.Credits <= 0 {
		v.stay = guestStay
	}
	for !f.initialized {
		ch, data, err := f.next(jctx)
		if err != nil {
			f.close()
			return nil, fmt.Errorf("cameraprive chat join: %w", err)
		}
		if !strings.HasPrefix(ch, chNotificate) {
			continue
		}
		var n notice
		if json.Unmarshal(data, &n) != nil {
			continue
		}
		if n.ConfigUser {
			if sec, err := strconv.ParseFloat(strings.Trim(string(n.UserConfig.Stay), `"`), 64); err == nil && sec > 0 {
				v.stay = time.Duration(sec * float64(time.Second))
			}
			continue
		}
		if _, err := n.outcome(false); err != nil {
			f.close()
			if errors.Is(err, errPaused) {
				// Away from the camera is out of free chat until they return.
				return nil, fmt.Errorf("%w (%s)", ErrEnded, n.Code)
			}
			return nil, err
		}
	}
	if err := s.openPlaylist(jctx, v); err != nil {
		f.leave()
		return nil, err
	}
	return v, nil
}

// openPlaylist reads the seat's master playlist, for the HLS session it names.
func (s *Seat) openPlaylist(ctx context.Context, v *viewer) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.r.master(), nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", s.userAgent)
	resp, err := s.api.Do(req)
	if err != nil {
		// A media server that drops the request is joined again like a refusal.
		return fmt.Errorf("%w (playlist: %v)", errRefused, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxPage))
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		// The performer is in free chat but not publishing.
		return fmt.Errorf("%w (no stream published)", ErrEnded)
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("%w (playlist %d)", errRefused, resp.StatusCode)
	default:
		if resp.StatusCode >= 500 {
			return fmt.Errorf("%w (playlist %d)", errRefused, resp.StatusCode)
		}
		return fmt.Errorf("cameraprive playlist: status %d", resp.StatusCode)
	}
	if m := sessionRe.FindSubmatch(body); m != nil {
		v.sessionID = string(m[1])
	}
	return nil
}

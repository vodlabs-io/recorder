package cameraprive

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestParse(t *testing.T) {
	if slug, err := Parse("cameraprive://srta-mornni"); err != nil || slug != "srta-mornni" {
		t.Errorf("Parse = %q, %v", slug, err)
	}
	for _, bad := range []string{"cameraprive://", "cameraprive://Bad Name", "peeks://1"} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("Parse(%q) accepted", bad)
		}
	}
}

const roomPage = `<script> var chat_url = 'https://chat2.livecamcdn.com/8619'; </script>
<script> var cp_user_is_logged = %s; var cp_user_credits = %s; var cp_chat_server = '2'; var cp_chat_instance = 'inst'; var cp_chat_media_url = 'tuna.livecamcdn.com'; var cp_init_hashkey = 'hk'; var cp_htsp = 'sp'; var cp_htrp = 'rp'; var cp_chat_port = 8619; var cp_chat_info = "info"; var cp_chat_performer_hashkey = 'perf'; </script>`

func TestParseRoom(t *testing.T) {
	r, err := parseRoom(fmt.Sprintf(roomPage, "false", "0"))
	if err != nil {
		t.Fatal(err)
	}
	if r.Member || r.Credits != 0 || r.Port != "8619" || r.Server != "2" || r.Media != "tuna.livecamcdn.com" {
		t.Errorf("guest room: %+v", r)
	}
	if want := "https://tuna.livecamcdn.com:8445/perf/perf.m3u8?auth=hk-sp-perf-8619-inst-2"; r.master() != want {
		t.Errorf("master %q, want %q", r.master(), want)
	}
	r, err = parseRoom(fmt.Sprintf(roomPage, "true", "12.50"))
	if err != nil || !r.Member || r.Credits != 12.5 {
		t.Errorf("member room: %+v, %v", r, err)
	}
	if _, err := parseRoom(strings.Replace(fmt.Sprintf(roomPage, "false", "0"), "tuna.livecamcdn.com", "evil.example", 1)); err == nil {
		t.Error("a media server off the site's CDN was accepted")
	}
}

func TestNoticeOutcome(t *testing.T) {
	cases := []struct {
		json        string
		initialized bool
		rotate      bool
		err         error
	}{
		{`{"timeout":true}`, true, true, nil},
		{`{"code":"U2031","session_paused":true}`, true, false, errPaused},
		{`{"code":"U2026","finalize_session_from_disconnect":true}`, true, false, ErrEnded},
		{`{"code":"U2019","insufficient_credits_for_new_session":true}`, true, false, ErrPaid},
		{`{"code":"U2002","finalize_session":true,"message":"Anonimous user when chat is paid on auth"}`, false, false, ErrPaid},
		{`{"code":"U2006","finalize_session":true,"message":"Server error on connect"}`, false, false, errRefused},
		{`{"set_new_topic":true}`, true, false, nil},
	}
	for _, c := range cases {
		var n notice
		if err := json.Unmarshal([]byte(c.json), &n); err != nil {
			t.Fatal(err)
		}
		rotate, err := n.outcome(c.initialized)
		if rotate != c.rotate || !errors.Is(err, c.err) || (c.err == nil) != (err == nil) {
			t.Errorf("%s: rotate %v err %v", c.json, rotate, err)
		}
	}
}

// fakeSite is the site, its chat server and its media server in one: each
// room page hands out a new viewer (hk1, hk2, …); the chat refuses the first
// refuse joins, admits the rest with free time stay, and refuses a second
// seat while one is held; the media server serves a viewer seated in chat.
type fakeSite struct {
	t      *testing.T
	srv    *httptest.Server
	stay   string
	refuse int
	// pause makes the first seat's performer step away, and the room page
	// redirect until pausedUntil.
	pause       bool
	pausedUntil time.Time
	// drop is how many master playlist reads of seated viewers to fail:
	// odd ones by closing the connection, even ones with a 502.
	drop int

	mu      sync.Mutex
	pages   int
	joins   int
	held    map[string]bool // viewers seated
	cookies []string
	served  []string // auth of each media request
}

func newFakeSite(t *testing.T, stay string, refuse int) *fakeSite {
	f := &fakeSite{t: t, stay: stay, refuse: refuse, held: map[string]bool{}}
	f.srv = httptest.NewTLSServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	host, port, _ := net.SplitHostPort(strings.TrimPrefix(f.srv.URL, "https://"))
	oldCheck := pauseCheck
	pauseCheck = 100 * time.Millisecond
	t.Cleanup(func() { pauseCheck = oldCheck })
	oldOrigin, oldCDN, oldPort, oldGrace, oldGap, oldMax := siteOrigin, onSiteCDN, mediaPort, seatGrace, joinGap, maxJoinGap
	siteOrigin, onSiteCDN, mediaPort = f.srv.URL, func(h string) bool { return h == host }, port
	seatGrace, joinGap, maxJoinGap = 100*time.Millisecond, 10*time.Millisecond, 20*time.Millisecond
	t.Cleanup(func() {
		siteOrigin, onSiteCDN, mediaPort, seatGrace, joinGap, maxJoinGap = oldOrigin, oldCDN, oldPort, oldGrace, oldGap, oldMax
	})
	return f
}

func (f *fakeSite) serve(w http.ResponseWriter, r *http.Request) {
	host := strings.TrimPrefix(f.srv.URL, "https://")
	switch {
	case r.URL.Path == "/us/room/anics":
		f.mu.Lock()
		if time.Now().Before(f.pausedUntil) {
			f.mu.Unlock()
			http.Redirect(w, r, "/us/", http.StatusFound)
			return
		}
		f.pages++
		hk := fmt.Sprintf("hk%d", f.pages)
		f.cookies = append(f.cookies, r.Header.Get("Cookie"))
		f.mu.Unlock()
		logged, credits := "false", "0"
		if r.Header.Get("Cookie") != "" {
			logged, credits = "true", "5"
		}
		h, _, _ := net.SplitHostPort(host)
		fmt.Fprintf(w, `<script> var chat_url = 'https://%s/8619'; var cp_user_is_logged = %s; var cp_user_credits = %s; var cp_chat_server = '2'; var cp_chat_instance = 'inst'; var cp_chat_media_url = '%s'; var cp_init_hashkey = '%s'; var cp_htsp = 'sp'; var cp_htrp = 'rp'; var cp_chat_port = 8619; var cp_chat_info = "info"; var cp_chat_performer_hashkey = 'perf'; </script>`,
			host, logged, credits, h, hk)
	case r.URL.Path == "/8619":
		f.chat(w, r)
	case strings.HasPrefix(r.URL.Path, "/perf/"):
		auth := r.URL.Query().Get("auth")
		hk := strings.SplitN(auth, "-", 2)[0]
		f.mu.Lock()
		ok := f.held[hk]
		f.served = append(f.served, auth+"|"+r.URL.Query().Get("sessionId"))
		drop := ok && f.drop > 0 && !strings.Contains(r.URL.Path, "/v/")
		if drop {
			f.drop--
		}
		n := f.drop
		f.mu.Unlock()
		if drop && n%2 == 1 {
			conn, _, _ := w.(http.Hijacker).Hijack()
			conn.Close()
			return
		}
		if drop {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		if !ok {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		fmt.Fprintf(w, "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1\nv/v.m3u8?sessionId=%s&auth=%s\n#EXT-X-ENDLIST\n", strings.TrimPrefix(hk, "hk"), auth)
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeSite) chat(w http.ResponseWriter, r *http.Request) {
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer c.CloseNow()
	ctx := r.Context()
	write := func(msgs ...map[string]any) {
		b, _ := json.Marshal(msgs)
		_ = c.Write(ctx, websocket.MessageText, b)
	}
	var hk string
	defer func() {
		f.mu.Lock()
		delete(f.held, hk)
		f.mu.Unlock()
	}()
	for {
		_, b, err := c.Read(ctx)
		if err != nil {
			return
		}
		var in []map[string]any
		_ = json.Unmarshal(b, &in)
		for _, m := range in {
			ch, _ := m["channel"].(string)
			if ext, ok := m["ext"].(map[string]any); ok {
				hk, _ = ext["hashkey"].(string)
			}
			ack := map[string]any{"channel": ch, "id": m["id"], "successful": true}
			switch ch {
			case "/meta/handshake":
				ack["clientId"] = "cid"
				write(ack)
			case "/meta/connect", "/meta/subscribe", "/meta/disconnect":
				write(ack)
			case "/inst/user/check":
				write(ack, map[string]any{"channel": "/inst/user/checked/" + hk, "data": map[string]any{"server_vars": map[string]any{"streaming_type": "rtc"}}})
			case "/inst/connect/user":
				f.mu.Lock()
				f.joins++
				refused := f.joins <= f.refuse || len(f.held) > 0
				if !refused {
					f.held[hk] = true
				}
				f.mu.Unlock()
				write(ack, map[string]any{"channel": "/inst/notificate/" + hk, "data": map[string]any{"config_user": true, "user_configurations": map[string]any{"credits": 0, "time_that_the_user_can_stay": f.stay}}})
				if refused {
					write(map[string]any{"channel": "/inst/notificate/" + hk, "data": map[string]any{"code": "U2006", "finalize_session": true, "message": "Server error on connect"}})
					continue
				}
				write(map[string]any{"channel": "/inst/chat_initialized/" + hk, "data": map[string]any{"server_vars": map[string]any{}}})
				f.mu.Lock()
				pause := f.pause
				if pause {
					f.pause = false
					f.pausedUntil = time.Now().Add(400 * time.Millisecond)
				}
				f.mu.Unlock()
				if pause {
					write(map[string]any{"channel": "/inst/notificate/" + hk, "data": map[string]any{"code": "U2031", "session_paused": true}})
				}
			case "/inst/disconnect":
				f.mu.Lock()
				delete(f.held, hk)
				f.mu.Unlock()
				write(ack)
			}
		}
	}
}

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestSeatRotatesAndSigns(t *testing.T) {
	f := newFakeSite(t, "1", 2)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s, err := Open(ctx, "cameraprive://anics", f.srv.Client(), "", nil, quietLog())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	client := s.Client(f.srv.Client())
	get := func() int {
		resp, err := client.Get(s.MasterURL)
		if err != nil {
			t.Fatalf("GET master: %v", err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := get(); code != http.StatusOK {
		t.Fatalf("first seat's master: %d", code)
	}
	// The first seat's free time is a second, so by now a later viewer holds
	// the seat, and the master URL handed out first is signed as that one.
	time.Sleep(2500 * time.Millisecond)
	if code := get(); code != http.StatusOK {
		t.Fatalf("master after the change of seat: %d", code)
	}
	resp, err := client.Get(s.MasterURL)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	// The first seat (hk3, after two refusals) names the playlist still.
	if !strings.Contains(string(body), "sessionId=3&auth=hk3-") {
		t.Errorf("later seat's playlist not restamped as the first's:\n%s", body)
	}
	if strings.Contains(string(body), "ENDLIST") {
		t.Errorf("seat's playlist still closed:\n%s", body)
	}
	f.mu.Lock()
	last := f.served[len(f.served)-1]
	joins := f.joins
	f.mu.Unlock()
	if strings.HasPrefix(last, "hk1-") || strings.HasPrefix(last, "hk2-") || strings.HasPrefix(last, "hk3-") {
		t.Errorf("request signed as an old viewer: %s", last)
	}
	if joins < 4 {
		t.Errorf("%d joins; want the two refused, the first seat and a later one", joins)
	}
	select {
	case <-s.Done():
		t.Fatalf("seat lost: %v", s.Err())
	default:
	}
}

func TestSeatRetriesDroppedPlaylist(t *testing.T) {
	f := newFakeSite(t, "180", 0)
	f.drop = 2
	s, err := Open(context.Background(), "cameraprive://anics", f.srv.Client(), "", nil, quietLog())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	f.mu.Lock()
	joins, left := f.joins, f.drop
	f.mu.Unlock()
	// The transport may retry a dropped connection itself, so a join can
	// use up both drops.
	if left != 0 || joins < 2 {
		t.Errorf("%d joins, %d drops left; want the seat joined again after the drops", joins, left)
	}
}

func TestSeatLockedRoom(t *testing.T) {
	f := newFakeSite(t, "180", 1000)
	_, err := Open(context.Background(), "cameraprive://anics", f.srv.Client(), "", nil, quietLog())
	if !errors.Is(err, ErrLocked) {
		t.Fatalf("Open = %v, want ErrLocked", err)
	}
}

func TestSeatMemberWithCredits(t *testing.T) {
	f := newFakeSite(t, "0", 0)
	s, err := Open(context.Background(), "cameraprive://anics", f.srv.Client(), "", map[string]string{"Cookie": "remember-login-user=x"}, quietLog())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	s.mu.Lock()
	v := s.cur
	s.mu.Unlock()
	if !v.r.Member || v.stay != 0 {
		t.Errorf("member seat: member %v stay %v", v.r.Member, v.stay)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.cookies {
		if c != "remember-login-user=x" {
			t.Errorf("room read without the login: %q", c)
		}
	}
	for _, a := range f.served {
		if strings.Contains(a, "remember") {
			t.Errorf("login sent to the media server: %s", a)
		}
	}
}

func TestSeatWaitsOutAPause(t *testing.T) {
	f := newFakeSite(t, "180", 0)
	f.pause = true
	s, err := Open(context.Background(), "cameraprive://anics", f.srv.Client(), "", nil, quietLog())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	time.Sleep(1500 * time.Millisecond)
	select {
	case <-s.Done():
		t.Fatalf("seat lost over a pause: %v", s.Err())
	default:
	}
	resp, err := s.Client(f.srv.Client()).Get(s.MasterURL)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("master after the pause: %v %v", resp, err)
	}
	resp.Body.Close()
	f.mu.Lock()
	defer f.mu.Unlock()
	if last := f.served[len(f.served)-1]; !strings.HasPrefix(last, "hk2-") {
		t.Errorf("after the pause the master was signed as %s; want the second viewer", last)
	}
}

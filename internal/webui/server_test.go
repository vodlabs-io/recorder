package webui

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

type harness struct {
	t     *testing.T
	srv   *Server
	base  string
	ns    *server.Server
	dir   string
	token atomic.Value
}

func newHarness(t *testing.T, mutate ...func(*Config)) *harness {
	t.Helper()
	old := pollInterval
	pollInterval = 50 * time.Millisecond
	t.Cleanup(func() { pollInterval = old })

	ns, err := server.NewServer(&server.Options{
		Host: "127.0.0.1", Port: -1, JetStream: true, StoreDir: t.TempDir(),
		NoLog: true, NoSigs: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	ns.Start()
	if !ns.ReadyForConnections(5 * time.Second) {
		t.Fatal("nats not ready")
	}
	t.Cleanup(ns.Shutdown)

	h := &harness{t: t, ns: ns, dir: t.TempDir()}
	h.token.Store(testToken)
	cfg := Config{
		Host:         "127.0.0.1",
		Port:         0,
		Dir:          h.dir,
		ResolveToken: func() (string, error) { return h.token.Load().(string), nil },
		Dial:         ns.InProcessConn,
	}
	for _, m := range mutate {
		m(&cfg)
	}
	h.srv, err = Start(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.srv.Close)
	h.base = "http://" + h.srv.Addr() + "/"
	return h
}

// client is a browser: a cookie jar and no redirects followed.
func (h *harness) client() *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{
		Jar:           jar,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Timeout:       5 * time.Second,
	}
}

func (h *harness) do(c *http.Client, method, path, body string, hdr ...string) *http.Response {
	h.t.Helper()
	req, err := http.NewRequest(method, h.base+path, strings.NewReader(body))
	if err != nil {
		h.t.Fatal(err)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	res, err := c.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	res.Body.Close()
	return res
}

func (h *harness) signIn(c *http.Client, token string) int {
	b, _ := json.Marshal(map[string]string{"token": token})
	return h.do(c, "POST", "api/session", string(b), "Content-Type", "application/json").StatusCode
}

func (h *harness) cookieHeader(c *http.Client) string {
	u, _ := url.Parse(h.base)
	var parts []string
	for _, ck := range c.Jar.Cookies(u) {
		parts = append(parts, ck.Name+"="+ck.Value)
	}
	return strings.Join(parts, "; ")
}

// dialNATS connects nats.go through the bridge, as wsconnect does in the
// browser.
func (h *harness) dialNATS(c *http.Client, opts ...nats.Option) (*nats.Conn, error) {
	cookie := h.cookieHeader(c)
	dialer := wsDialer{url: "ws://" + h.srv.Addr() + "/nats", header: http.Header{
		"Cookie": {cookie},
		"Origin": {"http://" + h.srv.Addr()},
	}}
	return nats.Connect("nats://"+h.srv.Addr(), append([]nats.Option{
		nats.SetCustomDialer(dialer), nats.NoReconnect(), nats.Timeout(3 * time.Second),
	}, opts...)...)
}

type wsDialer struct {
	url    string
	header http.Header
}

func (d wsDialer) Dial(_, _ string) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, d.url, &websocket.DialOptions{HTTPHeader: d.header})
	if err != nil {
		return nil, err
	}
	return websocket.NetConn(context.Background(), c, websocket.MessageBinary), nil
}

func TestHealthzAndHeaders(t *testing.T) {
	h := newHarness(t)
	c := h.client()
	for _, path := range []string{"healthz", "signin", "signin.js", "api/session", "", "assets/x.js", "nats", "nope"} {
		res := h.do(c, "GET", path, "")
		for _, name := range []string{"Content-Security-Policy", "X-Content-Type-Options",
			"Referrer-Policy", "X-Frame-Options", "Cross-Origin-Opener-Policy",
			"Cross-Origin-Resource-Policy", "Permissions-Policy"} {
			if res.Header.Get(name) == "" {
				t.Errorf("GET /%s lacks %s", path, name)
			}
		}
	}
	res, err := c.Get(h.base + "healthz")
	if err != nil {
		t.Fatal(err)
	}
	var buf [8]byte
	n, _ := res.Body.Read(buf[:])
	res.Body.Close()
	if res.StatusCode != 200 || string(buf[:n]) != "ok" {
		t.Errorf("healthz = %d %q", res.StatusCode, buf[:n])
	}
}

func TestSignInFlow(t *testing.T) {
	h := newHarness(t)
	c := h.client()

	res := h.do(c, "GET", "", "")
	if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "signin" {
		t.Fatalf("GET / without a session = %d %q", res.StatusCode, res.Header.Get("Location"))
	}
	if res := h.do(c, "GET", "assets/a/b.js", ""); res.Header.Get("Location") != "../../signin" {
		t.Errorf("asset redirect = %q", res.Header.Get("Location"))
	}
	if code := h.do(c, "GET", "api/session", "").StatusCode; code != 401 {
		t.Errorf("api/session before sign-in = %d", code)
	}
	if code := h.signIn(c, "wrong"); code != 401 {
		t.Errorf("wrong token = %d", code)
	}
	if code := h.signIn(c, testToken); code != 204 {
		t.Fatalf("right token = %d", code)
	}
	if code := h.do(c, "GET", "api/session", "").StatusCode; code != 204 {
		t.Errorf("api/session after sign-in = %d", code)
	}
	if code := h.do(c, "GET", "", "").StatusCode; code != 200 {
		t.Errorf("GET / after sign-in = %d", code)
	}

	// The cookie over plain HTTP.
	u, _ := url.Parse(h.base)
	cks := c.Jar.Cookies(u)
	if len(cks) != 1 || cks[0].Name != cookieHTTP {
		t.Fatalf("cookies = %+v", cks)
	}

	if code := h.do(c, "POST", "api/signout", "").StatusCode; code != 204 {
		t.Fatalf("sign-out = %d", code)
	}
	if code := h.do(c, "GET", "api/session", "").StatusCode; code != 401 {
		t.Errorf("api/session after sign-out = %d", code)
	}
}

// A revoked cookie stays dead even when replayed.
func TestSignOutRevokesReplayedCookie(t *testing.T) {
	h := newHarness(t)
	c := h.client()
	h.signIn(c, testToken)
	stolen := h.cookieHeader(c)
	h.do(c, "POST", "api/signout", "")
	if code := h.do(h.client(), "GET", "api/session", "", "Cookie", stolen).StatusCode; code != 401 {
		t.Errorf("replayed cookie after sign-out = %d", code)
	}
}

func TestCookieAttributes(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.TrustedProxies = []string{"127.0.0.1/32"} })
	b := `{"token":"` + testToken + `"}`
	plain := h.do(h.client(), "POST", "api/session", b)
	if sc := plain.Header.Get("Set-Cookie"); !strings.HasPrefix(sc, cookieHTTP+"=") ||
		!strings.Contains(sc, "HttpOnly") || !strings.Contains(sc, "SameSite=Lax") ||
		strings.Contains(sc, "Secure") || !strings.Contains(sc, "Path=/") {
		t.Errorf("HTTP Set-Cookie = %q", sc)
	}
	viaTLS := h.do(h.client(), "POST", "api/session", b, "X-Forwarded-Proto", "https")
	if sc := viaTLS.Header.Get("Set-Cookie"); !strings.HasPrefix(sc, cookieHTTPS+"=") ||
		!strings.Contains(sc, "Secure") || strings.Contains(sc, "Domain=") {
		t.Errorf("HTTPS Set-Cookie = %q", sc)
	}
}

func TestUntrustedForwardedProtoIgnored(t *testing.T) {
	h := newHarness(t)
	res := h.do(h.client(), "POST", "api/session", `{"token":"`+testToken+`"}`, "X-Forwarded-Proto", "https")
	if sc := res.Header.Get("Set-Cookie"); !strings.HasPrefix(sc, cookieHTTP+"=") {
		t.Errorf("an untrusted client's X-Forwarded-Proto changed the cookie: %q", sc)
	}
}

func TestCrossOriginPostRefused(t *testing.T) {
	h := newHarness(t)
	res := h.do(h.client(), "POST", "api/session", `{"token":"`+testToken+`"}`,
		"Origin", "http://evil.example", "Sec-Fetch-Site", "cross-site")
	if res.StatusCode != http.StatusForbidden {
		t.Errorf("cross-origin sign-in = %d", res.StatusCode)
	}
}

func TestSignInRateLimited(t *testing.T) {
	h := newHarness(t)
	c := h.client()
	for range failsPerIP {
		h.signIn(c, "wrong")
	}
	b, _ := json.Marshal(map[string]string{"token": testToken})
	res := h.do(c, "POST", "api/session", string(b))
	if res.StatusCode != http.StatusTooManyRequests || res.Header.Get("Retry-After") == "" {
		t.Errorf("after %d failures = %d, Retry-After %q", failsPerIP, res.StatusCode, res.Header.Get("Retry-After"))
	}
}

func TestSignInBodyLimit(t *testing.T) {
	h := newHarness(t)
	big := `{"token":"` + strings.Repeat("x", maxSignInBody) + `"}`
	if code := h.do(h.client(), "POST", "api/session", big).StatusCode; code != http.StatusRequestEntityTooLarge {
		t.Errorf("oversized body = %d", code)
	}
}

func TestBridgeRefusals(t *testing.T) {
	h := newHarness(t)
	anon := h.client()
	if _, err := h.dialNATS(anon); err == nil {
		t.Fatal("bridge opened without a session")
	}
	if code := h.do(anon, "GET", "nats", "", "Connection", "Upgrade", "Upgrade", "websocket",
		"Sec-WebSocket-Version", "13", "Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==").StatusCode; code != 401 {
		t.Errorf("upgrade without a session = %d", code)
	}

	c := h.client()
	h.signIn(c, testToken)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, res, err := websocket.Dial(ctx, "ws://"+h.srv.Addr()+"/nats", &websocket.DialOptions{
		HTTPHeader: http.Header{"Cookie": {h.cookieHeader(c)}, "Origin": {"http://evil.example"}},
	})
	if err == nil || res == nil || res.StatusCode != http.StatusForbidden {
		t.Errorf("cross-origin socket: err %v, res %v", err, res)
	}
}

func TestBridgeAllowedOrigin(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.AllowedOrigins = []string{"https://rec.example"} })
	c := h.client()
	h.signIn(c, testToken)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, "ws://"+h.srv.Addr()+"/nats", &websocket.DialOptions{
		HTTPHeader: http.Header{"Cookie": {h.cookieHeader(c)}, "Origin": {"https://rec.example"}},
	})
	if err != nil {
		t.Fatalf("allowed origin refused: %v", err)
	}
	ws.CloseNow()
}

func TestBridgeNATS(t *testing.T) {
	h := newHarness(t)
	c := h.client()
	h.signIn(c, testToken)

	backend, err := nats.Connect(h.ns.ClientURL())
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	_, err = backend.Subscribe("echo", func(m *nats.Msg) {
		reply := nats.NewMsg(m.Reply)
		reply.Data = m.Data
		reply.Header = m.Header
		_ = m.RespondMsg(reply)
	})
	if err != nil {
		t.Fatal(err)
	}
	backend.Flush()

	nc, err := h.dialNATS(c)
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()

	// Request/reply with headers.
	msg := nats.NewMsg("echo")
	msg.Header.Set("X-Test", "1")
	msg.Data = []byte("hi")
	got, err := nc.RequestMsg(msg, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if string(got.Data) != "hi" || got.Header.Get("X-Test") != "1" {
		t.Errorf("echo = %q %v", got.Data, got.Header)
	}

	// A payload near the server's 1 MB limit.
	big := make([]byte, 900<<10)
	for i := range big {
		big[i] = byte(i)
	}
	got, err = nc.Request("echo", big, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Data) != len(big) || got.Data[len(big)-1] != big[len(big)-1] {
		t.Errorf("big echo came back %d bytes", len(got.Data))
	}

	// A KV watch, as the UI keeps one open for its whole life.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	bjs, _ := jetstream.New(backend)
	bkv, err := bjs.CreateOrUpdateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: "streamers"})
	if err != nil {
		t.Fatal(err)
	}
	js, _ := jetstream.New(nc)
	kv, err := js.KeyValue(ctx, "streamers")
	if err != nil {
		t.Fatal(err)
	}
	w, err := kv.Watch(ctx, "daemon.>")
	if err != nil {
		t.Fatal(err)
	}
	defer w.Stop()
	if _, err := bkv.PutString(ctx, "daemon.x", "1"); err != nil {
		t.Fatal(err)
	}
	for e := range w.Updates() {
		if e != nil && e.Key() == "daemon.x" {
			break
		}
	}
}

// The browser parses NATS as a stream: a command split across frames, or
// several in one, must arrive intact.
func TestBridgeSplitFrames(t *testing.T) {
	h := newHarness(t)
	c := h.client()
	h.signIn(c, testToken)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, "ws://"+h.srv.Addr()+"/nats", &websocket.DialOptions{
		HTTPHeader: http.Header{"Cookie": {h.cookieHeader(c)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer ws.CloseNow()
	for _, frame := range []string{`CONNECT {"verbose":false,"pedantic":false}` + "\r\nPI", "NG\r\n"} {
		if err := ws.Write(ctx, websocket.MessageBinary, []byte(frame)); err != nil {
			t.Fatal(err)
		}
	}
	var seen strings.Builder
	for !strings.Contains(seen.String(), "PONG\r\n") {
		_, b, err := ws.Read(ctx)
		if err != nil {
			t.Fatalf("no PONG; read %q: %v", seen.String(), err)
		}
		seen.Write(b)
	}
	if !strings.HasPrefix(seen.String(), "INFO ") {
		t.Errorf("stream did not open with INFO: %q", seen.String())
	}
}

func TestBridgePings(t *testing.T) {
	old := pingInterval
	t.Cleanup(func() { pingInterval = old })
	pingInterval = 50 * time.Millisecond

	h := newHarness(t)
	c := h.client()
	h.signIn(c, testToken)
	pinged := make(chan struct{}, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, "ws://"+h.srv.Addr()+"/nats", &websocket.DialOptions{
		HTTPHeader: http.Header{"Cookie": {h.cookieHeader(c)}},
		OnPingReceived: func(context.Context, []byte) bool {
			select {
			case pinged <- struct{}{}:
			default:
			}
			return true
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer ws.CloseNow()
	go func() {
		for {
			if _, _, err := ws.Read(ctx); err != nil {
				return
			}
		}
	}()
	select {
	case <-pinged:
	case <-ctx.Done():
		t.Fatal("no ping")
	}
}

func waitClosed(t *testing.T, nc *nats.Conn, what string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !nc.IsClosed() {
		if time.Now().After(deadline) {
			t.Fatalf("socket still open after %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestBridgeClosesOnSignOut(t *testing.T) {
	h := newHarness(t)
	c, other := h.client(), h.client()
	h.signIn(c, testToken)
	h.signIn(other, testToken)
	nc, err := h.dialNATS(c)
	if err != nil {
		t.Fatal(err)
	}
	keep, err := h.dialNATS(other)
	if err != nil {
		t.Fatal(err)
	}
	defer keep.Close()
	h.do(c, "POST", "api/signout", "")
	waitClosed(t, nc, "sign-out")
	if keep.IsClosed() {
		t.Error("another session's socket closed too")
	}
}

func TestBridgeClosesOnRotation(t *testing.T) {
	h := newHarness(t)
	c := h.client()
	h.signIn(c, testToken)
	nc, err := h.dialNATS(c)
	if err != nil {
		t.Fatal(err)
	}
	h.token.Store(testToken + "-rotated")
	waitClosed(t, nc, "token rotation")
	if code := h.do(c, "GET", "api/session", "").StatusCode; code != 401 {
		t.Errorf("old cookie after rotation = %d", code)
	}
}

func TestBridgeClosesOnSecretChange(t *testing.T) {
	h := newHarness(t)
	c := h.client()
	h.signIn(c, testToken)
	nc, err := h.dialNATS(c)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(h.dir, SecretFileName)); err != nil {
		t.Fatal(err)
	}
	waitClosed(t, nc, "secret change")
}

func TestBridgeClosesOnShutdown(t *testing.T) {
	h := newHarness(t)
	c := h.client()
	h.signIn(c, testToken)
	nc, err := h.dialNATS(c)
	if err != nil {
		t.Fatal(err)
	}
	h.srv.Close()
	waitClosed(t, nc, "shutdown")
}

func TestBridgeConnectionCap(t *testing.T) {
	h := newHarness(t)
	c := h.client()
	h.signIn(c, testToken)
	var open []*nats.Conn
	defer func() {
		for _, nc := range open {
			nc.Close()
		}
	}()
	for i := range maxBridges {
		nc, err := h.dialNATS(c)
		if err != nil {
			t.Fatalf("connection %d refused: %v", i, err)
		}
		open = append(open, nc)
	}
	if code := h.do(c, "GET", "nats", "", "Connection", "Upgrade", "Upgrade", "websocket",
		"Sec-WebSocket-Version", "13", "Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==").StatusCode; code != http.StatusServiceUnavailable {
		t.Errorf("connection past the cap = %d", code)
	}
}

package cloud

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/zalando/go-keyring"

	"github.com/vodlabs-io/recorder/internal/platformauth"
	"github.com/vodlabs-io/recorder/internal/vlconfig"
	"github.com/vodlabs-io/recorder/internal/vllog"
)

// These tests assert, against the real scrape handler, that an authenticated
// check's credentials reach the platform's own hosts and nowhere else.

// captureRequest runs one scrape through the handler and returns the headers
// the target actually received.
func captureRequest(t *testing.T, store *platformauth.Store, authPlatform, path string) http.Header {
	t.Helper()

	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	log, err := vllog.NewFileLogger(vlconfig.LoggerConfig{Mode: "off", Level: "error"}, "test")
	if err != nil {
		t.Fatalf("NewFileLogger: %v", err)
	}
	h := &scrapeHandler{
		logger:        log,
		httpClient:    &http.Client{Timeout: 10 * time.Second},
		cookies:       store,
		checkTarget:   allowAnyScrapeTarget,
		maxConcurrent: 10,
		maxPerMinute:  100,
		lastMinute:    time.Now(),
	}

	// A reply-less Msg: the response is discarded, the outbound request is the
	// subject of the test.
	data, err := json.Marshal(scrapeRequest{
		URL: srv.URL + path, Method: http.MethodGet, AuthPlatform: authPlatform,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	h.handleScrapeRequest(&nats.Msg{Data: data})
	if got == nil {
		t.Fatal("the handler never made the request")
	}
	return got
}

// youtubeStore returns a store holding a YouTube login shaped like a real
// capture: the session marker plus the cookie the signature is derived from.
func youtubeStore(t *testing.T, cookies ...platformauth.Cookie) *platformauth.Store {
	t.Helper()
	keyring.MockInit()
	store := platformauth.NewStore(
		platformauth.WithFilePath(t.TempDir() + "/cookies.json"),
	)
	if len(cookies) == 0 {
		cookies = []platformauth.Cookie{
			{Name: "LOGIN_INFO", Value: "login-info-value"},
			{Name: "__Secure-3PAPISID", Value: "sapisid-value"},
			{Name: "SID", Value: "sid-value"},
		}
	}
	if err := store.Set(platformauth.Session{
		Platform: "youtube", Cookies: cookies, CapturedAt: time.Now().Unix(), Healthy: true,
	}); err != nil {
		t.Fatalf("store.Set: %v", err)
	}
	return store
}

func TestAuthedScrapeSendsCookiesAndSignature(t *testing.T) {
	// vlconfig's embedded default carries YouTube's descriptor, including the
	// auth scheme; nothing here may override it (see the client-authoritative
	// guard in vlconfig).
	if l := vlconfig.LoginFlowFor("youtube"); l == nil || l.Auth == nil {
		t.Skip("youtube login descriptor unavailable")
	}

	// The handler only attaches to the platform's own hosts, so the request has
	// to look like one. httptest serves on 127.0.0.1, which never matches
	// youtube.com — hence the host allowlist is exercised separately below and
	// this case asserts the negative it implies.
	got := captureRequest(t, youtubeStore(t), "youtube", "/youtubei/v1/player")
	if got.Get("Cookie") != "" {
		t.Error("PRIVACY: cookies were sent to a host outside the platform's domains")
	}
	if got.Get("Authorization") != "" {
		t.Error("PRIVACY: a credential-derived signature was sent to a host outside the platform's domains")
	}
}

// TestAuthHeadersForYouTubeDescriptor checks the descriptor and the signing code
// agree: the scheme named in the embedded default must produce the headers
// InnerTube expects from the jar a capture actually stores.
func TestAuthHeadersForYouTubeDescriptor(t *testing.T) {
	login := vlconfig.LoginFlowFor("youtube")
	if login == nil || login.Auth == nil {
		t.Skip("youtube login descriptor unavailable")
	}
	sess := platformauth.Session{Platform: "youtube", Cookies: []platformauth.Cookie{
		{Name: "LOGIN_INFO", Value: "x"},
		{Name: "__Secure-3PAPISID", Value: "secret"},
	}}

	h := sess.AuthHeaders(login.Auth.Scheme, login.Auth.Cookies, login.Auth.Origin, platformauth.SignedRequest{})
	if h == nil {
		t.Fatal("the descriptor's scheme produced no headers for a well-formed jar")
	}
	if !strings.HasPrefix(h["Authorization"], "SAPISIDHASH ") {
		t.Errorf("Authorization = %q, want a SAPISIDHASH", h["Authorization"])
	}
	if h["X-Origin"] != "https://www.youtube.com" {
		t.Errorf("X-Origin = %q", h["X-Origin"])
	}
	if strings.Contains(h["Authorization"], "secret") {
		t.Error("PRIVACY: the signature leaks the cookie it was derived from")
	}

	// A jar captured without the signing cookie is the failure worth naming: it
	// stores clean, shows Active, and silently authenticates nothing.
	noSecret := platformauth.Session{Platform: "youtube", Cookies: []platformauth.Cookie{
		{Name: "LOGIN_INFO", Value: "x"},
	}}
	if noSecret.HasAuthSecret(login.Auth.Cookies) {
		t.Error("a jar with no signing cookie reported that it has one")
	}
	if noSecret.AuthHeaders(login.Auth.Scheme, login.Auth.Cookies, login.Auth.Origin, platformauth.SignedRequest{}) != nil {
		t.Error("headers were produced from a jar with no signing cookie")
	}
}

// TestUnauthedScrapeSendsNothing is the baseline: with no AuthPlatform the
// request carries no credentials at all, whatever is in the store.
func TestUnauthedScrapeSendsNothing(t *testing.T) {
	got := captureRequest(t, youtubeStore(t), "", "/youtubei/v1/player")
	if got.Get("Cookie") != "" || got.Get("Authorization") != "" {
		t.Error("an unauthenticated scrape carried credentials")
	}
}

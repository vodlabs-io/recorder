package cloud

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/vodlabs-io/recorder/internal/browserfetch"
	"github.com/vodlabs-io/recorder/internal/vlconfig"
	"github.com/vodlabs-io/recorder/internal/vllog"
)

// scrapeThrough runs one scrape through a handler on a real NATS connection and
// returns its reply.
func scrapeThrough(t *testing.T, nc *nats.Conn, withLocal bool, req scrapeRequest) scrapeResponse {
	t.Helper()
	log, err := vllog.NewFileLogger(vlconfig.LoggerConfig{Mode: "off", Level: "error"}, "test")
	if err != nil {
		t.Fatalf("NewFileLogger: %v", err)
	}
	h := &scrapeHandler{
		logger:        log,
		httpClient:    &http.Client{Timeout: 10 * time.Second},
		checkTarget:   allowAnyScrapeTarget,
		maxConcurrent: 10,
		maxPerMinute:  100,
		lastMinute:    time.Now(),
	}
	if withLocal {
		h.localNC = nc
	}
	subject := "scrape.test." + nats.NewInbox()
	sub, err := nc.Subscribe(subject, h.handleScrapeRequest)
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Unsubscribe()
	data, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	reply, err := nc.Request(subject, data, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	var resp scrapeResponse
	if err := json.Unmarshal(reply.Data, &resp); err != nil {
		t.Fatal(err)
	}
	return resp
}

// answerBrowserFetch stands in for a desktop app.
func answerBrowserFetch(t *testing.T, nc *nats.Conn, resp browserfetch.Response) <-chan browserfetch.Request {
	t.Helper()
	got := make(chan browserfetch.Request, 1)
	sub, err := nc.Subscribe(browserfetch.RequestSubject, func(m *nats.Msg) {
		var req browserfetch.Request
		if err := json.Unmarshal(m.Data, &req); err != nil {
			t.Error(err)
		}
		got <- req
		data, _ := json.Marshal(resp)
		_ = m.Respond(data)
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	if err := nc.Flush(); err != nil {
		t.Fatal(err)
	}
	return got
}

func TestBrowserFallbackRunsDirectlyWithNoWebview(t *testing.T) {
	nc, _ := newTestKV(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"keep":1,"drop":2}`))
	}))
	defer srv.Close()

	req := scrapeRequest{
		URL: srv.URL, Method: http.MethodGet, BrowserContext: true, BrowserFallback: true,
		ExtractorJSON: "{keep}",
	}
	for name, withLocal := range map[string]bool{"no local NATS": false, "no desktop": true} {
		resp := scrapeThrough(t, nc, withLocal, req)
		if resp.Error != "" || resp.Status != 200 || resp.Body != `{"keep":1}` {
			t.Errorf("%s: %+v", name, resp)
		}
	}

	req.BrowserFallback = false
	if resp := scrapeThrough(t, nc, true, req); resp.Error == "" {
		t.Errorf("without the fallback it answered %+v", resp)
	}
}

func TestBrowserHostPageCarriesHeadersAndNarrowsTheReply(t *testing.T) {
	nc, _ := newTestKV(t)
	got := answerBrowserFetch(t, nc, browserfetch.Response{Status: 200, Body: `{"keep":1,"drop":2}`})
	resp := scrapeThrough(t, nc, true, scrapeRequest{
		URL: "https://www.example.com/api", Method: http.MethodGet, AuthPlatform: "instagram",
		BrowserContext: true, BrowserHostPage: "/robots.txt", ExtractorJSON: "{keep}",
		Headers: map[string]string{"X-App": "1"},
	})
	if resp.Body != `{"keep":1}` {
		t.Errorf("reply %+v", resp)
	}
	req := <-got
	if req.HostPage != "/robots.txt" || req.Headers["X-App"] != "1" {
		t.Errorf("request %+v", req)
	}
}

// A request that names no host page keeps its headers to itself: a signed page
// sets its own.
func TestBrowserWithoutHostPageSendsNoHeaders(t *testing.T) {
	nc, _ := newTestKV(t)
	got := answerBrowserFetch(t, nc, browserfetch.Response{Status: 200, Body: `{"keep":1}`})
	scrapeThrough(t, nc, true, scrapeRequest{
		URL: "https://www.example.com/api", Method: http.MethodGet, AuthPlatform: "tiktok",
		BrowserContext: true, ExtractorJSON: "{keep}",
		Headers: map[string]string{"X-App": "1"},
	})
	if req := <-got; req.Headers != nil {
		t.Errorf("headers %+v", req.Headers)
	}
}

// All a plugin needs of a login wall is that it was sent there.
func TestBrowserRedirectTravelsWithoutThePage(t *testing.T) {
	nc, _ := newTestKV(t)
	answerBrowserFetch(t, nc, browserfetch.Response{
		Status: 200, Body: "<html>a signed-in page</html>",
		RedirectedTo: "https://www.example.com/accounts/login/",
	})
	resp := scrapeThrough(t, nc, true, scrapeRequest{
		URL: "https://www.example.com/api", Method: http.MethodGet, AuthPlatform: "instagram",
		BrowserContext: true, ExtractorJSON: "{keep}",
	})
	if resp.Error != "" || resp.Body != "" || resp.Headers["Location"] != "https://www.example.com/accounts/login/" {
		t.Errorf("reply %+v", resp)
	}
}

func TestBrowserRequestNamingOnlyARegexIsNarrowedInPage(t *testing.T) {
	nc, _ := newTestKV(t)
	answerBrowserFetch(t, nc, browserfetch.Response{Status: 200, Body: "a\n123"})
	resp := scrapeThrough(t, nc, true, scrapeRequest{
		URL: "https://www.example.com/page", Method: http.MethodGet, AuthPlatform: "instagram",
		BrowserContext: true, ExtractorRegex: `(a)\n(\d+)`,
	})
	if resp.Error != "" || resp.Body != "a\n123" {
		t.Errorf("reply %+v", resp)
	}
}

func TestDirectRegexJoinsItsGroupsAndReportsARedirect(t *testing.T) {
	nc, _ := newTestKV(t)
	mux := http.NewServeMux()
	mux.HandleFunc("/from", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/to", http.StatusFound)
	})
	mux.HandleFunc("/to", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`og="pic" x x "id":"123"`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	resp := scrapeThrough(t, nc, false, scrapeRequest{
		URL: srv.URL + "/from", Method: http.MethodGet,
		ExtractorRegex: `og="([^"]+)".*?"id":"(\d+)"`,
	})
	if resp.Body != "pic\n123" {
		t.Errorf("body %q", resp.Body)
	}
	if resp.Headers["Location"] != srv.URL+"/to" {
		t.Errorf("headers %+v", resp.Headers)
	}
	if err := errors.New(resp.Error); resp.Error != "" {
		t.Error(err)
	}
}

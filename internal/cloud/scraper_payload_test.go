package cloud

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"

	"github.com/vodlabs-io/recorder/internal/vlconfig"
	"github.com/vodlabs-io/recorder/internal/vllog"
)

// TestScrapeReplyOverPayloadLimit checks that a page too big for the cloud
// connection is refused with the backend's "too large" word, and reported as a
// warning naming the host rather than an error report.
func TestScrapeReplyOverPayloadLimit(t *testing.T) {
	const maxPayload = 4096

	srv, err := server.NewServer(&server.Options{
		Host: "127.0.0.1", Port: -1, NoLog: true, NoSigs: true, MaxPayload: maxPayload,
	})
	if err != nil {
		t.Fatalf("nats server: %v", err)
	}
	go srv.Start()
	if !srv.ReadyForConnections(5 * time.Second) {
		t.Fatal("nats server not ready")
	}
	t.Cleanup(srv.Shutdown)

	nc, err := nats.Connect(srv.ClientURL())
	if err != nil {
		t.Fatalf("nats connect: %v", err)
	}
	t.Cleanup(nc.Close)

	page := strings.Repeat("<div>x</div>", 1000) // 12KB, three times the limit
	web := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(page))
	}))
	t.Cleanup(web.Close)

	logDir := t.TempDir()
	logger, err := vllog.NewFileLogger(vlconfig.LoggerConfig{
		Mode: "file", Level: "info", Directory: logDir, FileName: "scraper.log",
	}, "scraper")
	if err != nil {
		t.Fatalf("logger: %v", err)
	}
	t.Cleanup(func() { _ = logger.Close() })

	h := newScrapeHandler(logger, &http.Client{Timeout: 10 * time.Second}, nil, nil, nc)
	h.checkTarget = allowAnyScrapeTarget
	sub, err := nc.Subscribe("scrape.requests", h.handleScrapeRequest)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	reqData, err := json.Marshal(scrapeRequest{URL: web.URL + "/live", Method: http.MethodGet})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	msg, err := nc.Request("scrape.requests", reqData, 15*time.Second)
	if err != nil {
		t.Fatalf("request: %v", err)
	}

	var resp scrapeResponse
	if err := json.Unmarshal(msg.Data, &resp); err != nil {
		t.Fatalf("unmarshal reply: %v", err)
	}
	if !strings.HasPrefix(resp.Error, errTooLargeToRelay) {
		t.Fatalf("reply error = %q, want it to start with %q", resp.Error, errTooLargeToRelay)
	}
	if resp.Body != "" {
		t.Fatalf("refusal carried a body of %d bytes", len(resp.Body))
	}

	raw, err := os.ReadFile(filepath.Join(logDir, "scraper.log"))
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	var line string
	for _, l := range strings.Split(string(raw), "\n") {
		if strings.Contains(l, "Scrape response exceeds NATS max payload") {
			line = l
		}
	}
	if line == "" {
		t.Fatalf("no oversize line in log:\n%s", raw)
	}
	u, _ := url.Parse(web.URL)
	for _, want := range []string{"level=WARN", "host=" + u.Host, "error=", "max_payload=4096"} {
		if !strings.Contains(line, want) {
			t.Errorf("log line missing %q: %s", want, line)
		}
	}
}

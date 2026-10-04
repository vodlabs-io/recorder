package cloud

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/vodlabs-io/recorder/internal/browserfetch"
	"github.com/vodlabs-io/recorder/internal/streamer"
	"github.com/vodlabs-io/recorder/internal/vlstate"
)

// sniffRig is a scrape handler wired to an in-process NATS with a KV bucket
// shaped like the daemon's, and a fake desktop answering sniffs.
type sniffRig struct {
	h      *scrapeHandler
	kv     jetstream.KeyValue
	nc     *nats.Conn
	sniffs atomic.Int32
}

func newSniffRig(t *testing.T, desktop *browserfetch.SniffResponse) *sniffRig {
	t.Helper()
	srv, err := server.NewServer(&server.Options{Host: "127.0.0.1", Port: -1, JetStream: true,
		StoreDir: t.TempDir(), NoLog: true, NoSigs: true})
	if err != nil {
		t.Fatal(err)
	}
	go srv.Start()
	if !srv.ReadyForConnections(5 * time.Second) {
		t.Fatal("nats server not ready")
	}
	t.Cleanup(srv.Shutdown)
	nc, err := nats.Connect(srv.ClientURL())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(nc.Close)
	js, _ := jetstream.New(nc)
	kv, err := js.CreateKeyValue(context.Background(), jetstream.KeyValueConfig{
		Bucket: "streamers", History: 1, LimitMarkerTTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	r := &sniffRig{kv: kv, nc: nc}
	if desktop != nil {
		_, err := nc.Subscribe(browserfetch.SniffSubject, func(m *nats.Msg) {
			r.sniffs.Add(1)
			b, _ := json.Marshal(desktop)
			_ = m.Respond(b)
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	r.h = newScrapeHandler(quietLogger(t), nil, nil, nc, nil)
	if _, err := nc.Subscribe("scrape.requests", r.h.handleScrapeRequest); err != nil {
		t.Fatal(err)
	}
	r.h.sniffPermitted = func(pageURL string) bool {
		return vlstate.SniffPermitted(context.Background(), kv, pageURL)
	}
	return r
}

// sniff sends one sniff scrape through the handler and returns the reply.
func (r *sniffRig) sniff(t *testing.T, pageURL string, noCache bool) scrapeResponse {
	t.Helper()
	data, _ := json.Marshal(scrapeRequest{URL: pageURL, Method: "GET", Sniff: true, NoCache: noCache})
	got, err := r.nc.Request("scrape.requests", data, 10*time.Second)
	if err != nil {
		t.Fatalf("no reply: %v", err)
	}
	var resp scrapeResponse
	if err := json.Unmarshal(got.Data, &resp); err != nil {
		t.Fatal(err)
	}
	return resp
}

var playing = &browserfetch.SniffResponse{Title: "Live", Candidates: []browserfetch.SniffCandidate{
	{URL: "http://10.0.0.2/x.m3u8", Format: "hls"},
	{URL: "https://cdn.example.net/live/index.m3u8", Format: "hls", Frame: "https://player.example.net/embed"},
}}

func TestSniffRefusesAPageTheUserDidNotAskFor(t *testing.T) {
	r := newSniffRig(t, playing)
	resp := r.sniff(t, "https://example.com/live", false)
	if resp.Error == "" || r.sniffs.Load() != 0 {
		t.Fatalf("reply = %+v after %d sniffs, want a refusal before any page load", resp, r.sniffs.Load())
	}
}

func TestSniffRelaysTheReportForAPermittedPage(t *testing.T) {
	r := newSniffRig(t, playing)
	ctx := context.Background()
	// Permitted as typed, asked for in another spelling.
	if err := vlstate.PermitSniff(ctx, r.kv, "Example.com/live", streamer.StreamerKey{}, vlstate.SniffPermitPending); err != nil {
		t.Fatal(err)
	}
	resp := r.sniff(t, "https://example.com:443/live#top", false)
	if resp.Error != "" || resp.Headers["X-Scrape-Sniff"] != "true" {
		t.Fatalf("reply = %+v", resp)
	}
	var report browserfetch.SniffResponse
	if err := json.Unmarshal([]byte(resp.Body), &report); err != nil {
		t.Fatal(err)
	}
	if len(report.Candidates) != 1 || report.Candidates[0].Frame != "https://player.example.net/embed" || report.Title != "Live" {
		t.Fatalf("report = %+v, want the public candidate only", report)
	}

	// A second ask within the minute is answered from the cache; a Re-sign's
	// is not.
	r.sniff(t, "https://example.com/live", false)
	if n := r.sniffs.Load(); n != 1 {
		t.Fatalf("sniffs = %d, want the second answered from cache", n)
	}
	r.sniff(t, "https://example.com/live", true)
	if n := r.sniffs.Load(); n != 2 {
		t.Fatalf("sniffs = %d, want a fresh one for no_cache", n)
	}
}

func TestSniffPermitIsRevokedWithItsStreamer(t *testing.T) {
	r := newSniffRig(t, playing)
	ctx := context.Background()
	key := streamer.StreamerKey{Platform: "generic", Username: "example.com-0123456789"}
	if err := vlstate.PermitSniff(ctx, r.kv, "https://example.com/live", key, vlstate.SniffPermitRecording); err != nil {
		t.Fatal(err)
	}
	if !vlstate.SniffPermitted(ctx, r.kv, "https://example.com/live") {
		t.Fatal("the permit did not take")
	}
	vlstate.RevokeSniffs(ctx, r.kv, key)
	if vlstate.SniffPermitted(ctx, r.kv, "https://example.com/live") {
		t.Fatal("the permit outlived its streamer")
	}
}

func TestSniffWithoutADesktopIsUnavailable(t *testing.T) {
	r := newSniffRig(t, nil)
	if err := vlstate.PermitSniff(context.Background(), r.kv, "https://example.com/live", streamer.StreamerKey{}, time.Minute); err != nil {
		t.Fatal(err)
	}
	if resp := r.sniff(t, "https://example.com/live", false); !strings.Contains(resp.Error, "browser context unavailable") {
		t.Fatalf("reply = %+v, want the refusal the service reads as no browser", resp)
	}
}

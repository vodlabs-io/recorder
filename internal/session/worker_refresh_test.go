package session

import (
	"context"
	"testing"
	"time"

	"github.com/vodlabs-io/recorder/internal/stream"
	"github.com/vodlabs-io/recorder/internal/streamer"
)

func refreshableState(url string) *streamer.StreamerState {
	return &streamer.StreamerState{
		Status:  streamer.StatusLive,
		Streams: stream.List{{URL: url}},
	}
}

// TestRefreshStreamURLWaitsForTheReply pins that a notification left by an
// earlier update does not pass for the answer to a refresh, which would hand
// back the record the refresh was asked to replace.
func TestRefreshStreamURLWaitsForTheReply(t *testing.T) {
	reqs := make(chan streamer.StreamerRef, 1)
	w := &worker{notifyCh: make(chan struct{}, 1), streamReqCh: reqs}
	w.str.Store(refreshableState("https://cdn.example/old.m3u8"))
	w.notifyCh <- struct{}{}

	go func() {
		<-reqs
		time.Sleep(20 * time.Millisecond)
		w.str.Store(refreshableState("https://cdn.example/new.m3u8"))
		w.notifyCh <- struct{}{}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	got, err := w.refreshStreamURL(ctx)
	if err != nil {
		t.Fatalf("refreshStreamURL: %v", err)
	}
	if got != "https://cdn.example/new.m3u8" {
		t.Errorf("URL = %q, want the reply's", got)
	}
}

func TestRefreshStreamURLReportsAStreamerGoneOffline(t *testing.T) {
	reqs := make(chan streamer.StreamerRef, 1)
	w := &worker{notifyCh: make(chan struct{}, 1), streamReqCh: reqs}
	w.str.Store(refreshableState("https://cdn.example/old.m3u8"))

	go func() {
		<-reqs
		w.str.Store(&streamer.StreamerState{Status: streamer.StatusOffline})
		w.notifyCh <- struct{}{}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := w.refreshStreamURL(ctx); err == nil {
		t.Error("refreshed a streamer who went offline")
	}
}

func TestRefreshStreamURLStopsWithItsContext(t *testing.T) {
	w := &worker{notifyCh: make(chan struct{}, 1), streamReqCh: make(chan streamer.StreamerRef, 1)}
	w.str.Store(refreshableState("https://cdn.example/old.m3u8"))
	w.notifyCh <- struct{}{}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := w.refreshStreamURL(ctx); err == nil {
		t.Error("returned without an answer or a cancellation")
	}
}

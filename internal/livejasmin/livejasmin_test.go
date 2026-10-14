package livejasmin

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/vodlabs-io/recorder/internal/fmp4/fmp4test"
)

func TestURLRoundTrip(t *testing.T) {
	r := Room{Name: "AbbyMoore", Height: 720}
	got, err := Parse(r.URL())
	if err != nil || got != r {
		t.Fatalf("Parse(%q) = %+v, %v", r.URL(), got, err)
	}
	for _, bad := range []string{"https://www.livejasmin.com/en/chat/x", "livejasmin://", "livejasmin://a/b?height=x"} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("Parse(%q) accepted", bad)
		}
	}
}

// fakeSeat streams a fixed fMP4 and lets go when it has been read.
type fakeSeat struct {
	r    io.Reader
	done chan struct{}
	once sync.Once
}

func (f *fakeSeat) Stream() io.Reader     { return f.r }
func (f *fakeSeat) Done() <-chan struct{} { return f.done }
func (f *fakeSeat) Close()                { f.once.Do(func() { close(f.done) }) }

func TestSplicesSeatsOntoOneTimeline(t *testing.T) {
	streams := [][]byte{fmp4test.Stream(5000, 10), fmp4test.Stream(90000, 10)}
	var mu sync.Mutex
	opened := 0
	defer func(f func(context.Context, Room, *http.Client, string, *slog.Logger) (seatStream, error)) {
		openSeatFunc = f
	}(openSeatFunc)
	openSeatFunc = func(context.Context, Room, *http.Client, string, *slog.Logger) (seatStream, error) {
		mu.Lock()
		defer mu.Unlock()
		if opened == len(streams) {
			return nil, ErrEnded
		}
		s := &fakeSeat{r: bytes.NewReader(streams[opened]), done: make(chan struct{})}
		opened++
		return s, nil
	}

	stream, err := Open(context.Background(), Room{Name: "x"}.URL(), nil, "ua", slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	done := make(chan []byte)
	go func() {
		b, _ := io.ReadAll(stream)
		done <- b
	}()
	var out []byte
	select {
	case out = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("recording did not end when the room did")
	}

	var video []int64
	seqHeaders := 0
	for _, tag := range fmp4test.ReadFLV(t, out) {
		if tag.Seq {
			seqHeaders++
			continue
		}
		if tag.Type == fmp4test.FLVVideo {
			video = append(video, tag.TS)
		}
	}
	if len(video) != 20 {
		t.Fatalf("got %d video frames, want 20", len(video))
	}
	for i, ts := range video {
		if want := int64(i) * 33; ts != want {
			t.Fatalf("frame %d at %d ms, want %d (timeline %v)", i, ts, want, video)
		}
	}
	// The second seat's configuration is the first's: no new headers.
	if seqHeaders != 2 {
		t.Fatalf("got %d sequence headers, want 2", seqHeaders)
	}
}

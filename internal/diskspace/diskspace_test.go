package diskspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"
)

const gib = 1 << 30

// fakeSampler answers from a script instead of a filesystem, so "full",
// "unreachable" and "space appeared" are ordinary test inputs. A real ENOSPC is
// not reproducible in CI and the Windows sampler cannot run here at all.
type fakeSampler struct {
	mu sync.Mutex

	// usage is the answer per path. A path present in errs fails instead.
	usage map[string]Usage
	errs  map[string]error

	// queue, when non-empty for a path, is popped one answer per call — used to
	// script a root that gains space between polls.
	queue map[string][]Usage

	// delay makes Sample block, to exercise Probe's timeout.
	delay time.Duration

	calls map[string]int
}

func newFake() *fakeSampler {
	return &fakeSampler{
		usage: map[string]Usage{},
		errs:  map[string]error{},
		queue: map[string][]Usage{},
		calls: map[string]int{},
	}
}

func (f *fakeSampler) Sample(path string) (Usage, error) {
	if f.delay > 0 {
		time.Sleep(f.delay)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls[path]++
	if err, ok := f.errs[path]; ok {
		return Usage{}, err
	}
	if q := f.queue[path]; len(q) > 0 {
		u := q[0]
		if len(q) > 1 {
			f.queue[path] = q[1:]
		}
		return u, nil
	}
	u, ok := f.usage[path]
	if !ok {
		return Usage{}, errors.New("no such path")
	}
	return u, nil
}

func TestClassify(t *testing.T) {
	const warn = 20 * gib
	tests := []struct {
		name string
		free uint64
		want State
	}{
		{"plenty", 500 * gib, StateOK},
		{"just above warn", warn + 1, StateOK},
		// At a threshold is the good side of it, matching CanStart's floor.
		{"at warn", warn, StateOK},
		{"just below warn", warn - 1, StateLow},
		{"below warn", 10 * gib, StateLow},
		{"at start floor", StartFloorBytes, StateLow},
		{"below start floor", StartFloorBytes - 1, StateFull},
		{"empty", 0, StateFull},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Classify(Usage{FreeBytes: tt.free}, warn); got != tt.want {
				t.Errorf("Classify(free=%d) = %q, want %q", tt.free, got, tt.want)
			}
		})
	}
}

func TestClassifyZeroWarnDisablesLow(t *testing.T) {
	if got := Classify(Usage{FreeBytes: 10 * gib}, 0); got != StateOK {
		t.Errorf("with warn disabled, got %q, want %q", got, StateOK)
	}
	// The hard floor still applies — it is not the user's to switch off.
	if got := Classify(Usage{FreeBytes: 1 * gib}, 0); got != StateFull {
		t.Errorf("with warn disabled, got %q, want %q", got, StateFull)
	}
}

func TestGates(t *testing.T) {
	tests := []struct {
		name         string
		free         uint64
		wantCanStart bool
		wantMustStop bool
	}{
		{"healthy", 100 * gib, true, false},
		{"at start floor", StartFloorBytes, true, false},
		// Between the floors nothing happens: no start, no stop. That gap is the
		// hysteresis that stops Sessions churning when space hovers at the limit.
		{"in hysteresis gap", StartFloorBytes - 1, false, false},
		{"at stop floor", StopFloorBytes, false, false},
		{"below stop floor", StopFloorBytes - 1, false, true},
		{"empty", 0, false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u := Usage{FreeBytes: tt.free}
			if got := CanStart(u); got != tt.wantCanStart {
				t.Errorf("CanStart(free=%d) = %v, want %v", tt.free, got, tt.wantCanStart)
			}
			if got := MustStop(u); got != tt.wantMustStop {
				t.Errorf("MustStop(free=%d) = %v, want %v", tt.free, got, tt.wantMustStop)
			}
		})
	}
}

func TestFitsAndShortfall(t *testing.T) {
	tests := []struct {
		name          string
		free, want    uint64
		size          uint64
		wantFits      bool
		wantShortfall uint64
	}{
		{name: "room to spare", free: 100 * gib, size: 10 * gib, wantFits: true},
		{name: "exactly headroom left", free: 11 * gib, size: 10 * gib, wantFits: true},
		// A move that would leave the volume at zero is refused: a full volume
		// breaks things unrelated to the recorder.
		{name: "would zero the volume", free: 10 * gib, size: 10 * gib,
			wantFits: false, wantShortfall: MoveHeadroomBytes},
		{name: "nowhere near", free: 1 * gib, size: 10 * gib,
			wantFits: false, wantShortfall: 10*gib + MoveHeadroomBytes - 1*gib},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u := Usage{FreeBytes: tt.free}
			if got := Fits(u, tt.size); got != tt.wantFits {
				t.Errorf("Fits(free=%d, size=%d) = %v, want %v", tt.free, tt.size, got, tt.wantFits)
			}
			if got := Shortfall(u, tt.size); got != tt.wantShortfall {
				t.Errorf("Shortfall(free=%d, size=%d) = %d, want %d", tt.free, tt.size, got, tt.wantShortfall)
			}
		})
	}
}

func TestNormalize(t *testing.T) {
	tests := []struct {
		name string
		in   []string
		want []string
	}{
		{"empty in, empty out", nil, []string{}},
		{"drops blanks", []string{"", "   ", "\t"}, []string{}},
		{"drops dot", []string{"."}, []string{}},
		{"trims and cleans", []string{"  /a/b/  ", "/a//b"}, []string{"/a/b"}},
		{"dedupes", []string{"/a", "/b", "/a"}, []string{"/a", "/b"}},
		{"preserves order", []string{"/z", "/a", "/m"}, []string{"/z", "/a", "/m"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Normalize(tt.in)
			want := make([]string, len(tt.want))
			for i, w := range tt.want {
				want[i] = filepath.Clean(w)
			}
			if len(got) == 0 && len(want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("Normalize(%q) = %q, want %q", tt.in, got, want)
			}
		})
	}
}

func TestProbeReturnsSample(t *testing.T) {
	f := newFake()
	f.usage["/data"] = Usage{TotalBytes: 100, FreeBytes: 40, Device: "1"}

	got, err := Probe(context.Background(), f, "/data", time.Second)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if got.FreeBytes != 40 {
		t.Errorf("FreeBytes = %d, want 40", got.FreeBytes)
	}
}

func TestProbeTimesOutOnHungMount(t *testing.T) {
	f := newFake()
	f.usage["/nas"] = Usage{TotalBytes: 100, FreeBytes: 40, Device: "1"}
	f.delay = 500 * time.Millisecond

	start := time.Now()
	_, err := Probe(context.Background(), f, "/nas", 20*time.Millisecond)
	elapsed := time.Since(start)

	var timeout ErrTimeout
	if !errors.As(err, &timeout) {
		t.Fatalf("err = %v, want ErrTimeout", err)
	}
	// The point of the timeout is that the caller is released while the syscall
	// is still parked, not that the syscall is cancelled.
	if elapsed > 250*time.Millisecond {
		t.Errorf("Probe blocked for %s; it should return at its timeout", elapsed)
	}
}

func TestProbeHonoursCancelledContext(t *testing.T) {
	f := newFake()
	f.delay = 500 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := Probe(ctx, f, "/nas", time.Minute); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

func TestGroupCollapsesSharedFilesystem(t *testing.T) {
	f := newFake()
	// Download folder and archive folder on the same disk: one root, two paths.
	f.usage["/home/rec"] = Usage{TotalBytes: 500 * gib, FreeBytes: 100 * gib, Device: "same"}
	f.usage["/home/archive"] = Usage{TotalBytes: 500 * gib, FreeBytes: 100 * gib, Device: "same"}
	f.usage["/mnt/nas"] = Usage{TotalBytes: 8000 * gib, FreeBytes: 4000 * gib, Device: "nas"}

	roots := Prober{Sampler: f, WarnBytes: 20 * gib}.
		Group(context.Background(), "/home/rec", []string{"/home/archive", "/mnt/nas"})

	if len(roots) != 2 {
		t.Fatalf("got %d roots, want 2: %+v", len(roots), roots)
	}
	if want := []string{"/home/rec", "/home/archive"}; !reflect.DeepEqual(roots[0].Paths, want) {
		t.Errorf("roots[0].Paths = %q, want %q", roots[0].Paths, want)
	}
	if !roots[0].Download {
		t.Error("roots[0].Download = false; the download folder's root must be flagged")
	}
	if roots[1].Download {
		t.Error("roots[1].Download = true; only the download folder's root is flagged")
	}
	if roots[0].State != StateOK {
		t.Errorf("roots[0].State = %q, want %q", roots[0].State, StateOK)
	}
}

func TestGroupDedupesDuplicateConfiguredPaths(t *testing.T) {
	f := newFake()
	f.usage["/home/rec"] = Usage{FreeBytes: 100 * gib, Device: "a"}
	f.usage["/mnt/nas"] = Usage{FreeBytes: 50 * gib, Device: "b"}

	// Two Streamers pointing at one archive folder must not yield two roots.
	roots := Prober{Sampler: f}.
		Group(context.Background(), "/home/rec", []string{"/mnt/nas", "/mnt/nas/", "/mnt//nas"})

	if len(roots) != 2 {
		t.Fatalf("got %d roots, want 2: %+v", len(roots), roots)
	}
	if n := f.calls["/mnt/nas"]; n != 1 {
		t.Errorf("probed /mnt/nas %d times, want 1", n)
	}
}

func TestGroupReportsUnreachableRootAsUnknown(t *testing.T) {
	f := newFake()
	f.usage["/home/rec"] = Usage{FreeBytes: 100 * gib, Device: "a"}
	f.errs["/mnt/nas"] = errors.New("host is down")

	roots := Prober{Sampler: f}.Group(context.Background(), "/home/rec", []string{"/mnt/nas"})

	if len(roots) != 2 {
		t.Fatalf("got %d roots, want 2: %+v", len(roots), roots)
	}
	nas := roots[1]
	if nas.State != StateUnknown {
		t.Errorf("State = %q, want %q", nas.State, StateUnknown)
	}
	// Unknown must not read as "no space free" — they call for different
	// messages and different responses.
	if nas.Usage.FreeBytes != 0 || nas.State == StateFull {
		t.Errorf("unreachable root reported as full: %+v", nas)
	}
	if nas.Error == "" {
		t.Error("Error is empty; an unknown root must say why")
	}
}

func TestGroupKeepsUnreachableRootsSeparate(t *testing.T) {
	f := newFake()
	f.usage["/home/rec"] = Usage{FreeBytes: 100 * gib, Device: "a"}
	f.errs["/mnt/one"] = errors.New("host is down")
	f.errs["/mnt/two"] = errors.New("host is down")

	roots := Prober{Sampler: f}.
		Group(context.Background(), "/home/rec", []string{"/mnt/one", "/mnt/two"})

	// Two unreachable folders may or may not be one volume; merging them would
	// report a number the user cannot check.
	if len(roots) != 3 {
		t.Fatalf("got %d roots, want 3: %+v", len(roots), roots)
	}
}

func TestGroupSeesSpaceAppear(t *testing.T) {
	f := newFake()
	f.usage["/home/rec"] = Usage{FreeBytes: 100 * gib, Device: "a"}
	f.queue["/mnt/nas"] = []Usage{
		{FreeBytes: 1 * gib, Device: "nas"},   // full
		{FreeBytes: 1 * gib, Device: "nas"},   // still full
		{FreeBytes: 500 * gib, Device: "nas"}, // user freed space
	}
	p := Prober{Sampler: f, WarnBytes: 20 * gib}

	var states []State
	for i := 0; i < 3; i++ {
		roots := p.Group(context.Background(), "/home/rec", []string{"/mnt/nas"})
		states = append(states, roots[1].State)
	}

	want := []State{StateFull, StateFull, StateOK}
	if !reflect.DeepEqual(states, want) {
		t.Errorf("states = %v, want %v", states, want)
	}
}

func TestGroupWithNoDownloadFolder(t *testing.T) {
	f := newFake()
	f.usage["/mnt/nas"] = Usage{FreeBytes: 50 * gib, Device: "b"}

	roots := Prober{Sampler: f}.Group(context.Background(), "", []string{"/mnt/nas"})

	if len(roots) != 1 {
		t.Fatalf("got %d roots, want 1: %+v", len(roots), roots)
	}
	if roots[0].Download {
		t.Error("Download = true with no download folder configured")
	}
}

func TestReachable(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "vod.mp4")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		path    string
		wantErr bool
	}{
		{"existing folder", dir, false},
		{"missing folder", filepath.Join(dir, "nope"), true},
		// A path that resolves to a file is not a usable Storage root, and saying
		// so here keeps the scan from trying to walk it.
		{"a file, not a folder", file, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := Reachable(context.Background(), tt.path, time.Second)
			if (err != nil) != tt.wantErr {
				t.Errorf("Reachable(%q) = %v, wantErr %v", tt.path, err, tt.wantErr)
			}
		})
	}
}

func TestWithTimeoutAbandonsHungCall(t *testing.T) {
	released := make(chan struct{})
	start := time.Now()

	_, err := WithTimeout(context.Background(), "/nas", 20*time.Millisecond,
		func() (struct{}, error) {
			<-released // stands in for a stat parked on an unreachable mount
			return struct{}{}, nil
		})
	elapsed := time.Since(start)
	close(released)

	var timeout ErrTimeout
	if !errors.As(err, &timeout) {
		t.Fatalf("err = %v, want ErrTimeout", err)
	}
	if elapsed > 250*time.Millisecond {
		t.Errorf("caller blocked for %s; it should be released at the timeout", elapsed)
	}
	if timeout.Path != "/nas" {
		t.Errorf("ErrTimeout.Path = %q, want %q", timeout.Path, "/nas")
	}
}

func TestWithTimeoutPassesValueThrough(t *testing.T) {
	got, err := WithTimeout(context.Background(), "/x", time.Second,
		func() (int, error) { return 42, nil })
	if err != nil || got != 42 {
		t.Errorf("got (%d, %v), want (42, nil)", got, err)
	}
}

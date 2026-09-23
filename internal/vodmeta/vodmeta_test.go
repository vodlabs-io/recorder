package vodmeta

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/vodlabs-io/recorder/internal/streamer"
)

func at(min int) time.Time {
	return time.Date(2026, 9, 4, 12, min, 0, 0, time.UTC)
}

func TestLogSkipsEmptyAndRepeatReadings(t *testing.T) {
	dir := t.TempDir()
	l := NewLog(dir, "twitch", "streamer_four", "s1")

	// A platform that reports nothing leaves no sidecar at all, rather than one
	// holding an empty reading.
	if got, err := l.Observe(streamer.LiveMetadata{}, at(0)); got || err != nil {
		t.Fatalf("Observe(empty) = %v, %v; want false, nil", got, err)
	}
	if _, err := os.Stat(filepath.Join(dir, FileName)); !os.IsNotExist(err) {
		t.Fatalf("sidecar written for an empty reading: %v", err)
	}

	first := streamer.LiveMetadata{Title: "one", Category: "IRL", Viewers: 10}
	if got, err := l.Observe(first, at(0)); !got || err != nil {
		t.Fatalf("Observe(first) = %v, %v; want true, nil", got, err)
	}

	// The same broadcast re-reported, with only the viewer count moved: not a
	// new reading. This is the common case — most status updates say nothing new.
	repeat := first
	repeat.Viewers = 4321
	if got, err := l.Observe(repeat, at(5)); got || err != nil {
		t.Fatalf("Observe(repeat) = %v, %v; want false, nil", got, err)
	}

	retitled := first
	retitled.Title = "two"
	if got, err := l.Observe(retitled, at(10)); !got || err != nil {
		t.Fatalf("Observe(retitled) = %v, %v; want true, nil", got, err)
	}

	doc, err := Read(dir)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(doc.Observations) != 2 {
		t.Fatalf("Observations = %d, want 2", len(doc.Observations))
	}
	if doc.Platform != "twitch" || doc.Username != "streamer_four" || doc.SessionID != "s1" {
		t.Errorf("identity not stamped: %+v", doc)
	}
	if doc.Version != fileVersion {
		t.Errorf("Version = %d, want %d", doc.Version, fileVersion)
	}
}

// The sidecar is rewritten as the session runs, so a recording killed
// mid-stream still leaves every reading it had by then.
func TestLogWritesAsItGoes(t *testing.T) {
	dir := t.TempDir()
	l := NewLog(dir, "kick", "someone", "s2")
	if _, err := l.Observe(streamer.LiveMetadata{Title: "one"}, at(0)); err != nil {
		t.Fatalf("Observe: %v", err)
	}
	doc, err := Read(dir)
	if err != nil {
		t.Fatalf("Read after first observation: %v", err)
	}
	if len(doc.Observations) != 1 {
		t.Fatalf("Observations = %d, want 1", len(doc.Observations))
	}
}

func TestTitlesCollapsesRuns(t *testing.T) {
	doc := Document{Observations: []Observation{
		{At: at(0), LiveMetadata: streamer.LiveMetadata{Title: "one", Category: "IRL"}},
		// Only the category moved, so the title run continues.
		{At: at(5), LiveMetadata: streamer.LiveMetadata{Title: "one", Category: "Chatting"}},
		{At: at(10), LiveMetadata: streamer.LiveMetadata{Title: "two", Category: "Chatting"}},
		// A reading that dropped the title is not the streamer clearing it.
		{At: at(15), LiveMetadata: streamer.LiveMetadata{Category: "Chatting"}},
		// A title returning after another one is a new entry, not a repeat.
		{At: at(20), LiveMetadata: streamer.LiveMetadata{Title: "one", Category: "Chatting"}},
	}}

	got := doc.Titles()
	want := []TimedString{
		{Text: "one", At: at(0)},
		{Text: "two", At: at(10)},
		{Text: "one", At: at(20)},
	}
	if len(got) != len(want) {
		t.Fatalf("Titles() = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Titles()[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}

	cats := doc.Categories()
	if len(cats) != 2 || cats[0].Text != "IRL" || cats[1].Text != "Chatting" {
		t.Errorf("Categories() = %+v", cats)
	}
}

// The reading is inlined into the observation rather than nested, so a consumer
// reading the file sees "title" next to "at" and not under a wrapper.
func TestObservationJSONIsFlat(t *testing.T) {
	dir := t.TempDir()
	l := NewLog(dir, "twitch", "x", "s3")
	if _, err := l.Observe(streamer.LiveMetadata{Title: "one"}, at(0)); err != nil {
		t.Fatalf("Observe: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, FileName))
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	var raw struct {
		Observations []map[string]any `json:"observations"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(raw.Observations) != 1 {
		t.Fatalf("observations = %d, want 1", len(raw.Observations))
	}
	for _, key := range []string{"at", "title"} {
		if _, ok := raw.Observations[0][key]; !ok {
			t.Errorf("observation has no %q key: %v", key, raw.Observations[0])
		}
	}
}

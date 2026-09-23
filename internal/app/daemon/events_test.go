package daemon

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/vodlabs-io/recorder/internal/streamer"
	"github.com/vodlabs-io/recorder/internal/vlconfig"
	"github.com/vodlabs-io/recorder/internal/vllog"
	"github.com/vodlabs-io/recorder/internal/vlstate"
)

// fakeEntry is a minimal jetstream.KeyValueEntry exposing only Value(); the
// embedded interface panics on any other method, which resolveArchiveDir
// never calls.
type fakeEntry struct {
	jetstream.KeyValueEntry
	value []byte
}

func (e fakeEntry) Value() []byte { return e.value }

// oneShotKV is a minimal jetstream.KeyValue that serves the seeded one-shot
// .options entries from memory; any other key reports "not found".
type oneShotKV struct {
	jetstream.KeyValue
	data map[string][]byte
}

func (k *oneShotKV) Get(_ context.Context, key string) (jetstream.KeyValueEntry, error) {
	v, ok := k.data[key]
	if !ok {
		return nil, jetstream.ErrKeyNotFound
	}
	return fakeEntry{value: v}, nil
}

func seedOneShot(t *testing.T, key streamer.StreamerKey, opts vlstate.OneShotOptions) map[string][]byte {
	t.Helper()
	data, err := json.Marshal(opts)
	if err != nil {
		t.Fatalf("marshal one-shot opts: %v", err)
	}
	return map[string][]byte{key.KVKey() + ".options": data}
}

func loadStrFile(t *testing.T, content string) *vlconfig.StreamersFile {
	t.Helper()
	path := filepath.Join(t.TempDir(), "streamers.toml")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write streamers.toml: %v", err)
	}
	sf, err := vlconfig.NewStreamersFile(path)
	if err != nil {
		t.Fatalf("NewStreamersFile: %v", err)
	}
	return sf
}

func newTestApp(t *testing.T, globalSize, globalDur string, kvData map[string][]byte, sf *vlconfig.StreamersFile) *App {
	t.Helper()
	log, err := vllog.NewFileLogger(vlconfig.LoggerConfig{Level: "info", Mode: "off"}, "test")
	if err != nil {
		t.Fatalf("logger: %v", err)
	}
	a := &App{log: log, natsKV: &oneShotKV{data: kvData}, strFile: sf}
	a.appCfg.Session.SplitSize = globalSize
	a.appCfg.Session.SplitDuration = globalDur
	return a
}

func TestResolveArchiveDir(t *testing.T) {
	alice := streamer.StreamerKey{Platform: "tiktok", Username: "alice"}
	archive := filepath.Join(string(filepath.Separator)+"data", "rec")
	baseOut := filepath.Join(archive, "tiktok", "alice", "sess")
	nas := filepath.Join(string(filepath.Separator)+"mnt", "nas")
	other := filepath.Join(string(filepath.Separator)+"mnt", "other")

	tests := []struct {
		name     string
		globalSt string
		oneShot  *vlstate.OneShotOptions
		fileRule string
		wantDest string
		wantRel  string // checked only when wantDest != ""
	}{
		{
			name:     "global only",
			globalSt: nas,
			wantDest: nas,
			wantRel:  filepath.Join("tiktok", "alice", "sess"),
		},
		{
			name:     "one-shot overrides global",
			globalSt: nas,
			oneShot:  &vlstate.OneShotOptions{ArchiveDir: other},
			wantDest: other,
			wantRel:  filepath.Join("tiktok", "alice", "sess"),
		},
		{
			name:     "one-shot off disables global",
			globalSt: nas,
			oneShot:  &vlstate.OneShotOptions{ArchiveDir: "off"},
			wantDest: "",
		},
		{
			name:     "file rule beats one-shot and global",
			globalSt: nas,
			oneShot:  &vlstate.OneShotOptions{ArchiveDir: other},
			fileRule: "[[rules]]\ntiktok = [\"alice\"]\narchive_dir = \"" + filepath.ToSlash(filepath.Join(string(filepath.Separator)+"mnt", "rule")) + "\"\n",
			wantDest: filepath.Join(string(filepath.Separator)+"mnt", "rule"),
			wantRel:  filepath.Join("tiktok", "alice", "sess"),
		},
		{
			name:     "file rule off disables global",
			globalSt: nas,
			fileRule: "[[rules]]\ntiktok = [\"alice\"]\narchive_dir = \"off\"\n",
			wantDest: "",
		},
		{
			name:     "global empty disables",
			globalSt: "",
			wantDest: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			kvData := map[string][]byte{}
			if tc.oneShot != nil {
				kvData = seedOneShot(t, alice, *tc.oneShot)
			}
			var sf *vlconfig.StreamersFile
			if tc.fileRule != "" {
				sf = loadStrFile(t, tc.fileRule)
			}
			a := newTestApp(t, "", "", kvData, sf)
			a.appCfg.Processing.ArchiveDir = tc.globalSt
			a.appCfg.Session.DownloadDir = archive

			gotDest, gotRel := a.resolveArchiveDir(context.Background(), alice, baseOut)
			if gotDest != tc.wantDest {
				t.Errorf("dest = %q, want %q", gotDest, tc.wantDest)
			}
			if tc.wantDest != "" && gotRel != tc.wantRel {
				t.Errorf("relDir = %q, want %q", gotRel, tc.wantRel)
			}
		})
	}
}

func TestResolveArchiveDirFallbackRelDir(t *testing.T) {
	alice := streamer.StreamerKey{Platform: "tiktok", Username: "alice"}
	nas := filepath.Join(string(filepath.Separator)+"mnt", "nas")
	// Session dir is outside the download folder (e.g. download_dir changed since
	// recording): resolveArchiveDir falls back to platform/username/<basename>.
	baseOut := filepath.Join(string(filepath.Separator)+"elsewhere", "tiktok", "alice", "sess123")

	a := newTestApp(t, "", "", map[string][]byte{}, nil)
	a.appCfg.Processing.ArchiveDir = nas
	a.appCfg.Session.DownloadDir = filepath.Join(string(filepath.Separator)+"data", "rec")

	gotDest, gotRel := a.resolveArchiveDir(context.Background(), alice, baseOut)
	if gotDest != nas {
		t.Fatalf("dest = %q, want %q", gotDest, nas)
	}
	want := filepath.Join("tiktok", "alice", "sess123")
	if gotRel != want {
		t.Errorf("relDir = %q, want %q", gotRel, want)
	}
}

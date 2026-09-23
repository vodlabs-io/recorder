package daemon

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/knadh/koanf/parsers/toml"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/v2"
	"github.com/vodlabs-io/recorder/internal/vlapi"
)

// readBackScanDirs persists cfg and reads processing.scan_dirs back out of the
// file, the way a daemon restart would.
func readBackScanDirs(t *testing.T, cfg vlapi.UserConfig) []string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("[processing]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeUserConfig(path, cfg); err != nil {
		t.Fatalf("writeUserConfig: %v", err)
	}
	k := koanf.New(".")
	if err := k.Load(file.Provider(path), toml.Parser()); err != nil {
		t.Fatalf("reload: %v", err)
	}
	return k.Strings("processing.scan_dirs")
}

func TestScanDirsSurviveAConfigWrite(t *testing.T) {
	dirs := []string{filepath.Clean("/mnt/old/vods"), filepath.Clean("/mnt/nas2/vods")}
	got := readBackScanDirs(t, vlapi.UserConfig{ScanDirs: dirs})
	if len(got) != len(dirs) {
		t.Fatalf("scan_dirs = %v, want %v", got, dirs)
	}
	for i := range dirs {
		if got[i] != dirs[i] {
			t.Errorf("scan_dirs[%d] = %q, want %q", i, got[i], dirs[i])
		}
	}
}

// Removing the last scan folder must actually clear the key. koanf keeps a
// file's existing value for a key that is never Set, so an empty list written
// as "nothing" would silently restore the folder the user just removed.
func TestClearingTheLastScanDirIsPersisted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path,
		[]byte("[processing]\nscan_dirs = [\"/mnt/old/vods\"]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeUserConfig(path, vlapi.UserConfig{ScanDirs: nil}); err != nil {
		t.Fatalf("writeUserConfig: %v", err)
	}
	k := koanf.New(".")
	if err := k.Load(file.Provider(path), toml.Parser()); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got := k.Strings("processing.scan_dirs"); len(got) != 0 {
		t.Errorf("scan_dirs = %v after clearing, want empty", got)
	}
}

// The UI decides a VOD is read-only by matching its scan root against
// config.get's scan_dirs as plain strings, so a hand-edited config.toml must
// not be able to make those two spellings differ.
func TestGetConfigNormalizesScanDirs(t *testing.T) {
	raw := filepath.Clean("/mnt/old/vods")
	a := &App{}
	a.appCfg.Processing.ScanDirs = []string{raw + string(filepath.Separator), "  " + raw + "  "}

	got := a.GetConfig().ScanDirs
	if len(got) != 1 || got[0] != raw {
		t.Errorf("GetConfig().ScanDirs = %v, want [%q]", got, raw)
	}
	if !a.isReadOnlyVODRoot(got[0]) {
		t.Errorf("isReadOnlyVODRoot(%q) = false; the API and the guard disagree", got[0])
	}
}

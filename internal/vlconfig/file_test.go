package vlconfig

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDefaultPathsHonourOverrides checks the overrides work on every platform:
// os.UserCacheDir and os.UserConfigDir ignore the XDG variables on macOS.
func TestDefaultPathsHonourOverrides(t *testing.T) {
	cache, config := t.TempDir(), t.TempDir()
	t.Setenv(CacheDirEnv, cache)
	t.Setenv(configDirEnv, config)

	if got, want := DefaultCacheFilePath("platforms.json"), filepath.Join(cache, "platforms.json"); got != want {
		t.Errorf("DefaultCacheFilePath = %q, want %q", got, want)
	}
	if got, want := DefaultFilePath("config.toml"), filepath.Join(config, "config.toml"); got != want {
		t.Errorf("DefaultFilePath = %q, want %q", got, want)
	}
}

// TestDefaultPathsFallBackToUserDirs checks an unset override still resolves to
// the per-user location, so the overrides stay a redirection and never become a
// requirement.
func TestDefaultPathsFallBackToUserDirs(t *testing.T) {
	t.Setenv(CacheDirEnv, "")
	t.Setenv(configDirEnv, "  ") // blank counts as unset, not as "write to CWD"

	for _, tc := range []struct {
		name string
		got  string
		base func() (string, error)
	}{
		{"cache", DefaultCacheFilePath("platforms.json"), os.UserCacheDir},
		{"config", DefaultFilePath("config.toml"), os.UserConfigDir},
	} {
		base, err := tc.base()
		if err != nil || base == "" {
			continue // no per-user dir on this machine; the CWD fallback applies
		}
		want := filepath.Join(base, "vodlabs", "recorder")
		if !strings.HasPrefix(tc.got, want+string(filepath.Separator)) {
			t.Errorf("%s path = %q, want it under %q", tc.name, tc.got, want)
		}
	}
}

// TestPlatformsCacheWritesUnderOverride exercises the whole chain: the
// heartbeat's cache write must land in the override directory, not in the real
// user cache.
func TestPlatformsCacheWritesUnderOverride(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(CacheDirEnv, dir)

	if err := SavePlatformsCache([]Platform{{Name: "tiktok"}, {Name: "kick"}}); err != nil {
		t.Fatalf("SavePlatformsCache: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "platforms.json")); err != nil {
		t.Fatalf("platforms.json not written under the override: %v", err)
	}

	got, err := LoadPlatformsCache()
	if err != nil {
		t.Fatalf("LoadPlatformsCache: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("read back %d platforms, want 2 — reads and writes disagree on the path", len(got))
	}
}

// TestEnsurePlatformsCacheExistsUnderOverride covers the startup seed, the other
// writer of the real cache path.
func TestEnsurePlatformsCacheExistsUnderOverride(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(CacheDirEnv, dir)

	if err := EnsurePlatformsCacheExists(); err != nil {
		t.Fatalf("EnsurePlatformsCacheExists: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "platforms.json")); err != nil {
		t.Fatalf("seed not written under the override: %v", err)
	}
}

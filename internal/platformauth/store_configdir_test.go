package platformauth

import (
	"os"
	"path/filepath"
	"testing"
)

// A daemon on a redirected config dir must neither read, write nor adopt the
// real user's logins.
func TestNewStore_HonoursConfigDir(t *testing.T) {
	home := t.TempDir()
	cfg := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("VLREC_CONFIG_DIR", cfg)

	// A legacy jar that a redirected profile must not adopt.
	legacy := filepath.Join(home, legacyCookieFileName)
	if err := os.WriteFile(legacy, []byte(`{"version":1,"data":"x"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	s := NewStore()
	if want := filepath.Join(cfg, cookieFileName); s.filePath != want {
		t.Errorf("filePath = %q, want %q", s.filePath, want)
	}
	if _, err := os.Stat(legacy); err != nil {
		t.Errorf("redirected profile moved the real user's jar: %v", err)
	}
	if _, err := os.Stat(s.filePath); err == nil {
		t.Errorf("redirected profile inherited a jar at %s", s.filePath)
	}
}

// The default profile keeps its stored logins across the move.
func TestNewStore_MigratesLegacyJar(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("VLREC_CONFIG_DIR", "")

	legacy := filepath.Join(home, legacyCookieFileName)
	body := []byte(`{"version":1,"data":"payload"}`)
	if err := os.WriteFile(legacy, body, 0o600); err != nil {
		t.Fatal(err)
	}

	s := NewStore()
	got, err := os.ReadFile(s.filePath)
	if err != nil {
		t.Fatalf("jar not migrated to %s: %v", s.filePath, err)
	}
	if string(got) != string(body) {
		t.Errorf("migrated jar = %q, want %q", got, body)
	}
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Errorf("legacy jar still present after migration")
	}
}

package daemon

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/vodlabs-io/recorder/internal/webui"
)

func TestOpenVODFile(t *testing.T) {
	root := t.TempDir()
	dir := newSession(t, root, "kick/someone/2026-08-30_12-00-00-0700")
	writeManifest(t, dir)
	secret := filepath.Join(root, "secret.txt")
	if err := os.WriteFile(secret, []byte("nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	a := newVODTestApp(t, root)
	ctx := context.Background()

	if _, err := a.OpenVODFile(ctx, dir, "vod.mp4"); !errors.Is(err, webui.ErrNotReady) {
		t.Fatalf("before the index is up: %v, want ErrNotReady", err)
	}
	a.vodReady.Store(true)
	waitIndexed(t, a)

	f, err := a.OpenVODFile(ctx, dir, "vod.mp4")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(f)
	f.Close()
	if string(b) != "payload" {
		t.Errorf("read %q", b)
	}

	// The manifest lists vod.mp4 only; swap it for a symlink out of the
	// session, as a hostile archive might.
	if err := os.Remove(filepath.Join(dir, "vod.mp4")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(dir, "vod.mp4")); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ dir, file string }{
		{dir, "vod.mp4"},                // symlink out of the session folder
		{dir, "vod.json"},               // present but not a listed VOD file
		{dir, "../../../../secret.txt"}, // traversal
		{dir, ""},
		{"/etc", "passwd"}, // not a listed session
	} {
		if f, err := a.OpenVODFile(ctx, tc.dir, tc.file); !errors.Is(err, webui.ErrNotFound) {
			if f != nil {
				f.Close()
			}
			t.Errorf("OpenVODFile(%q, %q) = %v, want ErrNotFound", tc.dir, tc.file, err)
		}
	}
}

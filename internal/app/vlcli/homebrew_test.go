package vlcli

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestInstalledByHomebrew(t *testing.T) {
	for path, want := range map[string]bool{
		"/opt/homebrew/Cellar/vod/0.0.29/bin/vod":              true,
		"/usr/local/Cellar/vod/0.0.29/bin/vod":                 true,
		"/home/linuxbrew/.linuxbrew/Cellar/vod/0.0.29/bin/vod": true,
		"/Users/ana/.local/bin/vod":                            false,
		"/opt/homebrew/Cellar/vodka/1.0/bin/vod":               false,
		`C:\Users\ana\AppData\Local\vodlabs\bin\vod.exe`:       false,
	} {
		if got := installedByHomebrew(path); got != want {
			t.Errorf("installedByHomebrew(%q) = %v, want %v", path, got, want)
		}
	}
}

func TestInstalledByHomebrewFollowsTheBinLink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	root := t.TempDir()
	cellar := filepath.Join(root, "Cellar", "vod", "0.0.29", "bin")
	if err := os.MkdirAll(cellar, 0o755); err != nil {
		t.Fatal(err)
	}
	real := filepath.Join(cellar, "vod")
	if err := os.WriteFile(real, nil, 0o755); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(root, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(bin, "vod")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if !installedByHomebrew(link) {
		t.Errorf("installedByHomebrew(%q) = false through a link to the Cellar", link)
	}
}

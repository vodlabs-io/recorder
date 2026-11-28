package vlcli

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestManagedBy(t *testing.T) {
	for path, want := range map[string]string{
		"/opt/homebrew/Cellar/vod/0.0.29/bin/vod":              "brew upgrade vod",
		"/usr/local/Cellar/vod/0.0.29/bin/vod":                 "brew upgrade vod",
		"/home/linuxbrew/.linuxbrew/Cellar/vod/0.0.29/bin/vod": "brew upgrade vod",
		`C:\Users\ana\scoop\apps\vod\0.0.30\vod.exe`:           "scoop update vod",
		`D:\Scoop\apps\vod\current\vod.exe`:                    "scoop update vod",
		`C:\Users\ana\AppData\Local\Microsoft\WinGet\Packages\VodLabs.vod_Microsoft.Winget.Source_8wekyb3d8bbwe\vod.exe`: "winget upgrade VodLabs.vod",
		`C:\ProgramData\chocolatey\lib\vod\tools\vod.exe`:                                                                "choco upgrade vod",
		"/usr/bin/vod":                                            "your system's package manager",
		"/Users/ana/.local/bin/vod":                               "",
		"/opt/homebrew/Cellar/vodka/1.0/bin/vod":                  "",
		`C:\Users\ana\scoop\apps\vodka\1.0\vod.exe`:               "",
		`C:\Users\ana\AppData\Local\Programs\VodLabs\bin\vod.exe`: "",
		"/usr/local/bin/vod":                                      "",
	} {
		if _, got := managedBy(path); got != want {
			t.Errorf("managedBy(%q) update = %q, want %q", path, got, want)
		}
	}
}

func TestManagedByFollowsTheHomebrewLink(t *testing.T) {
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
	if name, _ := managedBy(link); name != "Homebrew" {
		t.Errorf("managedBy(%q) = %q through a link to the Cellar, want Homebrew", link, name)
	}
}

func TestDirWritable(t *testing.T) {
	dir := t.TempDir()
	if !dirWritable(dir) {
		t.Errorf("dirWritable(%q) = false for a fresh temp dir", dir)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("dirWritable left %d files behind", len(entries))
	}
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		return
	}
	locked := filepath.Join(dir, "locked")
	if err := os.Mkdir(locked, 0o555); err != nil {
		t.Fatal(err)
	}
	if dirWritable(locked) {
		t.Errorf("dirWritable(%q) = true for a read-only dir", locked)
	}
}

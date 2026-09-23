package vlconfig

import (
	"path/filepath"
	"testing"
)

func TestNormalizeScanDirs(t *testing.T) {
	got := NormalizeScanDirs([]string{
		"  /a/b  ", "/a/b/", "", "   ", "/a/b/../b", "/c/d",
	})
	want := []string{filepath.Clean("/a/b"), filepath.Clean("/c/d")}
	if len(got) != len(want) {
		t.Fatalf("NormalizeScanDirs = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("NormalizeScanDirs[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestValidateScanDirs(t *testing.T) {
	download := filepath.Clean("/data/downloads")
	archive := filepath.Clean("/mnt/nas/vods")

	tests := []struct {
		name    string
		dirs    []string
		wantErr bool
	}{
		{"empty", nil, false},
		{"unrelated", []string{filepath.Clean("/mnt/old/vods")}, false},
		{"is the download folder", []string{download}, true},
		{"inside the download folder", []string{filepath.Join(download, "old")}, true},
		{"contains the download folder", []string{filepath.Clean("/data")}, true},
		{"is the archive folder", []string{archive}, true},
		{"inside the archive folder", []string{filepath.Join(archive, "2019")}, true},
		{"contains the archive folder", []string{filepath.Clean("/mnt/nas")}, true},
		{"relative", []string{"old/vods"}, true},
		{"scheme", []string{"s3://bucket/vods"}, true},
		// A sibling sharing a name prefix is not nested and must be accepted.
		{"prefix sibling", []string{download + "-2019"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateScanDirs(NormalizeScanDirs(tt.dirs), download, []string{archive})
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateScanDirs(%v) err = %v, wantErr = %v", tt.dirs, err, tt.wantErr)
			}
		})
	}
}

// An empty or "off" archive folder is not a destination to check against.
func TestValidateScanDirsIgnoresDisabledArchive(t *testing.T) {
	dirs := NormalizeScanDirs([]string{filepath.Clean("/mnt/old/vods")})
	for _, archive := range []string{"", "off", "  "} {
		if err := ValidateScanDirs(dirs, filepath.Clean("/data/downloads"), []string{archive}); err != nil {
			t.Errorf("ValidateScanDirs with archive %q = %v, want nil", archive, err)
		}
	}
}

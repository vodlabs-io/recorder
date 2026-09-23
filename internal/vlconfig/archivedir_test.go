package vlconfig

import (
	"os"
	"path/filepath"
	"testing"
)

func TestIsArchiveOff(t *testing.T) {
	for _, raw := range []string{"off", "OFF", " Off "} {
		if !IsArchiveOff(raw) {
			t.Errorf("IsArchiveOff(%q) = false, want true", raw)
		}
	}
	for _, raw := range []string{"", "/mnt/nas", "0"} {
		if IsArchiveOff(raw) {
			t.Errorf("IsArchiveOff(%q) = true, want false", raw)
		}
	}
}

func TestValidateArchiveDir(t *testing.T) {
	abs := string(filepath.Separator) + filepath.Join("mnt", "nas")
	tests := []struct {
		raw     string
		wantErr bool
	}{
		{"", false},
		{"off", false},
		{abs, false},
		{"relative/path", true},
		{"s3://bucket/prefix", true},
		{"https://example.com/x", true},
	}
	for _, tt := range tests {
		err := ValidateArchiveDir(tt.raw)
		if (err != nil) != tt.wantErr {
			t.Errorf("ValidateArchiveDir(%q) err = %v, wantErr = %v", tt.raw, err, tt.wantErr)
		}
	}
}

func TestArchiveDirWritable(t *testing.T) {
	tmp := t.TempDir()

	// Existing writable dir, and a not-yet-created child of it, both pass: the
	// move worker creates the leaf on first use, so a writable ancestor is enough.
	for _, dest := range []string{tmp, filepath.Join(tmp, "moved", "deeper")} {
		if err := ArchiveDirWritable(dest); err != nil {
			t.Errorf("ArchiveDirWritable(%q) = %v, want nil", dest, err)
		}
	}

	// A path whose nearest existing ancestor is not writable fails — the
	// containerized /Volumes/... case, where the only existing ancestor (/) is
	// not writable by the unprivileged daemon.
	ro := filepath.Join(tmp, "ro")
	if err := os.Mkdir(ro, 0o555); err != nil {
		t.Fatalf("mkdir ro: %v", err)
	}
	if os.Geteuid() != 0 { // root ignores mode bits, so this check is meaningless as root
		if err := ArchiveDirWritable(filepath.Join(ro, "child")); err == nil {
			t.Errorf("ArchiveDirWritable on read-only ancestor = nil, want error")
		}
	}

	// A path that exists but is a file, not a directory, fails.
	file := filepath.Join(tmp, "afile")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if err := ArchiveDirWritable(file); err == nil {
		t.Errorf("ArchiveDirWritable on a file = nil, want error")
	}
}

func TestValidateArchiveDirAgainstDownloadDir(t *testing.T) {
	downloadDir := filepath.Join(string(filepath.Separator)+"data", "recordings")
	tests := []struct {
		name    string
		dest    string
		wantErr bool
	}{
		{"separate", filepath.Join(string(filepath.Separator)+"mnt", "nas"), false},
		{"equal", downloadDir, true},
		{"archive inside download", filepath.Join(downloadDir, "archive"), true},
		{"download inside archive", string(filepath.Separator) + "data", true},
		{"empty disables", "", false},
		{"off disables", "off", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateArchiveDirAgainstDownloadDir(tt.dest, downloadDir)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateArchiveDirAgainstDownloadDir(%q, %q) err = %v, wantErr = %v",
					tt.dest, downloadDir, err, tt.wantErr)
			}
		})
	}
}

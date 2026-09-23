package processing

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestEndPlaylist covers the parts a hard kill leaves behind, which the writer's
// own finalize never gets to run on.
func TestEndPlaylist(t *testing.T) {
	const head = "#EXTM3U\n#EXT-X-VERSION:3\n#EXTINF:2.000,\n000000.ts\n"

	cases := []struct {
		name    string
		content string
		want    string
	}{
		{"unterminated", head, head + "#EXT-X-ENDLIST\n"},
		{"no trailing newline", strings.TrimSuffix(head, "\n"), head + "#EXT-X-ENDLIST\n"},
		{"already terminated", head + "#EXT-X-ENDLIST\n", head + "#EXT-X-ENDLIST\n"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "index.m3u8")
			if err := os.WriteFile(path, []byte(tc.content), 0o644); err != nil {
				t.Fatalf("write playlist: %v", err)
			}
			if err := endPlaylist(path); err != nil {
				t.Fatalf("endPlaylist: %v", err)
			}
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read playlist: %v", err)
			}
			if string(got) != tc.want {
				t.Errorf("playlist = %q, want %q", got, tc.want)
			}
		})
	}
}

package writer

import (
	"os"
	"path/filepath"
	"testing"
)

// Each writer answers for its own Part layout. These pin those answers, so a
// writer that changes shape cannot do it silently.
func TestPreparePartLayout(t *testing.T) {
	cases := []struct {
		writer       string
		wantFileName string
		wantRetries  int
		// pathIsDir says the Part is identified by a directory rather than a
		// file, which is also what decides how it is discarded.
		pathIsDir bool
	}{
		{writer: "hlswriter", wantFileName: "index.m3u8", wantRetries: 3, pathIsDir: true},
		{writer: "flvwriter", wantFileName: "pt1_720p.ts", wantRetries: 2, pathIsDir: false},
		{writer: "webmwriter", wantFileName: "pt1_720p.webm", wantRetries: 2, pathIsDir: false},
	}

	for _, tc := range cases {
		t.Run(tc.writer, func(t *testing.T) {
			w, ok := Get(tc.writer)
			if !ok {
				t.Fatalf("writer %q not registered", tc.writer)
			}
			base := t.TempDir()
			part, err := w.PreparePart(base, "pt1_720p")
			if err != nil {
				t.Fatalf("PreparePart: %v", err)
			}
			if part.FileName != tc.wantFileName {
				t.Errorf("FileName = %q, want %q", part.FileName, tc.wantFileName)
			}
			if part.Retries != tc.wantRetries {
				t.Errorf("Retries = %d, want %d", part.Retries, tc.wantRetries)
			}

			// The directory the writer will open its file in must exist by the
			// time PreparePart returns; a writer that had to create it itself
			// would fail on its first write.
			if fi, err := os.Stat(part.Dir); err != nil || !fi.IsDir() {
				t.Fatalf("Dir %q is not a directory after PreparePart (err=%v)", part.Dir, err)
			}

			wantPath := filepath.Join(part.Dir, part.FileName)
			if tc.pathIsDir {
				wantPath = part.Dir
				if fi, err := os.Stat(part.Path); err != nil || !fi.IsDir() {
					t.Errorf("Path %q should be the part's own directory (err=%v)", part.Path, err)
				}
				if part.Dir == base {
					t.Errorf("Dir %q must be a subdirectory of %q, or parts overwrite each other",
						part.Dir, base)
				}
			} else if part.Dir != base {
				t.Errorf("Dir = %q, want the session directory %q", part.Dir, base)
			}
			if part.Path != wantPath {
				t.Errorf("Path = %q, want %q", part.Path, wantPath)
			}
		})
	}
}

// A Part that recorded nothing is removed whole, whatever shape it has.
func TestDiscardPartRemovesTheWholePart(t *testing.T) {
	for _, name := range []string{"hlswriter", "flvwriter"} {
		t.Run(name, func(t *testing.T) {
			w, _ := Get(name)
			base := t.TempDir()
			part, err := w.PreparePart(base, "pt1_720p")
			if err != nil {
				t.Fatalf("PreparePart: %v", err)
			}
			// Whatever the writer would have written.
			if err := os.WriteFile(filepath.Join(part.Dir, part.FileName), []byte("x"), 0o644); err != nil {
				t.Fatalf("seed part: %v", err)
			}

			if err := w.DiscardPart(part); err != nil {
				t.Fatalf("DiscardPart: %v", err)
			}
			if _, err := os.Stat(part.Path); !os.IsNotExist(err) {
				t.Errorf("Path %q survived DiscardPart (err=%v)", part.Path, err)
			}
			// The session directory is not the writer's to remove: later parts
			// and session.log live there.
			if fi, err := os.Stat(base); err != nil || !fi.IsDir() {
				t.Errorf("DiscardPart removed the session directory %q (err=%v)", base, err)
			}
		})
	}
}

// Discarding a Part the writer never got to write must not fail the session:
// the run loop calls it on any empty part, including one that failed instantly.
func TestDiscardPartIsSafeOnAnUnwrittenPart(t *testing.T) {
	for _, name := range []string{"hlswriter", "flvwriter"} {
		t.Run(name, func(t *testing.T) {
			w, _ := Get(name)
			base := t.TempDir()
			part, err := w.PreparePart(base, "pt1_720p")
			if err != nil {
				t.Fatalf("PreparePart: %v", err)
			}
			if err := w.DiscardPart(part); err != nil {
				t.Fatalf("DiscardPart on an unwritten part: %v", err)
			}
			// And again, now that it is gone.
			if err := w.DiscardPart(part); err != nil {
				t.Errorf("DiscardPart on an already-removed part: %v", err)
			}
		})
	}
}

// Successive parts of one session must not collide.
func TestPartsOfOneSessionAreDistinct(t *testing.T) {
	for _, name := range []string{"hlswriter", "flvwriter"} {
		t.Run(name, func(t *testing.T) {
			w, _ := Get(name)
			base := t.TempDir()
			first, err := w.PreparePart(base, "pt1_720p")
			if err != nil {
				t.Fatalf("PreparePart: %v", err)
			}
			second, err := w.PreparePart(base, "pt2_720p")
			if err != nil {
				t.Fatalf("PreparePart: %v", err)
			}
			if first.Path == second.Path {
				t.Errorf("both parts claim %q", first.Path)
			}
		})
	}
}

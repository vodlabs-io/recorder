package flvfix

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCorpus sanitizes every raw FLV capture (*.ts) under the directory named
// by FLVFIX_CORPUS and re-parses the outputs against the sanitizer's
// invariants. Set FLVFIX_CORPUS_OUT to keep the sanitized files (for ffmpeg
// conversion and playback checks); otherwise a temp dir is used.
func TestCorpus(t *testing.T) {
	corpus := os.Getenv("FLVFIX_CORPUS")
	if corpus == "" {
		t.Skip("FLVFIX_CORPUS not set")
	}
	outRoot := os.Getenv("FLVFIX_CORPUS_OUT")
	if outRoot == "" {
		outRoot = t.TempDir()
	}

	var inputs []string
	err := filepath.WalkDir(corpus, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(path, ".ts") {
			inputs = append(inputs, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(inputs) == 0 {
		t.Fatalf("no .ts files under %s", corpus)
	}

	for _, in := range inputs {
		rel, _ := filepath.Rel(corpus, in)
		t.Run(rel, func(t *testing.T) {
			stem := filepath.Join(outRoot,
				strings.TrimSuffix(strings.ReplaceAll(rel, string(filepath.Separator), "_"), ".ts")+"_fixed")
			outputs, stats, err := RepairFile(in, stem, nil)
			if err != nil {
				t.Fatalf("repair: %v", err)
			}
			t.Logf("stats: %+v", stats)
			t.Logf("outputs: %d", len(outputs))
			for _, out := range outputs {
				data, err := os.ReadFile(out)
				if err != nil {
					t.Fatal(err)
				}
				if len(data) == 0 {
					continue
				}
				tags := parseOutput(t, data)
				checkInvariants(t, tags)
			}
		})
	}
}

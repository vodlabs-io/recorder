package recording

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/vodlabs-io/recorder/internal/provenance"
	"github.com/vodlabs-io/recorder/internal/version"
)

// ManifestName is the sidecar file written into a VOD's folder when processing
// finishes. It travels with the folder when the VOD is moved, so the VOD
// Archive scanner can recover full session metadata wherever it ends up. A
// folder without one is indexed via a best-effort filesystem fallback.
const ManifestName = "vod.json"

// manifestVersion is bumped when the manifest schema changes incompatibly.
const manifestVersion = 1

// ManifestFile describes one produced output file (vod.mp4 or vodN.mp4).
type ManifestFile struct {
	Name        string    `json:"name"`
	Bytes       int64     `json:"bytes,omitempty"`
	Start       time.Time `json:"start,omitzero"`
	End         time.Time `json:"end,omitzero"`
	DurationSec float64   `json:"duration_sec,omitempty"`
}

// Manifest is the JSON document stored as vod.json next to a session's output
// files. It is a wire contract: the vodindex scanner and (indirectly) GUI
// clients read it, so fields are only ever added, never repurposed.
type Manifest struct {
	Version         int    `json:"version"`
	Producer        string `json:"producer,omitempty"`
	ProducerVersion string `json:"producer_version,omitempty"`
	ProducerCommit  string `json:"producer_commit,omitempty"`
	ProvenanceSpec  int    `json:"provenance_spec,omitempty"`

	Platform    string         `json:"platform"`
	Username    string         `json:"username"`
	SessionID   string         `json:"session_id"`
	Writer      string         `json:"writer"`
	Start       time.Time      `json:"start,omitzero"`
	End         time.Time      `json:"end,omitzero"`
	TotalBytes  int64          `json:"total_bytes"`
	DurationSec float64        `json:"duration_sec"`
	Files       []ManifestFile `json:"files"`
}

// WriteManifest atomically writes m as dir/vod.json.
func WriteManifest(dir string, m Manifest) error {
	m.Version = manifestVersion
	// Stamped here rather than by callers so every writer of vod.json carries
	// the producer, including the merge path that rewrites an existing one.
	m.Producer = provenance.Producer
	m.ProducerVersion = version.Version
	m.ProducerCommit = version.Commit
	m.ProvenanceSpec = provenance.SpecVersion
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal manifest: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".vod-manifest-*")
	if err != nil {
		return fmt.Errorf("create manifest temp file: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("write manifest: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("close manifest: %w", err)
	}
	if err := os.Rename(tmpName, filepath.Join(dir, ManifestName)); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("rename manifest into place: %w", err)
	}
	return nil
}

// ReadManifest loads dir/vod.json.
func ReadManifest(dir string) (Manifest, error) {
	var m Manifest
	data, err := os.ReadFile(filepath.Join(dir, ManifestName))
	if err != nil {
		return m, err
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return m, fmt.Errorf("parse %s: %w", ManifestName, err)
	}
	return m, nil
}

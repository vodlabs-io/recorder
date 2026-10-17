package processing

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/vodlabs-io/recorder/internal/ffmpeg"
	"github.com/vodlabs-io/recorder/internal/recording"
)

// writeManifest assembles and writes the session's vod.json at finalize time,
// before the move sweep so the manifest travels with the folder. Per-file
// times/durations come from matching each completed group's part stems against
// the recorded parts; file sizes prefer the bytes on disk (a file already
// relocated by an eager move falls back to the summed part bytes). Best-effort:
// callers log the returned error and continue, since a missing manifest only
// degrades the VOD Archive listing to the filesystem fallback.
func (j *job) writeManifest(ctx context.Context, logger *slog.Logger, s *Service) error {
	groups, err := s.completedGroupsForSession(ctx, j.vod.SessionID)
	if err != nil {
		return err
	}

	partsByStem := make(map[string]recording.Part, len(j.vod.Parts))
	for _, p := range j.vod.Parts {
		partsByStem[partStem(p.Path)] = p
	}

	m := recording.Manifest{
		Platform:    j.vod.Platform,
		Username:    j.vod.Username,
		SessionID:   j.vod.SessionID,
		Writer:      j.vod.WriterName,
		TotalBytes:  j.vod.Stats.TotalBytes,
		DurationSec: 0,
	}

	for _, g := range groups {
		f := recording.ManifestFile{Name: g.output}
		var partBytes int64
		for _, stem := range g.inputs {
			p, ok := partsByStem[stem]
			if !ok {
				continue
			}
			partBytes += p.Bytes
			if f.Start.IsZero() || p.StartTime.Before(f.Start) {
				f.Start = p.StartTime
			}
			if p.EndTime.After(f.End) {
				f.End = p.EndTime
			}
			f.DurationSec += p.EndTime.Sub(p.StartTime).Seconds()
		}
		outPath := filepath.Join(j.vod.BaseOutDir, g.output)
		if info, err := os.Stat(outPath); err == nil {
			f.Bytes = info.Size()
			// Prefer the media-measured duration over the wall-clock estimate
			// (which includes reconnect gaps). Best-effort: builds bundling no
			// ffprobe, or an already-moved file, keep the wall-clock value.
			if res, perr := ffmpeg.Probe(ctx, logger, outPath); perr == nil && res.Duration > 0 {
				f.DurationSec = res.Duration
			} else if perr != nil {
				logger.Debug("Manifest duration probe unavailable", "file", g.output, "error", perr)
			}
		} else {
			f.Bytes = partBytes
		}
		m.DurationSec += f.DurationSec
		if m.Start.IsZero() || (!f.Start.IsZero() && f.Start.Before(m.Start)) {
			m.Start = f.Start
		}
		if f.End.After(m.End) {
			m.End = f.End
		}
		m.Files = append(m.Files, f)
	}

	// Session-level fallbacks when part bookkeeping was lost (e.g. a daemon
	// restart mid-session re-reads jobs from the queue without parts).
	if m.Start.IsZero() {
		for _, p := range j.vod.Parts {
			if m.Start.IsZero() || p.StartTime.Before(m.Start) {
				m.Start = p.StartTime
			}
		}
	}
	if m.End.IsZero() {
		m.End = time.Now()
	}
	if m.DurationSec == 0 && !m.Start.IsZero() {
		m.DurationSec = m.End.Sub(m.Start).Seconds()
	}

	if err := recording.WriteManifest(j.vod.BaseOutDir, m); err != nil {
		return err
	}
	logger.Info("Wrote session manifest",
		"path", filepath.Join(j.vod.BaseOutDir, recording.ManifestName), "files", len(m.Files))
	return nil
}

// partStem reduces a part path to the stem the group rows reference: the base
// name without extension (pt1_best.ts and the HLS part dir pt1_best both map
// to "pt1_best").
func partStem(path string) string {
	base := filepath.Base(path)
	return strings.TrimSuffix(base, filepath.Ext(base))
}

// sessionGroup is one completed concat group of a session: which part stems
// went into which output file.
type sessionGroup struct {
	inputs []string
	output string
}

// completedGroupsForSession returns the session's completed group jobs in
// production order, i.e. the definitive list of output files and their parts.
func (s *Service) completedGroupsForSession(ctx context.Context, sessionID string) ([]sessionGroup, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT group_inputs, group_output
		FROM postprocess_jobs
		WHERE session_id = ? AND job_stage = 'group' AND status = 'completed'
		ORDER BY id ASC`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var groups []sessionGroup
	for rows.Next() {
		var inputs, output sql.NullString
		if err := rows.Scan(&inputs, &output); err != nil {
			return nil, err
		}
		g := sessionGroup{output: output.String}
		if inputs.Valid && inputs.String != "" {
			if err := json.Unmarshal([]byte(inputs.String), &g.inputs); err != nil {
				continue
			}
		}
		if g.output != "" {
			groups = append(groups, g)
		}
	}
	return groups, rows.Err()
}

// failedGroupsErr returns an error naming the session's failed group outputs,
// nil when none failed.
func (s *Service) failedGroupsErr(ctx context.Context, sessionID string) error {
	rows, err := s.db.QueryContext(ctx, `
		SELECT group_output
		FROM postprocess_jobs
		WHERE session_id = ? AND job_stage = 'group' AND status = 'failed'
		ORDER BY id ASC`, sessionID)
	if err != nil {
		return fmt.Errorf("list failed groups: %w", err)
	}
	defer rows.Close()

	var failed []string
	for rows.Next() {
		var output sql.NullString
		if err := rows.Scan(&output); err != nil {
			return err
		}
		failed = append(failed, output.String)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(failed) == 0 {
		return nil
	}
	return fmt.Errorf("%d group(s) failed (%s); raw parts are kept",
		len(failed), strings.Join(failed, ", "))
}

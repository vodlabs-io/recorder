package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/vodlabs-io/recorder/internal/encoding"
	"github.com/vodlabs-io/recorder/internal/streamer"
	"github.com/vodlabs-io/recorder/internal/vlapi"
	"github.com/vodlabs-io/recorder/internal/vlconfig"
	"github.com/vodlabs-io/recorder/internal/vodindex"
)

// errDemo refuses the work a Demo daemon cannot fake.
var errDemo = errors.New("not available in the demo")

// archive serves the VOD Archive and the encode queue from the dataset. It
// implements vlapi.VODController and vlapi.EncodeController.
type archive struct {
	ds *Dataset

	mu      sync.Mutex
	vods    []VOD
	onDirty func() // a VOD was removed
}

func newArchive(ds *Dataset) *archive {
	return &archive{ds: ds, vods: slices.Clone(ds.VODs)}
}

// dir is where the daemon's default output template would have put the VOD.
func (a *archive) dir(v VOD) string {
	return filepath.Join(v.Root, v.Key.Platform, v.Key.Username,
		v.Start.Local().Format("2006-01-02_15-04-05-0700"))
}

func fileName(v VOD, i int) string {
	if v.Files == 1 {
		return "vod.mp4"
	}
	return fmt.Sprintf("vod%d.mp4", i+1)
}

// thumbPath is the thumbnail for a VOD's i-th file: <id>.jpg for the first,
// <id>-<n>.jpg after it, falling back to the first when a later one is missing.
func (a *archive) thumbPath(v VOD, i int) string {
	first := filepath.Join(a.ds.Dir, "images", "thumbs", v.ID+".jpg")
	if i == 0 {
		return first
	}
	p := filepath.Join(a.ds.Dir, "images", "thumbs", fmt.Sprintf("%s-%d.jpg", v.ID, i+1))
	if _, err := os.Stat(p); err == nil {
		return p
	}
	return first
}

func (a *archive) ListVODs(context.Context) vodindex.Result {
	a.mu.Lock()
	defer a.mu.Unlock()
	res := vodindex.Result{
		Sessions:         make([]vodindex.VOD, 0, len(a.vods)),
		UnavailableRoots: []string{},
		ScannedAt:        a.ds.Now,
		Rev:              1,
	}
	for _, v := range a.vods {
		res.Sessions = append(res.Sessions, a.index(v))
	}
	slices.SortFunc(res.Sessions, func(x, y vodindex.VOD) int { return y.Start.Compare(x.Start) })
	return res
}

func (a *archive) index(v VOD) vodindex.VOD {
	end := v.Start.Add(v.Duration)
	out := vodindex.VOD{
		Dir:          a.dir(v),
		Root:         v.Root,
		DirMod:       end,
		Platform:     v.Key.Platform,
		Username:     v.Key.Username,
		SessionID:    v.SessionID,
		Start:        v.Start,
		End:          end,
		TotalBytes:   v.Bytes,
		DurationSec:  v.Duration.Seconds(),
		State:        v.State,
		Reason:       v.Reason,
		FromManifest: true,
	}
	for i, t := range v.Titles {
		out.Titles = append(out.Titles, vodindex.Title{
			Text: t,
			At:   v.Start.Add(v.Duration * time.Duration(i) / time.Duration(len(v.Titles))),
		})
	}
	// A Recording still being processed has produced no VOD file yet.
	if v.State == vodindex.StateProcessing {
		return out
	}
	partDur := v.Duration / time.Duration(v.Files)
	for i := range v.Files {
		// Uneven, as real splits are: the last file holds the remainder.
		bytes := v.Bytes / int64(v.Files)
		if i == v.Files-1 {
			bytes = v.Bytes - bytes*int64(v.Files-1)
		}
		start := v.Start.Add(partDur * time.Duration(i))
		_, err := os.Stat(a.thumbPath(v, i))
		out.Files = append(out.Files, vodindex.File{
			Name:        fileName(v, i),
			Bytes:       bytes,
			Start:       start,
			End:         start.Add(partDur),
			DurationSec: partDur.Seconds(),
			HasThumb:    err == nil,
		})
	}
	return out
}

func (a *archive) find(dir string) (VOD, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, v := range a.vods {
		if a.dir(v) == filepath.Clean(dir) {
			return v, true
		}
	}
	return VOD{}, false
}

func (a *archive) RescanVODs(context.Context) {}

func (a *archive) VODThumbnail(_ context.Context, dir, file string) ([]byte, error) {
	v, ok := a.find(dir)
	if !ok {
		return nil, fmt.Errorf("%w: no VOD at %s", vlapi.ErrNotFound, dir)
	}
	for i := range v.Files {
		if fileName(v, i) == file {
			return os.ReadFile(a.thumbPath(v, i))
		}
	}
	return nil, fmt.Errorf("%w: %s has no %s", vlapi.ErrNotFound, dir, file)
}

// DeleteVOD drops the VOD from the listing, so a walkthrough can show a delete.
func (a *archive) DeleteVOD(_ context.Context, dir string) error {
	a.mu.Lock()
	n := len(a.vods)
	a.vods = slices.DeleteFunc(a.vods, func(v VOD) bool { return a.dir(v) == filepath.Clean(dir) })
	removed := len(a.vods) < n
	a.mu.Unlock()
	if !removed {
		return fmt.Errorf("%w: no VOD at %s", vlapi.ErrNotFound, dir)
	}
	if a.onDirty != nil {
		a.onDirty()
	}
	return nil
}

func (a *archive) CancelMoves(context.Context, *streamer.StreamerKey) (int, error) {
	return 0, nil
}

// encodeJobs is one running merge per VOD in the encoding state.
func (a *archive) encodeJobs() []encoding.Job {
	a.mu.Lock()
	defer a.mu.Unlock()
	var jobs []encoding.Job
	for _, v := range a.vods {
		if v.State != vodindex.StateEncoding {
			continue
		}
		inputs := make([]string, 0, v.Files)
		for i := range max(v.Files, 2) {
			inputs = append(inputs, fmt.Sprintf("vod%d.mp4", i+1))
		}
		started := a.ds.Now.Add(-4 * time.Minute)
		jobs = append(jobs, encoding.Job{
			ID:        int64(len(jobs) + 1),
			Type:      "merge",
			Source:    "manual",
			Dir:       a.dir(v),
			Platform:  v.Key.Platform,
			Username:  v.Key.Username,
			SessionID: v.SessionID,
			Inputs:    inputs,
			Output:    "vod.mp4",
			Position:  len(jobs),
			Status:    "running",
			Stage:     "encode",
			CreatedAt: started.Unix(),
			StartedAt: started.Unix(),
		})
	}
	return jobs
}

func (a *archive) ListEncodeQueue(context.Context) (encoding.Queue, error) {
	jobs := a.encodeJobs()
	if jobs == nil {
		jobs = []encoding.Job{}
	}
	return encoding.Queue{Jobs: jobs}, nil
}

func (a *archive) ListEncoders(context.Context, bool) (vlapi.EncoderList, error) {
	return vlapi.EncoderList{
		Selected: "auto",
		Encoders: []encoding.EncoderStatus{
			{ID: "h264_videotoolbox", Name: "Apple VideoToolbox", Available: true, Working: true, Hardware: true},
			{ID: "libx264", Name: "Software (libx264)", Available: true, Working: true},
		},
	}, nil
}

func (a *archive) EnqueueMerge(context.Context, string, bool) (encoding.Job, error) {
	return encoding.Job{}, errDemo
}
func (a *archive) RemoveEncodeJob(context.Context, int64) error          { return errDemo }
func (a *archive) ReorderEncodeJob(context.Context, int64, string) error { return errDemo }
func (a *archive) CancelEncodeJob(context.Context, int64) error          { return errDemo }
func (a *archive) RetryEncodeJob(context.Context, int64) error           { return errDemo }
func (a *archive) SetEncodeQueuePaused(context.Context, bool) error      { return errDemo }

// settings is the user-facing configuration. Edits are kept in memory so
// Settings screens behave, and are forgotten on exit.
type settings struct {
	mu  sync.Mutex
	cfg vlapi.UserConfig
}

func newSettings(ds *Dataset) *settings {
	return &settings{cfg: vlapi.UserConfig{
		DownloadDir:     ds.DownloadDir,
		Variant:         "best",
		OutputTemplate:  vlconfig.DefaultOutputTemplate,
		DeletePartFiles: true,
		ArchiveDir:      ds.ArchiveDir,
		ScanDirs:        []string{},
		Encoder:         "auto",
		LowSpaceWarnGB:  vlconfig.DefaultLowSpaceWarnGB,
	}}
}

func (s *settings) GetConfig() vlapi.UserConfig {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg
}

func (s *settings) SetConfig(cfg vlapi.UserConfig) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cfg = cfg
	return nil
}

func (s *settings) ArchiveConfigChanged() {}

// intervals reports a plausible polling cadence for every Platform.
func intervals(context.Context) (vlapi.CloudIntervals, error) {
	now := time.Now().Unix()
	out := vlapi.CloudIntervals{TargetSeconds: 60}
	for i, name := range vlconfig.PlatformNames() {
		secs := 35 + 7*i
		out.Intervals = append(out.Intervals, vlapi.CloudInterval{
			Platform: name, EstimatedSeconds: &secs, UpdatedAt: &now,
		})
	}
	return out, nil
}

// streamerFuncs accepts any add on a known Platform, as if the cloud had found
// the account, so a walkthrough can show adding a Streamer.
func streamerFuncs() vlapi.StreamerFuncs {
	return vlapi.StreamerFuncs{
		Resolve: func(_ context.Context, id streamer.StreamerRef) (streamer.StreamerRef, error) {
			if !id.StreamerKey.Valid() {
				return id, fmt.Errorf("%w: add by platform and username", errDemo)
			}
			if !slices.Contains(vlconfig.PlatformNames(), strings.ToLower(id.Platform)) {
				return id, fmt.Errorf("%w: unknown platform %q", errDemo, id.Platform)
			}
			return id.Canonical(), nil
		},
		Recheck: func(context.Context, streamer.StreamerRef) error { return nil },
	}
}

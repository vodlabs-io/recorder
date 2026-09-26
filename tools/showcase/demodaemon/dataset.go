package main

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	toml "github.com/pelletier/go-toml/v2"
	"github.com/vodlabs-io/recorder/internal/streamer"
)

// datasetFile is dataset.toml as written. Every time in it is an offset back
// from "now" (see resolve), so the same dataset looks current whenever it is
// served. See tools/showcase/README.md for the field reference.
type datasetFile struct {
	Version     string         `toml:"version"`
	Uptime      string         `toml:"uptime"`
	DownloadDir string         `toml:"download_dir"`
	ArchiveDir  string         `toml:"archive_dir"`
	Roots       []rootSpec     `toml:"root"`
	Streamers   []streamerSpec `toml:"streamer"`
	VODs        []vodSpec      `toml:"vod"`
}

type rootSpec struct {
	Paths    []string `toml:"paths"`
	Download bool     `toml:"download"`
	Total    string   `toml:"total"`
	Free     string   `toml:"free"`
}

type streamerSpec struct {
	Platform    string `toml:"platform"`
	Username    string `toml:"username"`
	DisplayName string `toml:"display_name"`
	// Status is the Streamer's LiveStatus.
	Status string `toml:"status"`
	// Record defaults to true.
	Record *bool `toml:"record"`
	// Live Streamers only.
	Title    string `toml:"title"`
	Category string `toml:"category"`
	LiveFor  string `toml:"live_for"`
	Bitrate  string `toml:"bitrate"`
	// Session is the active Session's state while live and recorded:
	// recording (default), lagging or paused.
	Session string `toml:"session"`
	// Offline Streamers: when their last Broadcast started, and how long it ran.
	LastLive         string `toml:"last_live"`
	LastLiveDuration string `toml:"last_live_duration"`
	Added            string `toml:"added"`
	// AvatarPrompt is what images/avatars/<platform>-<username>.jpg was
	// generated from; the daemon itself ignores it.
	AvatarPrompt string `toml:"avatar_prompt"`
	// Verified records when the username was confirmed not to exist on its
	// Platform.
	Verified string `toml:"verified"`
}

type vodSpec struct {
	// ID names the VOD's thumbnails (images/thumbs/<id>.jpg, <id>-2.jpg, …)
	// and seeds its session ID, so it must stay stable once images exist.
	ID       string `toml:"id"`
	Streamer string `toml:"streamer"` // "<platform>/<username>"
	Ago      string `toml:"ago"`
	Duration string `toml:"duration"`
	Size     string `toml:"size"`
	// Files is how many VOD files the VOD holds: 1 → vod.mp4, n → vod1..vodn.mp4.
	Files int `toml:"files"`
	// State is ready (default), processing, moving or encoding.
	State string `toml:"state"`
	// Reason explains a stalled move (no_space, unreachable, permission, other).
	Reason string `toml:"reason"`
	// Folder is download or archive (default: archive when archive_dir is set).
	Folder string `toml:"folder"`
	// Percent is the running encode's progress, for state = encoding.
	Percent float64 `toml:"percent"`
	// Titles are the stream titles, spread evenly across the VOD.
	Titles []string `toml:"titles"`
}

// Dataset is a dataset resolved against one instant.
type Dataset struct {
	Dir         string
	Now         time.Time
	Version     string
	StartTime   time.Time
	DownloadDir string
	ArchiveDir  string
	Roots       []Root
	Streamers   []Streamer
	VODs        []VOD
}

type Root struct {
	Paths    []string
	Download bool
	Total    uint64
	Free     uint64
}

type Streamer struct {
	Key         streamer.StreamerKey
	DisplayName string
	Status      streamer.LiveStatus
	Record      bool
	Title       string
	Category    string
	// LiveStart is when the current (live) or last (offline) Broadcast started.
	LiveStart        time.Time
	LastLiveDuration time.Duration
	Added            time.Time
	// BytesPerS is the Broadcast's bitrate: how fast a recording of it grows,
	// and what a VOD without an explicit size weighs.
	BytesPerS float64
	// SessionID and Session are set for a live Streamer being recorded.
	SessionID string
	Session   string
}

type VOD struct {
	ID        string
	Key       streamer.StreamerKey
	SessionID string
	Start     time.Time
	Duration  time.Duration
	Bytes     int64
	Files     int
	State     string
	Reason    string
	Root      string
	Percent   float64
	Titles    []string
}

func loadDataset(dir string, now time.Time) (*Dataset, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "dataset.toml"))
	if err != nil {
		return nil, err
	}
	var f datasetFile
	if err := toml.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("dataset.toml: %w", err)
	}
	return f.resolve(dir, now)
}

func (f *datasetFile) resolve(dir string, now time.Time) (*Dataset, error) {
	ds := &Dataset{
		Dir:         dir,
		Now:         now,
		Version:     f.Version,
		DownloadDir: f.DownloadDir,
		ArchiveDir:  f.ArchiveDir,
	}
	if ds.Version == "" {
		ds.Version = "1.0.0"
	}
	uptime, err := parseAgo(f.Uptime, "uptime")
	if err != nil {
		return nil, err
	}
	ds.StartTime = now.Add(-uptime)

	for _, r := range f.Roots {
		total, err := parseSize(r.Total)
		if err != nil {
			return nil, err
		}
		free, err := parseSize(r.Free)
		if err != nil {
			return nil, err
		}
		ds.Roots = append(ds.Roots, Root{Paths: r.Paths, Download: r.Download, Total: uint64(total), Free: uint64(free)})
	}

	known := map[streamer.StreamerKey]Streamer{}
	for _, s := range f.Streamers {
		st, err := s.resolve(now)
		if err != nil {
			return nil, fmt.Errorf("streamer %s/%s: %w", s.Platform, s.Username, err)
		}
		if _, dup := known[st.Key]; dup {
			return nil, fmt.Errorf("streamer %s/%s listed twice", s.Platform, s.Username)
		}
		known[st.Key] = st
		ds.Streamers = append(ds.Streamers, st)
	}

	ids := map[string]bool{}
	for _, v := range f.VODs {
		vod, err := v.resolve(now, f, known)
		if err != nil {
			return nil, fmt.Errorf("vod %s: %w", v.ID, err)
		}
		if ids[vod.ID] {
			return nil, fmt.Errorf("vod id %s used twice", vod.ID)
		}
		ids[vod.ID] = true
		ds.VODs = append(ds.VODs, vod)
	}
	return ds, nil
}

func (s streamerSpec) resolve(now time.Time) (Streamer, error) {
	st := Streamer{
		Key:         streamer.StreamerKey{Platform: strings.ToLower(s.Platform), Username: strings.ToLower(s.Username)},
		DisplayName: s.DisplayName,
		Status:      streamer.LiveStatus(s.Status),
		Record:      s.Record == nil || *s.Record,
		Title:       s.Title,
		Category:    s.Category,
	}
	if !st.Key.Valid() {
		return st, fmt.Errorf("platform and username are required")
	}
	if st.Status == "" {
		st.Status = streamer.StatusOffline
	}
	bps, err := parseBitrate(s.Bitrate)
	if err != nil {
		return st, err
	}
	st.BytesPerS = bps
	added, err := parseAgo(s.Added, "added")
	if err != nil {
		return st, err
	}
	if added == 0 {
		added = 60 * 24 * time.Hour
	}
	st.Added = now.Add(-added)

	if st.Status == streamer.StatusLive {
		liveFor, err := parseAgo(s.LiveFor, "live_for")
		if err != nil {
			return st, err
		}
		st.LiveStart = now.Add(-liveFor)
		if st.Record {
			st.SessionID = shortID("session:" + st.Key.String())
			st.Session = s.Session
			if st.Session == "" {
				st.Session = "recording"
			}
		}
		return st, nil
	}
	if s.LastLive != "" {
		ago, err := parseAgo(s.LastLive, "last_live")
		if err != nil {
			return st, err
		}
		st.LiveStart = now.Add(-ago)
		if st.LastLiveDuration, err = parseAgo(s.LastLiveDuration, "last_live_duration"); err != nil {
			return st, err
		}
	}
	return st, nil
}

func (v vodSpec) resolve(now time.Time, f *datasetFile, streamers map[streamer.StreamerKey]Streamer) (VOD, error) {
	platform, username, ok := strings.Cut(v.Streamer, "/")
	if v.ID == "" || !ok {
		return VOD{}, fmt.Errorf(`id and streamer ("platform/username") are required`)
	}
	key := streamer.StreamerKey{Platform: strings.ToLower(platform), Username: strings.ToLower(username)}
	owner, ok := streamers[key]
	if !ok {
		return VOD{}, fmt.Errorf("streamer %s is not in the dataset", v.Streamer)
	}
	vod := VOD{
		ID:        v.ID,
		Key:       key,
		SessionID: shortID("vod:" + v.ID),
		Files:     max(v.Files, 1),
		State:     v.State,
		Reason:    v.Reason,
		Percent:   v.Percent,
		Titles:    v.Titles,
	}
	if vod.State == "" {
		vod.State = "ready"
	}
	ago, err := parseAgo(v.Ago, "ago")
	if err != nil {
		return vod, err
	}
	vod.Start = now.Add(-ago)
	if vod.Duration, err = parseAgo(v.Duration, "duration"); err != nil {
		return vod, err
	}
	if vod.Bytes, err = parseSize(v.Size); err != nil {
		return vod, err
	}
	if vod.Bytes == 0 {
		// The Streamer's bitrate over the VOD's length, give or take a few
		// percent, as encoders never hold a rate exactly.
		sum := sha256.Sum256([]byte(v.ID))
		wobble := 0.94 + 0.12*float64(sum[0])/255
		vod.Bytes = int64(owner.BytesPerS * vod.Duration.Seconds() * wobble)
	}
	switch v.Folder {
	case "download":
		vod.Root = f.DownloadDir
	case "archive":
		vod.Root = f.ArchiveDir
	case "":
		vod.Root = f.ArchiveDir
		// Work still in flight has not reached the Archive folder yet.
		if vod.Root == "" || vod.State == "processing" || vod.State == "moving" {
			vod.Root = f.DownloadDir
		}
	default:
		return vod, fmt.Errorf("folder must be download or archive, not %q", v.Folder)
	}
	if vod.Root == "" {
		return vod, fmt.Errorf("folder %q is not configured", v.Folder)
	}
	return vod, nil
}

// shortID derives a stable 10-character ID shaped like the daemon's session
// IDs, so a dataset's session IDs survive edits to anything but their seed.
func shortID(seed string) string {
	const alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz_-"
	sum := sha256.Sum256([]byte(seed))
	out := make([]byte, 10)
	for i := range out {
		out[i] = alphabet[sum[i]%64]
	}
	return string(out)
}

var agoRe = regexp.MustCompile(`^(?:(\d+)d)?(.*)$`)

// parseAgo parses a duration that may lead with whole days ("3d4h"). Empty is
// zero.
func parseAgo(s, field string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	m := agoRe.FindStringSubmatch(s)
	var d time.Duration
	if m[1] != "" {
		days, _ := strconv.Atoi(m[1])
		d = time.Duration(days) * 24 * time.Hour
	}
	if m[2] != "" {
		rest, err := time.ParseDuration(m[2])
		if err != nil {
			return 0, fmt.Errorf("%s: %q is not a duration like 3d4h20m", field, s)
		}
		d += rest
	}
	return d, nil
}

var sizeRe = regexp.MustCompile(`^([\d.]+)\s*([KMGT]?)B?$`)

// parseSize parses a binary size ("4.9GB" is 4.9 GiB), matching how the UI
// formats bytes back.
func parseSize(s string) (int64, error) {
	s = strings.ToUpper(strings.TrimSpace(s))
	if s == "" {
		return 0, nil
	}
	m := sizeRe.FindStringSubmatch(s)
	if m == nil {
		return 0, fmt.Errorf("%q is not a size like 4.9GB", s)
	}
	n, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0, err
	}
	shift := map[string]uint{"": 0, "K": 10, "M": 20, "G": 30, "T": 40}[m[2]]
	return int64(n * float64(uint64(1)<<shift)), nil
}

// parseBitrate parses a decimal bitrate ("6Mbps") into bytes per second.
func parseBitrate(s string) (float64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 750_000, nil // 6 Mbps
	}
	num := strings.TrimSuffix(strings.TrimSuffix(s, "bps"), "b")
	mult := 1.0
	switch {
	case strings.HasSuffix(num, "M"):
		mult, num = 1e6, strings.TrimSuffix(num, "M")
	case strings.HasSuffix(num, "k"), strings.HasSuffix(num, "K"):
		mult, num = 1e3, num[:len(num)-1]
	}
	n, err := strconv.ParseFloat(num, 64)
	if err != nil {
		return 0, fmt.Errorf("%q is not a bitrate like 6Mbps", s)
	}
	return n * mult / 8, nil
}

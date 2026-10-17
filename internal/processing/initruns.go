package processing

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/vodlabs-io/recorder/internal/ffmpeg"
)

// initRun is a stretch of an HLS part played under one init segment: a
// playlist naming that init once, then its fragments. init is nil for an
// MPEG-TS part.
type initRun struct {
	playlist string
	init     []byte
}

// partInit is one distinct init segment of a part.
type partInit struct {
	mapLine string
	body    []byte
	tracks  []uint32
}

// partSeg is one segment of a part playlist: its tag lines, then its URI.
type partSeg struct {
	lines []string
	init  int    // index into the part's inits, -1 before any EXT-X-MAP
	key   string // EXT-X-KEY in force
}

// initRuns splits a part playlist into runs that each play under a single init
// segment, writing one playlist per run beside it (index.run00.m3u8, …). A
// playlist naming one init (or none) is returned as is.
//
// A fragment is matched to its init by track IDs, not only by the EXT-X-MAP
// above it: across an encoder switch an origin can list fragments of the new
// layout under the old init. Such a fragment joins the nearest earlier init
// whose tracks it carries.
func initRuns(playlist string) ([]initRun, error) {
	data, err := os.ReadFile(playlist)
	if err != nil {
		return nil, err
	}
	dir := filepath.Dir(playlist)

	var header, pending []string
	var inits []partInit
	var segs []partSeg
	cur, key, inBody := -1, "", false
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch {
		case line == "" || line == "#EXT-X-ENDLIST":
		case strings.HasPrefix(line, "#EXT-X-MAP:"):
			inBody = true
			body, err := os.ReadFile(filepath.Join(dir, mapURI(line)))
			if err != nil {
				return nil, fmt.Errorf("read init segment: %w", err)
			}
			cur = slices.IndexFunc(inits, func(in partInit) bool { return bytes.Equal(in.body, body) })
			if cur < 0 {
				inits = append(inits, partInit{mapLine: line, body: body, tracks: initTrackIDs(body)})
				cur = len(inits) - 1
			}
		case strings.HasPrefix(line, "#EXT-X-KEY:"):
			inBody = true
			key = line
			pending = append(pending, line)
		case !strings.HasPrefix(line, "#"):
			inBody = true
			segs = append(segs, partSeg{lines: append(pending, line), init: cur, key: key})
			pending = nil
		case !inBody && !strings.HasPrefix(line, "#EXTINF") && !strings.HasPrefix(line, "#EXT-X-DISCONTINUITY"):
			header = append(header, line)
		default:
			inBody = true
			pending = append(pending, line)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(inits) < 2 {
		var body []byte
		if len(inits) == 1 {
			body = inits[0].body
		}
		return []initRun{{playlist: playlist, init: body}}, nil
	}

	for i := range segs {
		uri := segs[i].lines[len(segs[i].lines)-1]
		segs[i].init = matchInit(inits, segs[i].init, fragmentTrackIDs(filepath.Join(dir, uri)))
	}

	base := strings.TrimSuffix(filepath.Base(playlist), filepath.Ext(playlist))
	var out []initRun
	for start := 0; start < len(segs); {
		end := start + 1
		for end < len(segs) && segs[end].init == segs[start].init {
			end++
		}
		var sb strings.Builder
		for _, l := range header {
			sb.WriteString(l + "\n")
		}
		run := initRun{}
		if k := segs[start].key; k != "" && !slices.Contains(segs[start].lines, k) {
			sb.WriteString(k + "\n")
		}
		if i := segs[start].init; i >= 0 {
			sb.WriteString(inits[i].mapLine + "\n")
			run.init = inits[i].body
		}
		for _, s := range segs[start:end] {
			for _, l := range s.lines {
				sb.WriteString(l + "\n")
			}
		}
		sb.WriteString("#EXT-X-ENDLIST\n")
		run.playlist = filepath.Join(dir, fmt.Sprintf("%s.run%02d.m3u8", base, len(out)))
		if err := os.WriteFile(run.playlist, []byte(sb.String()), 0o644); err != nil {
			return nil, err
		}
		out = append(out, run)
		start = end
	}
	return out, nil
}

// matchInit returns the init a fragment carrying tracks plays under: listed
// when it fits, else the nearest earlier init that does, else the nearest
// later one. A fragment whose tracks are unknown stays with listed.
func matchInit(inits []partInit, listed int, tracks []uint32) int {
	fits := func(i int) bool {
		for _, t := range tracks {
			if !slices.Contains(inits[i].tracks, t) {
				return false
			}
		}
		return len(inits[i].tracks) > 0
	}
	if len(tracks) == 0 || listed < 0 || fits(listed) {
		return listed
	}
	for i := listed - 1; i >= 0; i-- {
		if fits(i) {
			return i
		}
	}
	for i := listed + 1; i < len(inits); i++ {
		if fits(i) {
			return i
		}
	}
	return listed
}

// mixedInits reports whether the runs carry more than one distinct init
// segment.
func mixedInits(runs []initRun) bool {
	var first []byte
	for _, r := range runs {
		if r.init == nil {
			continue
		}
		if first == nil {
			first = r.init
		} else if !bytes.Equal(r.init, first) {
			return true
		}
	}
	return false
}

// remuxRun stream-copies one init run to an MP4 beside its playlist, video
// first and audio second. Inits can order and number their tracks differently,
// and the concat demuxer pairs streams by index, so every run is normalised to
// one layout before the join.
func (j *job) remuxRun(ctx context.Context, logger *slog.Logger, r initRun) (string, error) {
	out := strings.TrimSuffix(r.playlist, filepath.Ext(r.playlist)) + ".mp4"
	if filepath.Base(r.playlist) == "index.m3u8" {
		out = filepath.Join(filepath.Dir(r.playlist), "index.run.mp4")
	}
	args := []string{
		"-analyzeduration", ffmpeg.AnalyzeDuration,
		"-i", r.playlist,
		"-map", "0:v:0?", "-map", "0:a:0?",
		"-c", "copy",
		"-y", out,
	}
	if err := j.runFFmpeg(ctx, args, out, logger); err != nil {
		return "", fmt.Errorf("remux %s: %w", filepath.Base(r.playlist), err)
	}
	return out, nil
}

// initTrackIDs returns the track IDs an init segment declares (moov/trak/tkhd).
func initTrackIDs(init []byte) []uint32 {
	var ids []uint32
	for _, moov := range childBoxes(init, "moov") {
		for _, trak := range childBoxes(moov, "trak") {
			for _, tkhd := range childBoxes(trak, "tkhd") {
				off := 12 // version+flags, creation and modification times (v0)
				if len(tkhd) > 0 && tkhd[0] == 1 {
					off = 20
				}
				if len(tkhd) >= off+4 {
					ids = append(ids, binary.BigEndian.Uint32(tkhd[off:]))
				}
			}
		}
	}
	return ids
}

// fragmentTrackIDs returns the track IDs a fragment's movie fragments carry
// (moof/traf/tfhd), nil when it cannot be read as fMP4 (for one, a segment
// still encrypted). Only box headers and moof boxes are read.
func fragmentTrackIDs(path string) []uint32 {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	var ids []uint32
	hdr := make([]byte, 16)
	for {
		if _, err := io.ReadFull(f, hdr[:8]); err != nil {
			return ids
		}
		size, typ, hlen := int64(binary.BigEndian.Uint32(hdr)), string(hdr[4:8]), int64(8)
		switch size {
		case 0:
			return ids
		case 1:
			if _, err := io.ReadFull(f, hdr[8:16]); err != nil {
				return ids
			}
			size, hlen = int64(binary.BigEndian.Uint64(hdr[8:])), 16
		}
		if size < hlen {
			return ids
		}
		if typ != "moof" {
			if _, err := f.Seek(size-hlen, io.SeekCurrent); err != nil {
				return ids
			}
			continue
		}
		if size > 16<<20 {
			return ids
		}
		moof := make([]byte, size-hlen)
		if _, err := io.ReadFull(f, moof); err != nil {
			return ids
		}
		for _, traf := range childBoxes(moof, "traf") {
			for _, tfhd := range childBoxes(traf, "tfhd") {
				if len(tfhd) >= 8 {
					if id := binary.BigEndian.Uint32(tfhd[4:]); !slices.Contains(ids, id) {
						ids = append(ids, id)
					}
				}
			}
		}
	}
}

// childBoxes returns the payloads of the boxes of type typ directly inside b.
func childBoxes(b []byte, typ string) [][]byte {
	var out [][]byte
	for len(b) >= 8 {
		size, hlen := uint64(binary.BigEndian.Uint32(b)), uint64(8)
		switch size {
		case 0:
			size = uint64(len(b))
		case 1:
			if len(b) < 16 {
				return out
			}
			size, hlen = binary.BigEndian.Uint64(b[8:]), 16
		}
		if size < hlen || size > uint64(len(b)) {
			return out
		}
		if string(b[4:8]) == typ {
			out = append(out, b[hlen:size])
		}
		b = b[size:]
	}
	return out
}

// mapURI returns the URI attribute of an EXT-X-MAP line.
func mapURI(line string) string {
	_, attrs, _ := strings.Cut(line, ":")
	for _, a := range strings.Split(attrs, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(a), "=")
		if ok && k == "URI" {
			return strings.Trim(v, `"`)
		}
	}
	return ""
}

// playlistDuration sums a playlist's EXTINF durations in seconds.
func playlistDuration(playlist string) float64 {
	data, err := os.ReadFile(playlist)
	if err != nil {
		return 0
	}
	var total float64
	for _, line := range strings.Split(string(data), "\n") {
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), "#EXTINF:")
		if !ok {
			continue
		}
		v, _, _ := strings.Cut(rest, ",")
		var d float64
		if _, err := fmt.Sscanf(v, "%g", &d); err == nil && d > 0 {
			total += d
		}
	}
	return total
}

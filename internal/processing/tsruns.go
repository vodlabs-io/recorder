package processing

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// tsClockSlack is how far, in 90 kHz ticks, a segment may start before the
// one ahead of it without being read as a clock restart: B-frame reordering
// and audio that opens a hair early stay well inside it.
const tsClockSlack = 90000 // 1s

// tsHeadBytes caps how much of a segment is read to find its first timestamp.
const tsHeadBytes = 512 << 10

// tsResetRuns splits an MPEG-TS part playlist wherever the stream's clock goes
// backwards, writing one playlist per stretch beside it (index.run00.m3u8, …).
// A playlist whose clock only moves forward is returned as is.
//
// An origin that restarts its encoder mid-broadcast starts its timestamps over,
// often without the EXT-X-DISCONTINUITY tag, and MPEG-TS wraps its 33-bit clock
// every 26.5 hours. Joined as one playlist, every packet after the reset lands
// on top of time already written: the mp4 muxer squeezes them in, the output
// keeps nearly all its bytes but only part of its length, and the group is
// rejected as short with its parts kept. The concat demuxer lays separately
// listed inputs end to end, so listing each stretch on its own restores the
// timeline.
//
// Segments whose timestamp cannot be read (encrypted, or not MPEG-TS) never
// cause a split.
func tsResetRuns(playlist string) ([]initRun, error) {
	data, err := os.ReadFile(playlist)
	if err != nil {
		return nil, err
	}
	dir := filepath.Dir(playlist)

	var header, pending []string
	var segs []partSeg
	key, inBody := "", false
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch {
		case line == "" || line == "#EXT-X-ENDLIST":
		case strings.HasPrefix(line, "#EXT-X-MAP:"):
			// fMP4: init runs handle it.
			return []initRun{{playlist: playlist}}, nil
		case strings.HasPrefix(line, "#EXT-X-KEY:"):
			inBody = true
			key = line
			pending = append(pending, line)
		case !strings.HasPrefix(line, "#"):
			inBody = true
			segs = append(segs, partSeg{lines: append(pending, line), init: -1, key: key})
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

	// starts[i] opens a new stretch at segs[i].
	var starts []int
	prev, havePrev := int64(0), false
	for i, s := range segs {
		pts, ok := tsFirstPTS(filepath.Join(dir, s.lines[len(s.lines)-1]))
		if !ok {
			continue
		}
		if havePrev && pts+tsClockSlack < prev {
			starts = append(starts, i)
		}
		prev, havePrev = pts, true
	}
	if len(starts) == 0 {
		return []initRun{{playlist: playlist}}, nil
	}

	base := strings.TrimSuffix(filepath.Base(playlist), filepath.Ext(playlist))
	bounds := append(append([]int{0}, starts...), len(segs))
	out := make([]initRun, 0, len(bounds)-1)
	for r := 0; r+1 < len(bounds); r++ {
		var sb strings.Builder
		for _, l := range header {
			sb.WriteString(l + "\n")
		}
		run := segs[bounds[r]:bounds[r+1]]
		if k := run[0].key; k != "" && !slices.Contains(run[0].lines, k) {
			sb.WriteString(k + "\n")
		}
		for _, s := range run {
			for _, l := range s.lines {
				if l == "#EXT-X-DISCONTINUITY" {
					continue // each stretch is its own input now
				}
				sb.WriteString(l + "\n")
			}
		}
		sb.WriteString("#EXT-X-ENDLIST\n")
		p := filepath.Join(dir, fmt.Sprintf("%s.run%02d.m3u8", base, r))
		if err := os.WriteFile(p, []byte(sb.String()), 0o644); err != nil {
			return nil, err
		}
		out = append(out, initRun{playlist: p})
	}
	return out, nil
}

// tsFirstPTS returns the first video PTS in an MPEG-TS segment, in 90 kHz
// ticks, falling back to the first PTS of any stream when the head holds no
// video. ok is false when the file is not readable MPEG-TS.
func tsFirstPTS(path string) (pts int64, ok bool) {
	f, err := os.Open(path)
	if err != nil {
		return 0, false
	}
	defer f.Close()
	buf := make([]byte, tsHeadBytes)
	n, err := io.ReadFull(f, buf)
	if err != nil && err != io.ErrUnexpectedEOF {
		return 0, false
	}
	buf = buf[:n]

	anyPTS, haveAny := int64(0), false
	for off := 0; off+188 <= len(buf); off += 188 {
		pkt := buf[off : off+188]
		if pkt[0] != 0x47 {
			return 0, false // not MPEG-TS, or encrypted
		}
		if pkt[1]&0x40 == 0 { // no payload unit start
			continue
		}
		p := 4
		switch pkt[3] >> 4 & 0x3 {
		case 1:
		case 3:
			p += 1 + int(pkt[4])
		default:
			continue
		}
		if p+14 > len(pkt) {
			continue
		}
		pes := pkt[p:]
		if pes[0] != 0 || pes[1] != 0 || pes[2] != 1 {
			continue
		}
		sid := pes[3]
		if pes[7]&0x80 == 0 { // no PTS
			continue
		}
		v := pesTimestamp(pes[9:14])
		if sid >= 0xE0 && sid <= 0xEF {
			return v, true
		}
		if !haveAny {
			anyPTS, haveAny = v, true
		}
	}
	return anyPTS, haveAny
}

// pesTimestamp decodes a 33-bit PES PTS/DTS field.
func pesTimestamp(b []byte) int64 {
	return int64(b[0]>>1&0x07)<<30 | int64(b[1])<<22 | int64(b[2]>>1)<<15 |
		int64(b[3])<<7 | int64(b[4]>>1)
}

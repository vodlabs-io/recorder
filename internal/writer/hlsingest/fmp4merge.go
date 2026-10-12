package hlsingest

// Box-level surgery for joining a video rendition and a separate audio
// rendition into one fragmented-MP4 stream (see demux.go). Only the boxes that
// name a track are touched: the audio track is given an id the video init does
// not use, in its init and in every fragment, and the two inits are joined into
// one moov. Sample data and offsets are left alone, so no fragment changes size.

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
)

// mp4Box is one ISO-BMFF box, by absolute offsets into the buffer it was read
// from.
type mp4Box struct {
	typ        string
	start, end int
	// hdr is the header length: 8, or 16 with a 64-bit size.
	hdr int
}

func (b mp4Box) payload() int { return b.start + b.hdr }

// readBoxes lists the boxes laid end to end in buf[off:end].
func readBoxes(buf []byte, off, end int) ([]mp4Box, error) {
	var out []mp4Box
	for off < end {
		if end-off < 8 {
			return nil, fmt.Errorf("truncated box header at %d", off)
		}
		size := int(binary.BigEndian.Uint32(buf[off:]))
		typ := string(buf[off+4 : off+8])
		hdr := 8
		switch size {
		case 0:
			size = end - off
		case 1:
			if end-off < 16 {
				return nil, fmt.Errorf("truncated %s header at %d", typ, off)
			}
			large := binary.BigEndian.Uint64(buf[off+8:])
			if large > uint64(end-off) {
				return nil, fmt.Errorf("%s box at %d overruns its parent", typ, off)
			}
			size, hdr = int(large), 16
		}
		if size < hdr || off+size > end {
			return nil, fmt.Errorf("%s box at %d has bad size %d", typ, off, size)
		}
		out = append(out, mp4Box{typ: typ, start: off, end: off + size, hdr: hdr})
		off += size
	}
	return out, nil
}

// children lists the boxes inside b.
func children(buf []byte, b mp4Box) ([]mp4Box, error) {
	return readBoxes(buf, b.payload(), b.end)
}

// findBoxes returns every box reached by following path from the top level of
// buf, e.g. "moov", "trak", "tkhd".
func findBoxes(buf []byte, path ...string) []mp4Box {
	level, err := readBoxes(buf, 0, len(buf))
	if err != nil {
		return nil
	}
	for i, typ := range path {
		var next []mp4Box
		for _, b := range level {
			if b.typ != typ {
				continue
			}
			if i == len(path)-1 {
				next = append(next, b)
				continue
			}
			kids, err := children(buf, b)
			if err != nil {
				return nil
			}
			next = append(next, kids...)
		}
		level = next
	}
	return level
}

// fullBoxField returns the offset of a field in a full box (one with version
// and flags), whose position depends on the version: v0 for version 0, v1
// otherwise. Offsets count from the end of version and flags.
func fullBoxField(buf []byte, b mp4Box, v0, v1, width int) (int, error) {
	at := b.payload() + 4 + v0
	if buf[b.payload()] != 0 {
		at = b.payload() + 4 + v1
	}
	if b.payload()+4 > b.end || at+width > b.end {
		return 0, fmt.Errorf("truncated %s box", b.typ)
	}
	return at, nil
}

// trackID reads tkhd's track_ID.
func trackID(buf []byte, tkhd mp4Box) (uint32, error) {
	at, err := fullBoxField(buf, tkhd, 8, 16, 4)
	if err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint32(buf[at:]), nil
}

// initTimescale returns the media timescale of an init segment's first track.
func initTimescale(init []byte) (uint32, error) {
	mdhd := findBoxes(init, "moov", "trak", "mdia", "mdhd")
	if len(mdhd) == 0 {
		return 0, errors.New("init segment has no mdhd")
	}
	at, err := fullBoxField(init, mdhd[0], 8, 16, 4)
	if err != nil {
		return 0, err
	}
	ts := binary.BigEndian.Uint32(init[at:])
	if ts == 0 {
		return 0, errors.New("init segment has a zero timescale")
	}
	return ts, nil
}

// initHasSound reports whether an init segment declares an audio track.
func initHasSound(init []byte) bool {
	for _, h := range findBoxes(init, "moov", "trak", "mdia", "hdlr") {
		// version/flags, pre_defined, then handler_type.
		at := h.payload() + 8
		if at+4 <= h.end && string(init[at:at+4]) == "soun" {
			return true
		}
	}
	return false
}

// fragmentStart returns the decode time the first fragment of a media segment
// starts at, in its track's timescale.
func fragmentStart(seg []byte) (uint64, bool) {
	tfdt := findBoxes(seg, "moof", "traf", "tfdt")
	if len(tfdt) == 0 {
		return 0, false
	}
	b := tfdt[0]
	if b.payload()+4 > b.end {
		return 0, false
	}
	if seg[b.payload()] != 0 {
		if b.payload()+12 > b.end {
			return 0, false
		}
		return binary.BigEndian.Uint64(seg[b.payload()+4:]), true
	}
	if b.payload()+8 > b.end {
		return 0, false
	}
	return uint64(binary.BigEndian.Uint32(seg[b.payload()+4:])), true
}

// retrackFragments returns a copy of a media segment with every fragment, and
// any segment index, re-addressed to track id.
func retrackFragments(seg []byte, id uint32) ([]byte, error) {
	out := bytes.Clone(seg)
	top, err := readBoxes(out, 0, len(out))
	if err != nil {
		return nil, err
	}
	for _, b := range top {
		switch b.typ {
		case "sidx":
			// version/flags, then reference_ID.
			if b.payload()+8 > b.end {
				return nil, errors.New("truncated sidx box")
			}
			binary.BigEndian.PutUint32(out[b.payload()+4:], id)
		case "moof":
			trafs, err := children(out, b)
			if err != nil {
				return nil, err
			}
			for _, traf := range trafs {
				if traf.typ != "traf" {
					continue
				}
				kids, err := children(out, traf)
				if err != nil {
					return nil, err
				}
				for _, k := range kids {
					if k.typ != "tfhd" {
						continue
					}
					// version/flags, then track_ID.
					if k.payload()+8 > k.end {
						return nil, errors.New("truncated tfhd box")
					}
					binary.BigEndian.PutUint32(out[k.payload()+4:], id)
				}
			}
		}
	}
	return out, nil
}

// mergeInits joins a video init segment and an audio one into a single init
// declaring both tracks, and returns it with the id the audio track now has
// (one past the highest id the video init uses). Everything the video init
// carries is kept in order; the audio track and its trex are added beside the
// video's.
func mergeInits(video, audio []byte) ([]byte, uint32, error) {
	vTop, err := readBoxes(video, 0, len(video))
	if err != nil {
		return nil, 0, fmt.Errorf("video init: %w", err)
	}
	var vMoov *mp4Box
	for i := range vTop {
		if vTop[i].typ == "moov" {
			vMoov = &vTop[i]
		}
	}
	if vMoov == nil {
		return nil, 0, errors.New("video init has no moov")
	}
	vKids, err := children(video, *vMoov)
	if err != nil {
		return nil, 0, fmt.Errorf("video init: %w", err)
	}

	var maxID uint32
	for _, t := range findBoxes(video, "moov", "trak", "tkhd") {
		id, err := trackID(video, t)
		if err != nil {
			return nil, 0, fmt.Errorf("video init: %w", err)
		}
		maxID = max(maxID, id)
	}
	audioID := maxID + 1

	aTraks := findBoxes(audio, "moov", "trak")
	if len(aTraks) == 0 {
		return nil, 0, errors.New("audio init has no trak")
	}
	aTrak := bytes.Clone(audio[aTraks[0].start:aTraks[0].end])
	tkhd := findBoxes(aTrak, "trak", "tkhd")
	if len(tkhd) == 0 {
		return nil, 0, errors.New("audio init has no tkhd")
	}
	oldID, err := trackID(aTrak, tkhd[0])
	if err != nil {
		return nil, 0, fmt.Errorf("audio init: %w", err)
	}
	at, _ := fullBoxField(aTrak, tkhd[0], 8, 16, 4)
	binary.BigEndian.PutUint32(aTrak[at:], audioID)

	var aTrex []byte
	for _, t := range findBoxes(audio, "moov", "mvex", "trex") {
		// version/flags, then track_ID.
		if t.payload()+8 > t.end {
			return nil, 0, errors.New("audio init: truncated trex box")
		}
		if binary.BigEndian.Uint32(audio[t.payload()+4:]) == oldID || aTrex == nil {
			aTrex = bytes.Clone(audio[t.start:t.end])
			binary.BigEndian.PutUint32(aTrex[t.hdr+4:], audioID)
		}
	}
	if aTrex == nil {
		return nil, 0, errors.New("audio init has no trex")
	}

	var moov []byte
	sawMvex := false
	for _, k := range vKids {
		raw := video[k.start:k.end]
		switch k.typ {
		case "mvhd":
			mvhd := bytes.Clone(raw)
			if len(mvhd) < 12 {
				return nil, 0, errors.New("video init: truncated mvhd box")
			}
			// next_track_ID closes the box.
			binary.BigEndian.PutUint32(mvhd[len(mvhd)-4:], audioID+1)
			moov = append(moov, mvhd...)
		case "mvex":
			sawMvex = true
			moov = append(moov, aTrak...)
			kids, err := children(video, k)
			if err != nil {
				return nil, 0, fmt.Errorf("video init: %w", err)
			}
			var mvex []byte
			for _, c := range kids {
				mvex = append(mvex, video[c.start:c.end]...)
			}
			mvex = append(mvex, aTrex...)
			moov = append(moov, boxBytes("mvex", mvex)...)
		default:
			moov = append(moov, raw...)
		}
	}
	if !sawMvex {
		return nil, 0, errors.New("video init has no mvex")
	}

	var out []byte
	for _, b := range vTop {
		if b.typ == "moov" {
			out = append(out, boxBytes("moov", moov)...)
			continue
		}
		out = append(out, video[b.start:b.end]...)
	}
	return out, audioID, nil
}

// boxBytes wraps payload in a box header.
func boxBytes(typ string, payload []byte) []byte {
	out := make([]byte, 8, 8+len(payload))
	binary.BigEndian.PutUint32(out, uint32(8+len(payload)))
	copy(out[4:], typ)
	return append(out, payload...)
}

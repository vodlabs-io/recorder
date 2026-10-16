// Package webm writes the VP8 video and Opus audio of an FLV stream as a WebM
// file. Neither enhanced FLV ("vp08") nor MP4 carries VP8 through ffmpeg, so a
// VP8 recording is kept as WebM until post-processing re-encodes it.
package webm

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
)

// ErrUnsupportedCodec reports FLV content WebM cannot carry: video other than
// VP8, or audio other than Opus.
var ErrUnsupportedCodec = errors.New("webm: unsupported codec")

const (
	videoTrack = 1
	audioTrack = 2

	// clusterSpanMs bounds a cluster: block timecodes are signed 16-bit
	// milliseconds relative to it.
	clusterSpanMs = 30000

	// audioWaitMs is how long after the first keyframe the header waits for
	// an Opus sequence header before declaring the file video-only.
	audioWaitMs = 2000

	opusRate          = 48000
	opusSeekPreRollNs = 80_000_000 // the pre-roll Matroska prescribes for Opus
)

// EBML element IDs.
const (
	ebmlHeader         = 0x1A45DFA3
	ebmlVersion        = 0x4286
	ebmlReadVersion    = 0x42F7
	ebmlMaxIDLength    = 0x42F2
	ebmlMaxSizeLength  = 0x42F3
	ebmlDocType        = 0x4282
	ebmlDocTypeVersion = 0x4287
	ebmlDocTypeRead    = 0x4285

	mkvSegment         = 0x18538067
	mkvInfo            = 0x1549A966
	mkvTimecodeScale   = 0x2AD7B1
	mkvMuxingApp       = 0x4D80
	mkvWritingApp      = 0x5741
	mkvTracks          = 0x1654AE6B
	mkvTrackEntry      = 0xAE
	mkvTrackNumber     = 0xD7
	mkvTrackUID        = 0x73C5
	mkvTrackType       = 0x83
	mkvCodecID         = 0x86
	mkvCodecPrivate    = 0x63A2
	mkvCodecDelay      = 0x56AA
	mkvSeekPreRoll     = 0x56BB
	mkvVideo           = 0xE0
	mkvPixelWidth      = 0xB0
	mkvPixelHeight     = 0xBA
	mkvAudio           = 0xE1
	mkvSamplingFreq    = 0xB5
	mkvChannels        = 0x9F
	mkvCluster         = 0x1F43B675
	mkvClusterTimecode = 0xE7
	mkvSimpleBlock     = 0xA3
)

// unknownSize marks a Segment or Cluster whose end is wherever the file ends,
// so every byte is final once written and an interrupted recording stays
// readable.
var unknownSize = []byte{0x01, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}

type frame struct {
	track int
	tsMs  int64
	key   bool
	data  []byte
}

// Remuxer is an io.Writer that takes an FLV byte stream and writes it to its
// output as live WebM: an unknown-size Segment, then a Cluster at every video
// keyframe. The header is written once the first keyframe gives the frame size
// and the Opus sequence header, if any, has arrived; frames before then are
// held, and video before the first keyframe is dropped.
//
// Script tags are ignored. Any other codec fails the write with
// ErrUnsupportedCodec. It is not safe for concurrent use.
type Remuxer struct {
	out io.Writer
	buf []byte

	flvHeaderDone bool
	headerDone    bool
	opusHead      []byte
	width, height int
	firstKeyMs    int64
	held          []frame

	clusterOpen bool
	clusterMs   int64

	err error
}

// NewRemuxer returns a Remuxer writing WebM to out.
func NewRemuxer(out io.Writer) *Remuxer {
	return &Remuxer{out: out, firstKeyMs: -1}
}

// Write consumes FLV bytes, buffering a trailing partial tag.
func (r *Remuxer) Write(p []byte) (int, error) {
	if r.err != nil {
		return 0, r.err
	}
	r.buf = append(r.buf, p...)
	var e ebml
	err := r.parse(&e)
	if e.Len() > 0 {
		if _, werr := r.out.Write(e.Bytes()); werr != nil && err == nil {
			err = werr
		}
	}
	if err != nil {
		r.err = err
		return 0, err
	}
	return len(p), nil
}

// parse turns every complete FLV tag in buf into WebM appended to e.
func (r *Remuxer) parse(e *ebml) error {
	pos := 0
	defer func() { r.buf = append(r.buf[:0], r.buf[pos:]...) }()
	if !r.flvHeaderDone {
		if len(r.buf) < 3 {
			return nil
		}
		if string(r.buf[:3]) == "FLV" {
			if len(r.buf) < 9 {
				return nil
			}
			n := int(binary.BigEndian.Uint32(r.buf[5:9])) + 4 // header, then PreviousTagSize0
			if len(r.buf) < n {
				return nil
			}
			pos = n
		}
		r.flvHeaderDone = true
	}
	for len(r.buf)-pos >= 11 {
		h := r.buf[pos:]
		size := int(h[1])<<16 | int(h[2])<<8 | int(h[3])
		if len(h) < 11+size+4 {
			return nil
		}
		ts := int64(uint32(h[7])<<24 | uint32(h[4])<<16 | uint32(h[5])<<8 | uint32(h[6]))
		if err := r.tag(e, h[0]&0x1f, ts, h[11:11+size]); err != nil {
			return err
		}
		pos += 11 + size + 4
	}
	return nil
}

func (r *Remuxer) tag(e *ebml, typ byte, tsMs int64, p []byte) error {
	switch typ {
	case 8:
		return r.audioTag(e, tsMs, p)
	case 9:
		return r.videoTag(e, tsMs, p)
	}
	return nil
}

// videoTag takes an enhanced-FLV "vp08" tag: a SequenceStart (whose
// configuration record VP8 does not need) or CodedFrames.
func (r *Remuxer) videoTag(e *ebml, tsMs int64, p []byte) error {
	if len(p) < 5 || p[0]&0x80 == 0 || string(p[1:5]) != "vp08" {
		return fmt.Errorf("%w: video %s", ErrUnsupportedCodec, videoName(p))
	}
	switch p[0] & 0x0f {
	case 1, 3: // CodedFrames, CodedFramesX
	default:
		return nil
	}
	data := p[5:]
	if len(data) == 0 {
		return nil
	}
	key := p[0]>>4&0x07 == 1
	if r.firstKeyMs < 0 {
		w, h, ok := vp8Dimensions(data)
		if !ok {
			return nil
		}
		r.width, r.height, r.firstKeyMs = w, h, tsMs
		key = true
	}
	return r.media(e, frame{track: videoTrack, tsMs: tsMs, key: key, data: bytes.Clone(data)})
}

// audioTag takes an enhanced-FLV "Opus" tag: a SequenceStart carrying the
// identification header, or CodedFrames.
func (r *Remuxer) audioTag(e *ebml, tsMs int64, p []byte) error {
	if len(p) < 5 || p[0]>>4 != 9 || string(p[1:5]) != "Opus" {
		return fmt.Errorf("%w: audio %s", ErrUnsupportedCodec, audioName(p))
	}
	switch p[0] & 0x0f {
	case 0: // SequenceStart
		if !r.headerDone && len(p) >= 5+19 && string(p[5:13]) == "OpusHead" {
			r.opusHead = bytes.Clone(p[5:])
		}
		return nil
	case 1: // CodedFrames
	default:
		return nil
	}
	if len(p) == 5 || r.opusHead == nil {
		return nil
	}
	return r.media(e, frame{track: audioTrack, tsMs: tsMs, key: true, data: bytes.Clone(p[5:])})
}

// media writes a frame, or holds it until the header can be written.
func (r *Remuxer) media(e *ebml, f frame) error {
	if r.headerDone {
		if f.track == audioTrack && !r.hasAudio() {
			return nil
		}
		r.block(e, f)
		return nil
	}
	if r.firstKeyMs < 0 || f.tsMs < r.firstKeyMs {
		return nil
	}
	r.held = append(r.held, f)
	if r.opusHead == nil && f.tsMs-r.firstKeyMs < audioWaitMs {
		return nil
	}
	r.writeHeader(e)
	for _, h := range r.held {
		if h.track == audioTrack && !r.hasAudio() {
			continue
		}
		r.block(e, h)
	}
	r.held = nil
	return nil
}

func (r *Remuxer) hasAudio() bool { return r.headerDone && r.opusHead != nil }

func (r *Remuxer) writeHeader(e *ebml) {
	e.master(ebmlHeader, func(h *ebml) {
		h.uint(ebmlVersion, 1)
		h.uint(ebmlReadVersion, 1)
		h.uint(ebmlMaxIDLength, 4)
		h.uint(ebmlMaxSizeLength, 8)
		h.str(ebmlDocType, "webm")
		h.uint(ebmlDocTypeVersion, 4)
		h.uint(ebmlDocTypeRead, 2)
	})
	e.id(mkvSegment)
	e.Write(unknownSize)
	e.master(mkvInfo, func(i *ebml) {
		i.uint(mkvTimecodeScale, 1_000_000) // timecodes in milliseconds
		i.str(mkvMuxingApp, "vodlabs")
		i.str(mkvWritingApp, "vodlabs")
	})
	e.master(mkvTracks, func(t *ebml) {
		t.master(mkvTrackEntry, func(v *ebml) {
			v.uint(mkvTrackNumber, videoTrack)
			v.uint(mkvTrackUID, videoTrack)
			v.uint(mkvTrackType, 1)
			v.str(mkvCodecID, "V_VP8")
			v.master(mkvVideo, func(d *ebml) {
				d.uint(mkvPixelWidth, uint64(r.width))
				d.uint(mkvPixelHeight, uint64(r.height))
			})
		})
		if r.opusHead == nil {
			return
		}
		preSkip := binary.LittleEndian.Uint16(r.opusHead[10:12])
		t.master(mkvTrackEntry, func(a *ebml) {
			a.uint(mkvTrackNumber, audioTrack)
			a.uint(mkvTrackUID, audioTrack)
			a.uint(mkvTrackType, 2)
			a.str(mkvCodecID, "A_OPUS")
			a.bytes(mkvCodecPrivate, r.opusHead)
			a.uint(mkvCodecDelay, uint64(preSkip)*1_000_000_000/opusRate)
			a.uint(mkvSeekPreRoll, opusSeekPreRollNs)
			a.master(mkvAudio, func(d *ebml) {
				d.float(mkvSamplingFreq, opusRate)
				d.uint(mkvChannels, uint64(r.opusHead[9]))
			})
		})
	})
	r.headerDone = true
}

// block writes f as a SimpleBlock, opening a Cluster at each video keyframe
// and wherever its relative timecode would not fit.
func (r *Remuxer) block(e *ebml, f frame) {
	rel := f.tsMs - r.clusterMs
	if !r.clusterOpen || (f.track == videoTrack && f.key) || rel >= clusterSpanMs || rel < math.MinInt16 {
		r.clusterMs = max(f.tsMs, 0)
		r.clusterOpen = true
		rel = f.tsMs - r.clusterMs
		e.id(mkvCluster)
		e.Write(unknownSize)
		e.uint(mkvClusterTimecode, uint64(r.clusterMs))
	}
	flags := byte(0)
	if f.key {
		flags = 0x80
	}
	head := []byte{0x80 | byte(f.track), byte(uint16(rel) >> 8), byte(rel), flags}
	e.id(mkvSimpleBlock)
	e.size(uint64(len(head) + len(f.data)))
	e.Write(head)
	e.Write(f.data)
}

// vp8Dimensions reads the frame size from a VP8 key frame's header.
func vp8Dimensions(frame []byte) (w, h int, ok bool) {
	if len(frame) < 10 || frame[0]&1 != 0 || frame[3] != 0x9d || frame[4] != 0x01 || frame[5] != 0x2a {
		return 0, 0, false
	}
	w = int(binary.LittleEndian.Uint16(frame[6:8]) & 0x3fff)
	h = int(binary.LittleEndian.Uint16(frame[8:10]) & 0x3fff)
	return w, h, w > 0 && h > 0
}

func videoName(p []byte) string {
	switch {
	case len(p) == 0:
		return "(empty)"
	case p[0]&0x80 != 0 && len(p) >= 5:
		return fmt.Sprintf("%q", p[1:5])
	}
	return fmt.Sprintf("codec id %d", p[0]&0x0f)
}

func audioName(p []byte) string {
	switch {
	case len(p) == 0:
		return "(empty)"
	case p[0]>>4 == 9 && len(p) >= 5:
		return fmt.Sprintf("%q", p[1:5])
	}
	return fmt.Sprintf("sound format %d", p[0]>>4)
}

// ebml builds EBML elements.
type ebml struct{ bytes.Buffer }

func (e *ebml) id(id uint32) {
	switch {
	case id >= 1<<24:
		e.Write([]byte{byte(id >> 24), byte(id >> 16), byte(id >> 8), byte(id)})
	case id >= 1<<16:
		e.Write([]byte{byte(id >> 16), byte(id >> 8), byte(id)})
	case id >= 1<<8:
		e.Write([]byte{byte(id >> 8), byte(id)})
	default:
		e.WriteByte(byte(id))
	}
}

// size writes an element data size as an 8-byte vint, which every size fits.
func (e *ebml) size(n uint64) {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], n|1<<56)
	e.Write(b[:])
}

func (e *ebml) bytes(id uint32, data []byte) {
	e.id(id)
	e.size(uint64(len(data)))
	e.Write(data)
}

func (e *ebml) uint(id uint32, v uint64) {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], v)
	i := 0
	for i < 7 && b[i] == 0 {
		i++
	}
	e.bytes(id, b[i:])
}

func (e *ebml) str(id uint32, s string) { e.bytes(id, []byte(s)) }

func (e *ebml) float(id uint32, f float64) {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], math.Float64bits(f))
	e.bytes(id, b[:])
}

func (e *ebml) master(id uint32, body func(*ebml)) {
	var inner ebml
	body(&inner)
	e.bytes(id, inner.Bytes())
}

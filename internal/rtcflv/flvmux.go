package rtcflv

import (
	"bytes"
	"encoding/binary"
	"io"
)

// FLV tag types.
const (
	flvAudio = 8
	flvVideo = 9
)

// H.264 NAL unit types the muxer acts on.
const (
	nalIDR = 5
	nalSPS = 7
	nalPPS = 8
	nalAUD = 9
)

// flvMuxer writes H.264 access units and Opus packets as an FLV stream: AVC
// video in the classic layout, Opus audio in the enhanced (ex-header) layout,
// each preceded by its sequence header. It is not safe for concurrent use.
type flvMuxer struct {
	w          io.Writer
	headerDone bool
	sps, pps   []byte
	avcSent    []byte // the AVCDecoderConfigurationRecord last written
	opusSent   bool
	channels   int
}

func newFLVMuxer(w io.Writer, channels int) *flvMuxer {
	return &flvMuxer{w: w, channels: channels}
}

// writeVideo writes one access unit, given as AVCC (4-byte length-prefixed)
// NAL units, at tsMs. Units arriving before the first SPS/PPS and keyframe
// are dropped by the caller; parameter sets found in the unit refresh the
// sequence header, which is re-sent whenever it changes.
func (m *flvMuxer) writeVideo(tsMs int64, au []byte) error {
	var body bytes.Buffer
	key := false
	for len(au) >= 4 {
		n := int(binary.BigEndian.Uint32(au))
		if n == 0 || 4+n > len(au) {
			break
		}
		nal := au[4 : 4+n]
		au = au[4+n:]
		switch nal[0] & 0x1f {
		case nalSPS:
			m.sps = bytes.Clone(nal)
			continue
		case nalPPS:
			m.pps = bytes.Clone(nal)
			continue
		case nalAUD:
			continue
		case nalIDR:
			key = true
		}
		var l [4]byte
		binary.BigEndian.PutUint32(l[:], uint32(len(nal)))
		body.Write(l[:])
		body.Write(nal)
	}
	if body.Len() == 0 || m.sps == nil || m.pps == nil {
		return nil
	}
	if rec := avcConfig(m.sps, m.pps); !bytes.Equal(rec, m.avcSent) {
		if err := m.tag(flvVideo, tsMs, append([]byte{0x17, 0, 0, 0, 0}, rec...)); err != nil {
			return err
		}
		m.avcSent = rec
	}
	head := []byte{0x27, 1, 0, 0, 0}
	if key {
		head[0] = 0x17
	}
	return m.tag(flvVideo, tsMs, append(head, body.Bytes()...))
}

// writeAudio writes one Opus packet at tsMs.
func (m *flvMuxer) writeAudio(tsMs int64, pkt []byte) error {
	if len(pkt) == 0 {
		return nil
	}
	if !m.opusSent {
		if err := m.tag(flvAudio, tsMs, append([]byte{0x90, 'O', 'p', 'u', 's'}, opusHead(m.channels)...)); err != nil {
			return err
		}
		m.opusSent = true
	}
	return m.tag(flvAudio, tsMs, append([]byte{0x91, 'O', 'p', 'u', 's'}, pkt...))
}

func (m *flvMuxer) tag(typ byte, tsMs int64, payload []byte) error {
	if !m.headerDone {
		// FLV v1, audio+video, 9-byte header, then PreviousTagSize0.
		if _, err := m.w.Write([]byte{'F', 'L', 'V', 1, 0x05, 0, 0, 0, 9, 0, 0, 0, 0}); err != nil {
			return err
		}
		m.headerDone = true
	}
	ts := uint32(max(tsMs, 0))
	n := len(payload)
	buf := make([]byte, 0, 11+n+4)
	buf = append(buf, typ, byte(n>>16), byte(n>>8), byte(n),
		byte(ts>>16), byte(ts>>8), byte(ts), byte(ts>>24), 0, 0, 0)
	buf = append(buf, payload...)
	buf = binary.BigEndian.AppendUint32(buf, uint32(11+n))
	_, err := m.w.Write(buf)
	return err
}

// avcConfig builds an AVCDecoderConfigurationRecord from one SPS and PPS.
func avcConfig(sps, pps []byte) []byte {
	rec := []byte{1, sps[1], sps[2], sps[3], 0xff, 0xe1}
	rec = binary.BigEndian.AppendUint16(rec, uint16(len(sps)))
	rec = append(rec, sps...)
	rec = append(rec, 1)
	rec = binary.BigEndian.AppendUint16(rec, uint16(len(pps)))
	rec = append(rec, pps...)
	return rec
}

// opusHead is the Opus identification header (RFC 7845 §5.1) for a 48 kHz
// stream, which is what an Opus sequence header carries.
func opusHead(channels int) []byte {
	h := []byte("OpusHead")
	h = append(h, 1, byte(channels))
	h = binary.LittleEndian.AppendUint16(h, 312) // pre-skip: libopus's 48 kHz lookahead
	h = binary.LittleEndian.AppendUint32(h, 48000)
	h = binary.LittleEndian.AppendUint16(h, 0) // output gain
	return append(h, 0)                        // channel mapping family 0
}

// hasIDR reports whether an AVCC access unit contains an IDR slice.
func hasIDR(au []byte) bool {
	for len(au) >= 4 {
		n := int(binary.BigEndian.Uint32(au))
		if n == 0 || 4+n > len(au) {
			return false
		}
		if au[4]&0x1f == nalIDR {
			return true
		}
		au = au[4+n:]
	}
	return false
}

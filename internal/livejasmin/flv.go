package livejasmin

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

// flvMuxer writes AVC video and AAC audio as an FLV stream, each preceded by
// its sequence header, which is re-sent whenever the configuration changes.
// It is not safe for concurrent use.
type flvMuxer struct {
	w          io.Writer
	headerDone bool
	avcSent    []byte
	aacSent    []byte
}

func newFLVMuxer(w io.Writer) *flvMuxer {
	return &flvMuxer{w: w}
}

// setConfig sends the sequence headers for avcC and asc where they differ
// from the ones last sent; tsMs stamps them.
func (m *flvMuxer) setConfig(tsMs int64, avcC, asc []byte) error {
	if len(avcC) > 0 && !bytes.Equal(avcC, m.avcSent) {
		if err := m.tag(flvVideo, tsMs, append([]byte{0x17, 0, 0, 0, 0}, avcC...)); err != nil {
			return err
		}
		m.avcSent = bytes.Clone(avcC)
	}
	if len(asc) > 0 && !bytes.Equal(asc, m.aacSent) {
		if err := m.tag(flvAudio, tsMs, append([]byte{0xaf, 0}, asc...)); err != nil {
			return err
		}
		m.aacSent = bytes.Clone(asc)
	}
	return nil
}

// writeVideo writes one access unit of length-prefixed NAL units.
func (m *flvMuxer) writeVideo(tsMs, ctsMs int64, key bool, au []byte) error {
	frame := byte(0x27)
	if key {
		frame = 0x17
	}
	cts := int32(ctsMs)
	payload := make([]byte, 5, 5+len(au))
	payload[0] = frame
	payload[1] = 1
	payload[2], payload[3], payload[4] = byte(cts>>16), byte(cts>>8), byte(cts)
	return m.tag(flvVideo, tsMs, append(payload, au...))
}

// writeAudio writes one raw AAC frame.
func (m *flvMuxer) writeAudio(tsMs int64, frame []byte) error {
	payload := make([]byte, 2, 2+len(frame))
	payload[0], payload[1] = 0xaf, 1
	return m.tag(flvAudio, tsMs, append(payload, frame...))
}

func (m *flvMuxer) tag(typ byte, tsMs int64, payload []byte) error {
	if !m.headerDone {
		// Signature, version 1, audio and video present, header size 9, and
		// the first PreviousTagSize.
		if _, err := m.w.Write([]byte{'F', 'L', 'V', 1, 5, 0, 0, 0, 9, 0, 0, 0, 0}); err != nil {
			return err
		}
		m.headerDone = true
	}
	ts := uint32(tsMs)
	n := len(payload)
	buf := make([]byte, 11+n+4)
	buf[0] = typ
	buf[1], buf[2], buf[3] = byte(n>>16), byte(n>>8), byte(n)
	buf[4], buf[5], buf[6] = byte(ts>>16), byte(ts>>8), byte(ts)
	buf[7] = byte(ts >> 24)
	copy(buf[11:], payload)
	binary.BigEndian.PutUint32(buf[11+n:], uint32(11+n))
	_, err := m.w.Write(buf)
	return err
}

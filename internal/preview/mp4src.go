package preview

import (
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/Eyevinn/mp4ff/hevc"
	"github.com/Eyevinn/mp4ff/mp4"
)

// ErrUnsupported means a file has no video track Go can index; callers fall
// back to Decoder.DecodeFile.
var ErrUnsupported = errors.New("preview: unsupported file")

// Info describes a source's video.
type Info struct {
	Width, Height int // display size
	Duration      time.Duration
	Codec         string // e.g. "H.264"
	FrameRate     float64
}

// Portrait reports whether the video is taller than it is wide.
func (i Info) Portrait() bool { return i.Height > i.Width }

// MP4 is an indexed progressive MP4 (moov plus mdat): keyframe times come
// from its sample tables, and a keyframe's bytes are read only when asked
// for, so previews of a long file read megabytes, not the whole file.
type MP4 struct {
	f         *os.File
	trak      *mp4.TrakBox
	cfg       Config
	nalLen    int
	timescale uint32
	shift     int64 // composition offset of the first sample (edit-list delay)
	keys      []uint32
	keyPTS    []time.Duration
	info      Info
}

// OpenMP4 indexes path. It returns ErrUnsupported for a fragmented file or
// one without an H.264 or HEVC track.
func OpenMP4(path string) (*MP4, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	m, err := indexMP4(f)
	if err != nil {
		f.Close()
		return nil, err
	}
	return m, nil
}

func indexMP4(f *os.File) (*MP4, error) {
	file, err := mp4.DecodeFile(f, mp4.WithDecodeMode(mp4.DecModeLazyMdat))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnsupported, err)
	}
	if file.Moov == nil || file.IsFragmented() {
		return nil, ErrUnsupported
	}
	for _, trak := range file.Moov.Traks {
		if trak.Mdia == nil || trak.Mdia.Hdlr == nil || trak.Mdia.Hdlr.HandlerType != "vide" {
			continue
		}
		m := &MP4{f: f, trak: trak, timescale: trak.Mdia.Mdhd.Timescale}
		if err := m.readConfig(); err != nil {
			return nil, err
		}
		m.readKeys()
		return m, nil
	}
	return nil, ErrUnsupported
}

func (m *MP4) readConfig() error {
	stbl := m.trak.Mdia.Minf.Stbl
	if stbl == nil || stbl.Stsd == nil || stbl.Stsz == nil || stbl.Stts == nil || m.timescale == 0 {
		return ErrUnsupported
	}
	switch {
	case stbl.Stsd.AvcX != nil && stbl.Stsd.AvcX.AvcC != nil:
		c := stbl.Stsd.AvcX.AvcC
		m.info.Codec = "H.264"
		m.nalLen = 4
		m.cfg = Config{Codec: H264, SPS: c.SPSnalus, PPS: c.PPSnalus}
	case stbl.Stsd.HvcX != nil && stbl.Stsd.HvcX.HvcC != nil:
		c := stbl.Stsd.HvcX.HvcC
		m.info.Codec = "HEVC"
		m.nalLen = int(c.LengthSizeMinusOne) + 1
		m.cfg = Config{
			Codec: HEVC,
			VPS:   c.GetNalusForType(hevc.NALU_VPS),
			SPS:   c.GetNalusForType(hevc.NALU_SPS),
			PPS:   c.GetNalusForType(hevc.NALU_PPS),
		}
	default:
		return ErrUnsupported
	}
	m.info.Width = int(m.trak.Tkhd.Width >> 16)
	m.info.Height = int(m.trak.Tkhd.Height >> 16)
	if m.info.Width == 0 || m.info.Height == 0 {
		if e := stbl.Stsd.AvcX; e != nil {
			m.info.Width, m.info.Height = int(e.Width), int(e.Height)
		} else if e := stbl.Stsd.HvcX; e != nil {
			m.info.Width, m.info.Height = int(e.Width), int(e.Height)
		}
	}
	n := stbl.Stsz.GetNrSamples()
	if n == 0 {
		return ErrUnsupported
	}
	var total uint64
	for i, c := range stbl.Stts.SampleCount {
		total += uint64(c) * uint64(stbl.Stts.SampleTimeDelta[i])
	}
	m.info.Duration = m.dur(int64(total))
	if m.info.Duration > 0 {
		m.info.FrameRate = float64(n) / m.info.Duration.Seconds()
	}
	if stbl.Ctts != nil {
		m.shift = int64(stbl.Ctts.GetCompositionTimeOffset(1))
	}
	return nil
}

func (m *MP4) readKeys() {
	stbl := m.trak.Mdia.Minf.Stbl
	n := stbl.Stsz.GetNrSamples()
	if stbl.Stss == nil {
		// No sync table: every sample is a sync sample.
		m.keys = make([]uint32, n)
		for i := range m.keys {
			m.keys[i] = uint32(i + 1)
		}
	} else {
		m.keys = append([]uint32(nil), stbl.Stss.SampleNumber...)
	}
	m.keyPTS = make([]time.Duration, len(m.keys))
	for i, nr := range m.keys {
		dt, _, err := stbl.Stts.GetDecodeTime(nr)
		if err != nil {
			continue
		}
		pts := int64(dt) - m.shift
		if stbl.Ctts != nil {
			pts += int64(stbl.Ctts.GetCompositionTimeOffset(nr))
		}
		m.keyPTS[i] = m.dur(max(pts, 0))
	}
}

func (m *MP4) dur(units int64) time.Duration {
	return time.Duration(units) * time.Second / time.Duration(m.timescale)
}

// Info describes the file's video track.
func (m *MP4) Info() Info { return m.info }

// Config is the video track's decoder configuration.
func (m *MP4) Config() Config { return m.cfg }

// KeyTimes lists the presentation time of every keyframe, in order.
func (m *MP4) KeyTimes() []time.Duration { return m.keyPTS }

// Keyframe reads keyframe i (an index into KeyTimes).
func (m *MP4) Keyframe(i int) (Keyframe, error) {
	nr := m.keys[i]
	ranges, err := m.trak.GetRangesForSampleInterval(nr, nr)
	if err != nil || len(ranges) != 1 {
		return Keyframe{}, fmt.Errorf("preview: locate sample %d: %v", nr, err)
	}
	r := ranges[0]
	if r.Size > 64<<20 {
		return Keyframe{}, fmt.Errorf("preview: sample %d is %d bytes", nr, r.Size)
	}
	buf := make([]byte, r.Size)
	if _, err := m.f.ReadAt(buf, int64(r.Offset)); err != nil && err != io.EOF {
		return Keyframe{}, err
	}
	sample, err := Relength(buf, m.nalLen)
	if err != nil {
		return Keyframe{}, err
	}
	return Keyframe{PTS: m.keyPTS[i], Sample: sample}, nil
}

// Close releases the file.
func (m *MP4) Close() error { return m.f.Close() }

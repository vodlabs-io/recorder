package preview

import (
	"bytes"
	"encoding/binary"
	"errors"
	"time"

	"github.com/Eyevinn/mp4ff/mp4"
)

// Codec is the video codec of a Keyframe.
type Codec int

const (
	H264 Codec = iota + 1
	HEVC
)

// Config is what a decoder needs before a stream's first picture: its codec
// and parameter sets, as raw NAL units.
type Config struct {
	Codec         Codec
	VPS, SPS, PPS [][]byte
}

// Keyframe is one independently decodable picture.
type Keyframe struct {
	// PTS is the picture's position on the timeline previews are cued on.
	PTS time.Duration
	// Sample is the access unit with 4-byte NAL lengths, as MP4 stores it.
	Sample []byte
}

// Relength rewrites a sample with nalLen-byte NAL lengths (1, 2 or 4, as
// avcC/hvcC allow) into the 4-byte form a Keyframe carries.
func Relength(sample []byte, nalLen int) ([]byte, error) {
	if nalLen == 4 {
		return sample, nil
	}
	if nalLen < 1 || nalLen > 4 {
		return nil, errors.New("preview: bad NAL length size")
	}
	out := make([]byte, 0, len(sample)+len(sample)/16)
	for pos := 0; pos < len(sample); {
		if pos+nalLen > len(sample) {
			return nil, errors.New("preview: truncated NAL length")
		}
		var n int
		for i := range nalLen {
			n = n<<8 | int(sample[pos+i])
		}
		pos += nalLen
		if n <= 0 || pos+n > len(sample) {
			return nil, errors.New("preview: NAL unit overruns sample")
		}
		out = binary.BigEndian.AppendUint32(out, uint32(n))
		out = append(out, sample[pos:pos+n]...)
		pos += n
	}
	return out, nil
}

// wrapMP4 packs keyframes into a one-fragment MP4 a second apart. A raw
// elementary stream would leave ffmpeg without timestamps for pictures from
// a live source, which the JPEG encoder refuses; the container gives each
// one its own.
func wrapMP4(cfg Config, frames []Keyframe) ([]byte, error) {
	const timescale = 1000
	init := mp4.CreateEmptyInit()
	trak := init.AddEmptyTrack(timescale, "video", "und")
	var err error
	switch cfg.Codec {
	case H264:
		err = trak.SetAVCDescriptor("avc1", cfg.SPS, cfg.PPS, true)
	case HEVC:
		err = trak.SetHEVCDescriptor("hvc1", cfg.VPS, cfg.SPS, cfg.PPS, nil, true)
	default:
		err = errors.New("preview: unknown codec")
	}
	if err != nil {
		return nil, err
	}
	frag, err := mp4.CreateFragment(1, trak.Tkhd.TrackID)
	if err != nil {
		return nil, err
	}
	for i, f := range frames {
		frag.AddFullSample(mp4.FullSample{
			Sample:     mp4.Sample{Flags: mp4.SyncSampleFlags, Dur: timescale, Size: uint32(len(f.Sample))},
			DecodeTime: uint64(i) * timescale,
			Data:       f.Sample,
		})
	}
	var buf bytes.Buffer
	if err := init.Encode(&buf); err != nil {
		return nil, err
	}
	if err := frag.Encode(&buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

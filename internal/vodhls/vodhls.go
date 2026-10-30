// Package vodhls plays a recording's MP4 files as one HLS stream. Each file
// is cut into fMP4 segments at its keyframes when a segment is asked for,
// copying samples from the file with no re-encode; a file's moov is read
// once and kept while it is in use.
package vodhls

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"sync"

	"github.com/Eyevinn/mp4ff/mp4"
)

// ErrUnsupported means a file can't be cut into segments: not a progressive
// MP4, or no H.264/HEVC video with AAC, AC-3 or E-AC-3 audio (or none).
var ErrUnsupported = errors.New("vodhls: unsupported file")

// segmentTarget is the shortest a segment runs before the next keyframe
// cuts it, in seconds.
const segmentTarget = 6.0

// Plan is one file's segmentation.
type Plan struct {
	file   *mp4.File
	tracks []*track // video first
	segs   []segment
	init   []byte
	dur    float64
	weight int
}

type track struct {
	trak      *mp4.TrakBox
	id        uint32 // in the init segment
	timescale uint32
	durs      []uint32
	shift     int32 // edit-list start, taken off composition offsets
	video     bool
}

// segment holds, per track, its first sample (1-based), the sample after
// its last, and the first sample's decode time.
type segment struct {
	first, end []uint32
	dts        []uint64
	dur        float64
}

// Durations lists the file's segment lengths in seconds.
func (p *Plan) Durations() []float64 {
	out := make([]float64, len(p.segs))
	for i, s := range p.segs {
		out[i] = s.dur
	}
	return out
}

// Duration is the file's length in seconds.
func (p *Plan) Duration() float64 { return p.dur }

// Init is the file's initialization segment.
func (p *Plan) Init() []byte { return p.init }

// maxSegmentSpan bounds the bytes one segment reads.
const maxSegmentSpan = 512 << 20

// WriteSegment writes segment n, reading its samples from f. The tracks'
// chunks are interleaved in one stretch of the file, read in one go.
func (p *Plan) WriteSegment(w io.Writer, f io.ReaderAt, n int) error {
	if n < 0 || n >= len(p.segs) {
		return fmt.Errorf("vodhls: no segment %d", n)
	}
	s := p.segs[n]
	var ids []uint32
	ranges := make([][]mp4.DataRange, len(p.tracks))
	lo, hi := uint64(math.MaxUint64), uint64(0)
	for i, t := range p.tracks {
		if s.end[i] <= s.first[i] {
			continue
		}
		ids = append(ids, t.id)
		r, err := t.trak.GetRangesForSampleInterval(s.first[i], s.end[i]-1)
		if err != nil {
			return err
		}
		ranges[i] = r
		for _, c := range r {
			lo, hi = min(lo, c.Offset), max(hi, c.Offset+c.Size)
		}
	}
	if hi <= lo || hi-lo > maxSegmentSpan {
		return fmt.Errorf("vodhls: segment %d spans %d bytes", n, hi-lo)
	}
	span := make([]byte, hi-lo)
	if _, err := f.ReadAt(span, int64(lo)); err != nil && err != io.EOF {
		return err
	}
	frag, err := mp4.CreateMultiTrackFragment(uint32(n+1), ids)
	if err != nil {
		return err
	}
	for i, t := range p.tracks {
		first, end := s.first[i], s.end[i]
		if end <= first {
			continue
		}
		var data []byte
		for _, c := range ranges[i] {
			data = append(data, span[c.Offset-lo:c.Offset-lo+c.Size]...)
		}
		stbl := t.trak.Mdia.Minf.Stbl
		dts := s.dts[i]
		var pos uint32
		for nr := first; nr < end; nr++ {
			size := stbl.Stsz.GetSampleSize(int(nr))
			if uint64(pos)+uint64(size) > uint64(len(data)) {
				return fmt.Errorf("vodhls: sample %d overruns its data", nr)
			}
			flags := mp4.SyncSampleFlags
			if t.video && stbl.Stss != nil && !stbl.Stss.IsSyncSample(nr) {
				flags = mp4.NonSyncSampleFlags
			}
			var cto int32
			if stbl.Ctts != nil {
				cto = stbl.Ctts.GetCompositionTimeOffset(nr)
			}
			dur := t.durs[nr-1]
			err := frag.AddFullSampleToTrack(mp4.FullSample{
				Sample:     mp4.Sample{Flags: flags, Dur: dur, Size: size, CompositionTimeOffset: cto - t.shift},
				DecodeTime: dts,
				Data:       data[pos : pos+size],
			}, t.id)
			if err != nil {
				return err
			}
			pos += size
			dts += uint64(dur)
		}
	}
	return frag.Encode(w)
}

// NewPlan reads f's moov and plans its segments.
func NewPlan(f io.ReadSeeker) (*Plan, error) {
	file, err := mp4.DecodeFile(f, mp4.WithDecodeMode(mp4.DecModeLazyMdat))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnsupported, err)
	}
	if file.Moov == nil || file.IsFragmented() {
		return nil, ErrUnsupported
	}
	p := &Plan{file: file}
	var audio *mp4.TrakBox
	for _, trak := range file.Moov.Traks {
		if trak.Mdia == nil || trak.Mdia.Hdlr == nil || trak.Mdia.Minf == nil || trak.Mdia.Minf.Stbl == nil {
			continue
		}
		switch trak.Mdia.Hdlr.HandlerType {
		case "vide":
			if len(p.tracks) == 0 {
				t, err := newTrack(trak, true)
				if err != nil {
					return nil, err
				}
				p.tracks = append(p.tracks, t)
			}
		case "soun":
			if audio == nil {
				audio = trak
			}
		}
	}
	if len(p.tracks) == 0 {
		return nil, ErrUnsupported
	}
	if audio != nil {
		t, err := newTrack(audio, false)
		if err != nil {
			return nil, err
		}
		p.tracks = append(p.tracks, t)
	}
	if err := p.makeInit(); err != nil {
		return nil, err
	}
	p.cut()
	for _, t := range p.tracks {
		p.weight += len(t.durs)
	}
	return p, nil
}

func newTrack(trak *mp4.TrakBox, video bool) (*track, error) {
	stbl := trak.Mdia.Minf.Stbl
	sd := stbl.Stsd
	switch {
	case sd == nil || stbl.Stsz == nil || stbl.Stts == nil || stbl.Stsc == nil || trak.Mdia.Mdhd == nil:
		return nil, ErrUnsupported
	case video && sd.AvcX == nil && sd.HvcX == nil:
		return nil, ErrUnsupported
	case !video && sd.Mp4a == nil && sd.AC3 == nil && sd.EC3 == nil:
		return nil, ErrUnsupported
	}
	if err := trak.CheckDataIsSelfContained(); err != nil {
		return nil, ErrUnsupported
	}
	n := stbl.Stsz.GetNrSamples()
	if n == 0 {
		return nil, ErrUnsupported
	}
	durs, err := stbl.Stts.SampleDurations(n)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnsupported, err)
	}
	t := &track{trak: trak, timescale: trak.Mdia.Mdhd.Timescale, durs: durs, video: video}
	if t.timescale == 0 {
		return nil, ErrUnsupported
	}
	// A video edit list usually starts at the first frame's composition
	// offset; taking it off keeps the picture in step with the audio.
	if video && trak.Edts != nil && len(trak.Edts.Elst) == 1 {
		for _, e := range trak.Edts.Elst[0].Entries {
			if e.MediaTime >= 0 {
				if e.MediaTime <= 1<<30 {
					t.shift = int32(e.MediaTime)
				}
				break
			}
		}
	}
	return t, nil
}

func (p *Plan) makeInit() error {
	init := mp4.CreateEmptyInit()
	for _, t := range p.tracks {
		kind := "audio"
		if t.video {
			kind = "video"
		}
		out := init.AddEmptyTrack(t.timescale, kind, "und")
		t.id = out.Tkhd.TrackID
		sd := t.trak.Mdia.Minf.Stbl.Stsd
		switch {
		case sd.AvcX != nil:
			out.Mdia.Minf.Stbl.Stsd.AddChild(sd.AvcX)
		case sd.HvcX != nil:
			out.Mdia.Minf.Stbl.Stsd.AddChild(sd.HvcX)
		case sd.Mp4a != nil:
			out.Mdia.Minf.Stbl.Stsd.AddChild(sd.Mp4a)
		case sd.AC3 != nil:
			out.Mdia.Minf.Stbl.Stsd.AddChild(sd.AC3)
		case sd.EC3 != nil:
			out.Mdia.Minf.Stbl.Stsd.AddChild(sd.EC3)
		}
		if t.video && t.trak.Tkhd != nil {
			out.Tkhd.Width, out.Tkhd.Height = t.trak.Tkhd.Width, t.trak.Tkhd.Height
		}
	}
	ftyp, err := init.GenerateFtyp(mp4.FtypOptions{})
	if err != nil {
		return err
	}
	init.SetFtyp(ftyp)
	var buf bytes.Buffer
	if err := init.Encode(&buf); err != nil {
		return err
	}
	p.init = buf.Bytes()
	return nil
}

// cut splits the file at the first keyframe at least segmentTarget after
// the last cut. Other tracks follow the video: each sample goes in the
// segment its decode time falls in.
func (p *Plan) cut() {
	v := p.tracks[0]
	stss := v.trak.Mdia.Minf.Stbl.Stss
	k := len(p.tracks)
	cursor := make([]uint32, k) // next sample per track, 1-based
	dts := make([]uint64, k)
	for i := range cursor {
		cursor[i] = 1
	}
	open := func() segment {
		return segment{
			first: append([]uint32(nil), cursor...),
			dts:   append([]uint64(nil), dts...),
		}
	}
	cur := open()
	vts := float64(v.timescale)
	n := uint32(len(v.durs))
	for nr := uint32(1); nr <= n; nr++ {
		if nr > cur.first[0] && (stss == nil || stss.IsSyncSample(nr)) && float64(dts[0]-cur.dts[0])/vts >= segmentTarget {
			at := float64(dts[0]) / vts
			for i := 1; i < k; i++ {
				t := p.tracks[i]
				for int(cursor[i]) <= len(t.durs) && float64(dts[i])/float64(t.timescale) < at {
					dts[i] += uint64(t.durs[cursor[i]-1])
					cursor[i]++
				}
			}
			cursor[0] = nr
			cur.end = append([]uint32(nil), cursor...)
			cur.dur = float64(dts[0]-cur.dts[0]) / vts
			p.segs = append(p.segs, cur)
			cur = open()
		}
		dts[0] += uint64(v.durs[nr-1])
	}
	for i, t := range p.tracks {
		cursor[i] = uint32(len(t.durs)) + 1
	}
	cur.end = cursor
	cur.dur = float64(dts[0]-cur.dts[0]) / vts
	p.segs = append(p.segs, cur)
	p.dur = float64(dts[0]) / vts
}

// Cache keeps the plans of recently played files, bounded by their total
// sample count (each sample costs a few bytes of moov).
type Cache struct {
	mu     sync.Mutex
	max    int
	weight int
	order  []string // least recently used first
	plans  map[string]*Plan
}

// NewCache keeps plans of up to maxSamples samples in all.
func NewCache(maxSamples int) *Cache {
	return &Cache{max: maxSamples, plans: map[string]*Plan{}}
}

// Plan returns the plan cached under key, or plans f and caches it. A key
// names one version of one file, so a changed file needs a new key.
func (c *Cache) Plan(key string, f *os.File) (*Plan, error) {
	c.mu.Lock()
	if p, ok := c.plans[key]; ok {
		c.touch(key)
		c.mu.Unlock()
		return p, nil
	}
	c.mu.Unlock()
	p, err := NewPlan(f)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.plans[key]; !ok {
		c.plans[key] = p
		c.weight += p.weight
		c.order = append(c.order, key)
	}
	for c.weight > c.max && len(c.order) > 1 {
		old := c.order[0]
		c.order = c.order[1:]
		c.weight -= c.plans[old].weight
		delete(c.plans, old)
	}
	return p, nil
}

func (c *Cache) touch(key string) {
	for i, k := range c.order {
		if k == key {
			c.order = append(append(c.order[:i:i], c.order[i+1:]...), key)
			return
		}
	}
}

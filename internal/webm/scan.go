package webm

import (
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
)

// Element IDs the scan reads beyond those the Remuxer writes.
const (
	mkvBlockGroup = 0xA0
	mkvBlock      = 0xA1
)

// Video describes the VP8 track of a WebM file as its blocks show it.
type Video struct {
	// Width and Height are the largest key frame's size. A VP8 stream may
	// change size at any key frame, and the track header names only the size
	// it started at.
	Width, Height int
	// FrameRate is the frame rate the usual gap between frames gives,
	// snapped to a standard rate within 3%; 0 when too few frames tell. A
	// browser broadcaster's frames come at a varying rate on a millisecond
	// clock, from which ffmpeg cannot guess one.
	FrameRate float64
}

// Scan walks a WebM file block by block for its VP8 track's Video; only
// element headers and the first bytes of each video block are read.
func Scan(path string) (Video, error) {
	f, err := os.Open(path)
	if err != nil {
		return Video{}, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return Video{}, err
	}
	s := &scanner{r: f, end: fi.Size(), videoTrack: -1, timecodeScale: 1_000_000, lastMs: -1, gaps: map[int64]int{}}
	if err := s.walk(0, fi.Size()); err != nil {
		return Video{}, err
	}
	if s.width == 0 {
		return Video{}, errors.New("webm: no VP8 key frame")
	}
	return Video{Width: s.width, Height: s.height, FrameRate: s.frameRate()}, nil
}

type scanner struct {
	r             io.ReaderAt
	end           int64
	videoTrack    int64 // track number of the V_VP8 track, -1 until Tracks is read
	width, height int

	// The TrackEntry being read.
	entryNumber int64
	entryCodec  string

	timecodeScale int64 // nanoseconds per timecode tick
	clusterTC     int64
	lastMs        int64         // the previous video block's time, -1 before the first
	gaps          map[int64]int // count of each gap between video blocks, in ms
}

// standardRates are the frame rates a measured one snaps to.
var standardRates = []float64{10, 12, 15, 20, 24, 25, 30, 48, 50, 60}

func (s *scanner) frameRate() float64 {
	var gaps []int64
	n := 0
	for g, c := range s.gaps {
		gaps = append(gaps, g)
		n += c
	}
	if n < 10 {
		return 0
	}
	// The mean of the middle half of the gaps: a millisecond clock rounds a
	// 33.3 ms frame to 33 or 34, which a median cannot average, and stalls
	// and bursts fall in the outer quarters.
	sort.Slice(gaps, func(i, j int) bool { return gaps[i] < gaps[j] })
	lo, hi := n/4, n-n/4
	var sum, count int64
	i := 0
	for _, g := range gaps {
		for c := 0; c < s.gaps[g]; c++ {
			if i >= lo && i < hi {
				sum += g
				count++
			}
			i++
		}
	}
	fps := 1000 * float64(count) / float64(sum)
	for _, r := range standardRates {
		if math.Abs(fps-r)/r <= 0.03 {
			return r
		}
	}
	return math.Round(fps*100) / 100
}

// walk reads the elements in [off, end). The masters that lead to tracks and
// blocks are entered in place rather than recursed into, so a live file's
// unknown-size Clusters, each ending only where the next begins, read as the
// flat sequence they are.
func (s *scanner) walk(off, end int64) error {
	for off < end {
		id, size, n, err := s.header(off)
		if err != nil {
			if errors.Is(err, io.ErrUnexpectedEOF) {
				return nil // a file still being written ends mid-element
			}
			return err
		}
		off += n
		switch id {
		case mkvSegment, mkvInfo, mkvCluster, mkvTracks, mkvBlockGroup:
			continue
		}
		dataEnd := off + size
		if size < 0 || dataEnd > end {
			dataEnd = end
		}
		switch id {
		case mkvTrackEntry:
			s.entryNumber, s.entryCodec = 0, ""
			if err := s.walk(off, dataEnd); err != nil {
				return err
			}
			if s.entryCodec == "V_VP8" && s.videoTrack < 0 {
				s.videoTrack = s.entryNumber
			}
		case mkvTrackNumber:
			b, err := s.read(off, min(dataEnd-off, 8))
			if err != nil {
				return err
			}
			s.entryNumber = int64(beUint(b))
		case mkvCodecID:
			b, err := s.read(off, min(dataEnd-off, 32))
			if err != nil {
				return err
			}
			s.entryCodec = string(b)
		case mkvTimecodeScale, mkvClusterTimecode:
			b, err := s.read(off, min(dataEnd-off, 8))
			if err != nil {
				return err
			}
			if id == mkvTimecodeScale {
				s.timecodeScale = max(int64(beUint(b)), 1)
			} else {
				s.clusterTC = int64(beUint(b))
			}
		case mkvSimpleBlock, mkvBlock:
			s.block(off, dataEnd)
		}
		off = dataEnd
	}
	return nil
}

// block takes a block of the VP8 track: its time, and its frame size when it
// holds a key frame.
func (s *scanner) block(off, end int64) {
	if s.videoTrack < 0 {
		return
	}
	b, err := s.read(off, min(end-off, 32))
	if err != nil || len(b) == 0 {
		return
	}
	track, n := vint(b)
	if n == 0 || int64(track) != s.videoTrack || len(b) < n+3 {
		return
	}
	rel := int64(int16(uint16(b[n])<<8 | uint16(b[n+1])))
	ms := (s.clusterTC + rel) * s.timecodeScale / 1_000_000
	if s.lastMs >= 0 && ms > s.lastMs && ms-s.lastMs <= 1000 {
		s.gaps[ms-s.lastMs]++
	}
	s.lastMs = ms
	if w, h, ok := vp8Dimensions(b[n+3:]); ok && w*h > s.width*s.height {
		s.width, s.height = w, h
	}
}

// header reads an element ID and data size at off. size is -1 for an
// unknown-size element; n is the header's length.
func (s *scanner) header(off int64) (id uint32, size int64, n int64, err error) {
	b, err := s.read(off, 12)
	if err != nil {
		return 0, 0, 0, err
	}
	idLen := 1
	for m := byte(0x80); idLen <= 4 && b[0]&m == 0; m >>= 1 {
		idLen++
	}
	if idLen > 4 || len(b) < idLen+1 {
		return 0, 0, 0, fmt.Errorf("webm: bad element at %d", off)
	}
	for _, c := range b[:idLen] {
		id = id<<8 | uint32(c)
	}
	v, szLen := vint(b[idLen:])
	if szLen == 0 {
		return 0, 0, 0, io.ErrUnexpectedEOF
	}
	size = int64(v)
	if v == 1<<(7*szLen)-1 {
		size = -1 // all value bits set: unknown size
	}
	return id, size, int64(idLen + szLen), nil
}

// read returns up to n bytes at off, fewer at the end of the file;
// io.ErrUnexpectedEOF when there are none.
func (s *scanner) read(off, n int64) ([]byte, error) {
	n = min(n, s.end-off)
	if n <= 0 {
		return nil, io.ErrUnexpectedEOF
	}
	b := make([]byte, n)
	got, err := s.r.ReadAt(b, off)
	if got == 0 && err != nil {
		return nil, err
	}
	return b[:got], nil
}

// vint decodes an EBML variable-length integer, marker bit removed. n is 0
// when b is too short.
func vint(b []byte) (v uint64, n int) {
	if len(b) == 0 || b[0] == 0 {
		return 0, 0
	}
	n = 1
	for m := byte(0x80); b[0]&m == 0; m >>= 1 {
		n++
	}
	if len(b) < n {
		return 0, 0
	}
	v = uint64(b[0]) & (0xff >> n)
	for _, c := range b[1:n] {
		v = v<<8 | uint64(c)
	}
	return v, n
}

func beUint(b []byte) uint64 {
	var v uint64
	for _, c := range b {
		v = v<<8 | uint64(c)
	}
	return v
}

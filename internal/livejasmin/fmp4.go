package livejasmin

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// maxBox bounds one box read whole (moov, moof, mdat). The stream sends one
// sample per fragment, so an mdat is one frame; the cap only stops a corrupt
// size from allocating without bound.
const maxBox = 32 << 20

// sample is one access unit of the stream.
type sample struct {
	video bool
	dts   int64 // decode time, ms from the stream's start
	cts   int64 // composition offset, ms (video only)
	key   bool
	data  []byte
	// avcC and asc are the stream's codec configuration as of this sample.
	avcC, asc []byte
}

// track is what the init segment says about one track.
type track struct {
	video     bool
	timescale uint32
	// config is the AVCDecoderConfigurationRecord (video) or the
	// AudioSpecificConfig (audio).
	config []byte
	// trex defaults.
	defDur, defSize, defFlags uint32
	// decoded is the decode time reached so far, in timescale units, for a
	// fragment that carries no tfdt.
	decoded uint64
}

// demuxer reads a progressive fragmented MP4 stream: an init segment (ftyp,
// moov), then moof/mdat pairs.
type demuxer struct {
	r      *bufio.Reader
	pos    int64 // stream offset of the next unread byte
	tracks map[uint32]*track
	// pending are the samples of the last moof, waiting for their mdat.
	pending []pendingSample
	moofPos int64
	out     []sample
}

type pendingSample struct {
	trk    *track
	offset int64 // absolute stream offset of the sample's data
	size   uint32
	dts    uint64 // timescale units
	cts    int32
	key    bool
}

func newDemuxer(r io.Reader) *demuxer {
	return &demuxer{r: bufio.NewReaderSize(r, 64<<10), tracks: map[uint32]*track{}}
}

// videoConfig and audioConfig return the codec configuration the init
// segment declared, nil before it has been read.
func (d *demuxer) videoConfig() []byte { return d.config(true) }
func (d *demuxer) audioConfig() []byte { return d.config(false) }

func (d *demuxer) config(video bool) []byte {
	for _, t := range d.tracks {
		if t.video == video {
			return t.config
		}
	}
	return nil
}

// next returns the stream's next sample, in the order the stream carries
// them.
func (d *demuxer) next() (sample, error) {
	for len(d.out) == 0 {
		if err := d.readBox(); err != nil {
			return sample{}, err
		}
	}
	s := d.out[0]
	d.out = d.out[1:]
	return s, nil
}

func (d *demuxer) readBox() error {
	start := d.pos
	var hdr [8]byte
	if _, err := io.ReadFull(d.r, hdr[:]); err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) {
			err = io.EOF
		}
		return err
	}
	d.pos += 8
	size := uint64(binary.BigEndian.Uint32(hdr[:4]))
	typ := string(hdr[4:8])
	hdrLen := uint64(8)
	if size == 1 {
		var ext [8]byte
		if _, err := io.ReadFull(d.r, ext[:]); err != nil {
			return io.EOF
		}
		d.pos += 8
		size = binary.BigEndian.Uint64(ext[:])
		hdrLen = 16
	}
	if size < hdrLen {
		return fmt.Errorf("fmp4: box %q has size %d", typ, size)
	}
	bodyLen := size - hdrLen
	switch typ {
	case "moov", "moof", "mdat":
	default:
		n, err := io.CopyN(io.Discard, d.r, int64(bodyLen))
		d.pos += n
		if err != nil {
			return io.EOF
		}
		return nil
	}
	if bodyLen > maxBox {
		return fmt.Errorf("fmp4: %s of %d bytes", typ, bodyLen)
	}
	body := make([]byte, bodyLen)
	if _, err := io.ReadFull(d.r, body); err != nil {
		return io.EOF
	}
	d.pos += int64(bodyLen)
	switch typ {
	case "moov":
		return d.parseMoov(body)
	case "moof":
		d.moofPos = start
		return d.parseMoof(body)
	default:
		d.takeMdat(start+int64(hdrLen), body)
		return nil
	}
}

// boxes walks the boxes in b, calling fn with each one's type and body.
func boxes(b []byte, fn func(typ string, body []byte) error) error {
	for len(b) >= 8 {
		size := uint64(binary.BigEndian.Uint32(b))
		typ := string(b[4:8])
		hdr := uint64(8)
		if size == 1 {
			if len(b) < 16 {
				return errors.New("fmp4: truncated box header")
			}
			size, hdr = binary.BigEndian.Uint64(b[8:]), 16
		} else if size == 0 {
			size = uint64(len(b))
		}
		if size < hdr || size > uint64(len(b)) {
			return fmt.Errorf("fmp4: box %q overruns its parent", typ)
		}
		if err := fn(typ, b[hdr:size]); err != nil {
			return err
		}
		b = b[size:]
	}
	return nil
}

// child returns the body of the first box of type typ in b.
func child(b []byte, typ string) []byte {
	var found []byte
	_ = boxes(b, func(t string, body []byte) error {
		if t == typ && found == nil {
			found = body
		}
		return nil
	})
	return found
}

func (d *demuxer) parseMoov(b []byte) error {
	tracks := map[uint32]*track{}
	err := boxes(b, func(typ string, body []byte) error {
		if typ != "trak" {
			return nil
		}
		tkhd := child(body, "tkhd")
		mdia := child(body, "mdia")
		if len(tkhd) < 4 || mdia == nil {
			return nil
		}
		idOff := 12
		if tkhd[0] == 1 {
			idOff = 20
		}
		if len(tkhd) < idOff+4 {
			return nil
		}
		id := binary.BigEndian.Uint32(tkhd[idOff:])
		hdlr := child(mdia, "hdlr")
		mdhd := child(mdia, "mdhd")
		if len(hdlr) < 12 || len(mdhd) < 4 {
			return nil
		}
		t := &track{}
		switch string(hdlr[8:12]) {
		case "vide":
			t.video = true
		case "soun":
		default:
			return nil
		}
		tsOff := 12
		if mdhd[0] == 1 {
			tsOff = 20
		}
		if len(mdhd) < tsOff+4 {
			return nil
		}
		t.timescale = binary.BigEndian.Uint32(mdhd[tsOff:])
		stsd := child(child(child(mdia, "minf"), "stbl"), "stsd")
		t.config = sampleConfig(stsd, t.video)
		if t.timescale == 0 || t.config == nil {
			return fmt.Errorf("fmp4: track %d declares no usable codec", id)
		}
		tracks[id] = t
		return nil
	})
	if err != nil {
		return err
	}
	if trex := child(b, "mvex"); trex != nil {
		_ = boxes(trex, func(typ string, body []byte) error {
			if typ != "trex" || len(body) < 24 {
				return nil
			}
			if t := tracks[binary.BigEndian.Uint32(body[4:])]; t != nil {
				t.defDur = binary.BigEndian.Uint32(body[12:])
				t.defSize = binary.BigEndian.Uint32(body[16:])
				t.defFlags = binary.BigEndian.Uint32(body[20:])
			}
			return nil
		})
	}
	if len(tracks) == 0 {
		return errors.New("fmp4: init segment has no audio or video track")
	}
	d.tracks = tracks
	return nil
}

// sampleConfig takes the codec configuration out of a sample description:
// the avcC of an avc1/avc3 entry, or the AudioSpecificConfig in an mp4a
// entry's esds.
func sampleConfig(stsd []byte, video bool) []byte {
	if len(stsd) < 8 {
		return nil
	}
	var cfg []byte
	_ = boxes(stsd[8:], func(typ string, entry []byte) error {
		if cfg != nil {
			return nil
		}
		switch {
		case video && (typ == "avc1" || typ == "avc3") && len(entry) > 78:
			cfg = child(entry[78:], "avcC")
		case !video && typ == "mp4a" && len(entry) > 28:
			if esds := child(entry[28:], "esds"); len(esds) > 4 {
				cfg = audioSpecificConfig(esds[4:])
			}
		}
		return nil
	})
	return cfg
}

// audioSpecificConfig finds the DecoderSpecificInfo (tag 5) in an ES
// descriptor.
func audioSpecificConfig(b []byte) []byte {
	for len(b) >= 2 {
		tag := b[0]
		b = b[1:]
		n := 0
		for i := 0; i < 4 && len(b) > 0; i++ {
			c := b[0]
			b = b[1:]
			n = n<<7 | int(c&0x7f)
			if c&0x80 == 0 {
				break
			}
		}
		if n > len(b) {
			return nil
		}
		body := b[:n]
		switch tag {
		case 0x03: // ES_Descriptor: ES_ID, flags, then optional fields.
			if len(body) < 3 {
				return nil
			}
			flags := body[2]
			body = body[3:]
			if flags&0x80 != 0 && len(body) >= 2 {
				body = body[2:]
			}
			if flags&0x40 != 0 && len(body) >= 1 {
				body = body[1+int(body[0]):]
			}
			if flags&0x20 != 0 && len(body) >= 2 {
				body = body[2:]
			}
			return audioSpecificConfig(body)
		case 0x04: // DecoderConfigDescriptor: 13 bytes, then its descriptors.
			if len(body) < 13 {
				return nil
			}
			return audioSpecificConfig(body[13:])
		case 0x05:
			return append([]byte(nil), body...)
		}
		b = b[n:]
	}
	return nil
}

// tfhd flags.
const (
	tfBaseDataOffset    = 0x1
	tfSampleDescIndex   = 0x2
	tfDefaultDuration   = 0x8
	tfDefaultSize       = 0x10
	tfDefaultFlags      = 0x20
	tfDefaultBaseIsMoof = 0x20000
)

// trun flags.
const (
	trDataOffset       = 0x1
	trFirstSampleFlags = 0x4
	trDuration         = 0x100
	trSize             = 0x200
	trFlags            = 0x400
	trCTS              = 0x800
)

// sampleNonSync marks a sample that is not a random access point.
const sampleNonSync = 0x10000

func (d *demuxer) parseMoof(b []byte) error {
	d.pending = d.pending[:0]
	return boxes(b, func(typ string, traf []byte) error {
		if typ != "traf" {
			return nil
		}
		tfhd := child(traf, "tfhd")
		if len(tfhd) < 8 {
			return nil
		}
		flags := binary.BigEndian.Uint32(tfhd) & 0xffffff
		t := d.tracks[binary.BigEndian.Uint32(tfhd[4:])]
		if t == nil {
			return nil
		}
		p := tfhd[8:]
		base := d.moofPos
		dur, size, sflags := t.defDur, t.defSize, t.defFlags
		take := func() (uint32, bool) {
			if len(p) < 4 {
				return 0, false
			}
			v := binary.BigEndian.Uint32(p)
			p = p[4:]
			return v, true
		}
		if flags&tfBaseDataOffset != 0 {
			if len(p) < 8 {
				return errors.New("fmp4: truncated tfhd")
			}
			base = int64(binary.BigEndian.Uint64(p))
			p = p[8:]
		}
		if flags&tfSampleDescIndex != 0 {
			take()
		}
		if flags&tfDefaultDuration != 0 {
			dur, _ = take()
		}
		if flags&tfDefaultSize != 0 {
			size, _ = take()
		}
		if flags&tfDefaultFlags != 0 {
			sflags, _ = take()
		}
		if tfdt := child(traf, "tfdt"); len(tfdt) >= 8 {
			if tfdt[0] == 1 && len(tfdt) >= 12 {
				t.decoded = binary.BigEndian.Uint64(tfdt[4:])
			} else {
				t.decoded = uint64(binary.BigEndian.Uint32(tfdt[4:]))
			}
		}
		offset := base
		return boxes(traf, func(typ string, trun []byte) error {
			if typ != "trun" || len(trun) < 8 {
				return nil
			}
			ver := trun[0]
			tf := binary.BigEndian.Uint32(trun) & 0xffffff
			count := binary.BigEndian.Uint32(trun[4:])
			p := trun[8:]
			next := func() uint32 {
				if len(p) < 4 {
					return 0
				}
				v := binary.BigEndian.Uint32(p)
				p = p[4:]
				return v
			}
			if tf&trDataOffset != 0 {
				offset = base + int64(int32(next()))
			}
			first, hasFirst := uint32(0), tf&trFirstSampleFlags != 0
			if hasFirst {
				first = next()
			}
			for i := uint32(0); i < count; i++ {
				sd, ss, sf, sc := dur, size, sflags, int32(0)
				if tf&trDuration != 0 {
					sd = next()
				}
				if tf&trSize != 0 {
					ss = next()
				}
				if tf&trFlags != 0 {
					sf = next()
				} else if i == 0 && hasFirst {
					sf = first
				}
				if tf&trCTS != 0 {
					v := next()
					if ver == 0 {
						sc = int32(min(v, 1<<31-1))
					} else {
						sc = int32(v)
					}
				}
				d.pending = append(d.pending, pendingSample{
					trk: t, offset: offset, size: ss, dts: t.decoded, cts: sc,
					key: sf&sampleNonSync == 0,
				})
				offset += int64(ss)
				t.decoded += uint64(sd)
			}
			return nil
		})
	})
}

// takeMdat slices the pending samples out of an mdat whose payload starts at
// stream offset at.
func (d *demuxer) takeMdat(at int64, body []byte) {
	for _, ps := range d.pending {
		lo := ps.offset - at
		hi := lo + int64(ps.size)
		if lo < 0 || hi > int64(len(body)) || ps.size == 0 {
			continue
		}
		t := ps.trk
		s := sample{
			video: t.video,
			dts:   int64(ps.dts * 1000 / uint64(t.timescale)),
			data:  body[lo:hi],
			key:   !t.video || ps.key,
		}
		if t.video {
			s.cts = int64(ps.cts) * 1000 / int64(t.timescale)
			// The sample flags of some encoders mark every frame a sync
			// sample; an IDR slice is what a decoder can start from.
			s.key = hasIDR(s.data, nalLengthSize(t.config))
		}
		d.out = append(d.out, s)
	}
	d.pending = d.pending[:0]
}

// nalLengthSize reads the NAL length prefix size from an
// AVCDecoderConfigurationRecord.
func nalLengthSize(avcC []byte) int {
	if len(avcC) < 5 {
		return 4
	}
	return int(avcC[4]&3) + 1
}

// hasIDR reports whether a length-prefixed access unit holds an IDR slice.
func hasIDR(au []byte, lenSize int) bool {
	for len(au) > lenSize {
		n := 0
		for i := 0; i < lenSize; i++ {
			n = n<<8 | int(au[i])
		}
		au = au[lenSize:]
		if n <= 0 || n > len(au) {
			return false
		}
		if au[0]&0x1f == 5 {
			return true
		}
		au = au[n:]
	}
	return false
}

package flvfix

// Low-level FLV tag constants and payload inspection helpers. Only the minimum
// needed to sanitize a stream is parsed: tag framing, timestamps, keyframe and
// sequence-header (codec config) detection for AVC-style video and AAC audio.

const (
	tagAudio  = 8
	tagVideo  = 9
	tagScript = 18

	// tagHeaderLen is the fixed FLV tag header size; each tag is followed by a
	// 4-byte PreviousTagSize field equal to tagHeaderLen+dataSize.
	tagHeaderLen  = 11
	prevSizeLen   = 4
	fileHeaderLen = 9 // canonical FLV file header (before PreviousTagSize0)
)

func be16(b []byte) int { return int(b[0])<<8 | int(b[1]) }

func be24(b []byte) int { return int(b[0])<<16 | int(b[1])<<8 | int(b[2]) }

func be32(b []byte) uint32 {
	return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
}

// tagTimestamp decodes the FLV tag timestamp: 24-bit value + 8-bit extension
// forming a signed 32-bit millisecond timestamp per the FLV spec.
func tagTimestamp(hdr []byte) int64 {
	return int64(int32(uint32(be24(hdr[4:7])) | uint32(hdr[7])<<24))
}

// putTagTimestamp encodes ts back into a tag header (24-bit + extension byte).
func putTagTimestamp(hdr []byte, ts int64) {
	if ts < 0 {
		ts = 0
	}
	hdr[4] = byte(ts >> 16)
	hdr[5] = byte(ts >> 8)
	hdr[6] = byte(ts)
	hdr[7] = byte(ts >> 24)
}

// isFileHeader reports whether b begins with an FLV file-header signature.
func isFileHeader(b []byte) bool {
	return len(b) >= 4 && b[0] == 'F' && b[1] == 'L' && b[2] == 'V' && b[3] == 0x01
}

// videoInfo classifies a video tag payload.
type videoInfo struct {
	keyframe bool
	config   bool // codec sequence header (decoder configuration)
	endOfSeq bool // AVC end-of-sequence marker
}

// parseVideoInfo inspects a video tag payload. Handles the classic FLV layout
// (AVC codec id 7 and the AVC-like unofficial HEVC id 12) and the enhanced-FLV
// ex-header layout (IsExHeader bit set, fourCC-based).
func parseVideoInfo(p []byte) videoInfo {
	var vi videoInfo
	if len(p) == 0 {
		return vi
	}
	first := p[0]
	if first&0x80 != 0 {
		// Enhanced FLV: frameType in bits 4-6, packetType in low nibble.
		vi.keyframe = (first>>4)&0x07 == 1
		pktType := first & 0x0f
		vi.config = pktType == 0   // PacketTypeSequenceStart
		vi.endOfSeq = pktType == 2 // PacketTypeSequenceEnd
		return vi
	}
	vi.keyframe = first>>4 == 1
	codec := first & 0x0f
	if (codec == 7 || codec == 12) && len(p) >= 2 {
		vi.config = p[1] == 0
		vi.endOfSeq = p[1] == 2
	}
	return vi
}

// compositionTimeOffset returns where a video tag payload keeps its composition
// time (presentation minus decode time, SI24 milliseconds), or -1 when it has
// none: sequence headers, end-of-sequence markers, enhanced CodedFramesX, and
// codecs whose frames are never reordered.
func compositionTimeOffset(p []byte) int {
	if len(p) == 0 {
		return -1
	}
	if p[0]&0x80 != 0 {
		// Enhanced FLV: only AVC and HEVC CodedFrames carry one, after the fourCC.
		if p[0]&0x0f != 1 || len(p) < 8 {
			return -1
		}
		switch string(p[1:5]) {
		case "avc1", "hvc1":
			return 5
		}
		return -1
	}
	codec := p[0] & 0x0f
	if (codec == 7 || codec == 12) && len(p) >= 5 && p[1] == 1 {
		return 2
	}
	return -1
}

func si24(b []byte) int32 {
	return int32(uint32(b[0])<<24|uint32(b[1])<<16|uint32(b[2])<<8) >> 8
}

func putSI24(b []byte, v int32) {
	b[0] = byte(v >> 16)
	b[1] = byte(v >> 8)
	b[2] = byte(v)
}

// videoConfigIdentity returns the comparison form of a video sequence-header
// payload: a copy with H.264 level bytes masked out (avcC level_indication and
// each SPS level_idc), because a level-only change is stream-copy safe and must
// not trigger an output rotation. Non-AVC payloads are compared verbatim.
func videoConfigIdentity(p []byte) []byte {
	id := make([]byte, len(p))
	copy(id, p)
	if len(p) == 0 || p[0]&0x80 != 0 || p[0]&0x0f != 7 {
		return id
	}
	// Classic AVC layout: [0]=frame/codec [1]=pktType [2:5]=CTS [5:]=avcC.
	avcc := id[5:]
	if len(avcc) < 8 {
		return id
	}
	avcc[3] = 0 // AVCLevelIndication
	numSPS := int(avcc[5] & 0x1f)
	p2 := 6
	for i := 0; i < numSPS; i++ {
		if p2+2 > len(avcc) {
			return id
		}
		l := be16(avcc[p2 : p2+2])
		p2 += 2
		if p2+l > len(avcc) {
			return id
		}
		if l >= 4 {
			avcc[p2+3] = 0 // SPS level_idc
		}
		p2 += l
	}
	return id
}

// audioConfigInfo reports whether an audio tag payload is a sequence header
// (an AAC AudioSpecificConfig, or an enhanced-FLV SequenceStart such as an
// Opus identification header), and whether the format requires one before
// raw frames.
func audioConfigInfo(p []byte) (isConfig, needsConfig bool) {
	if len(p) == 0 {
		return false, false
	}
	switch p[0] >> 4 {
	case 10: // AAC
		return len(p) >= 2 && p[1] == 0, true
	case 9: // enhanced FLV: AudioPacketType in the low nibble, 0 = SequenceStart
		return p[0]&0x0f == 0, true
	}
	return false, false
}

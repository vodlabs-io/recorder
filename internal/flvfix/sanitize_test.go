package flvfix

import (
	"bytes"
	"errors"
	"math/rand"
	"testing"
)

// ---- synthetic FLV builders ----

func fileHeader() []byte {
	return []byte{'F', 'L', 'V', 0x01, 0x05, 0, 0, 0, 9, 0, 0, 0, 0}
}

func buildTag(typ byte, ts int64, payload []byte) []byte {
	size := len(payload)
	b := make([]byte, 0, tagHeaderLen+size+prevSizeLen)
	b = append(b, typ,
		byte(size>>16), byte(size>>8), byte(size),
		byte(ts>>16), byte(ts>>8), byte(ts), byte(ts>>24),
		0, 0, 0)
	b = append(b, payload...)
	prev := uint32(tagHeaderLen + size)
	b = append(b, byte(prev>>24), byte(prev>>16), byte(prev>>8), byte(prev))
	return b
}

// avcSeqHdr builds a minimal AVC sequence header. res selects the SPS content
// byte (stands in for different resolutions); level sets both level bytes.
func avcSeqHdr(res, level byte) []byte {
	sps := []byte{0x67, 100, 0, level, 0xac, res, 0x40, 0x51, 0xa6}
	pps := []byte{0x68, 0xeb, 0xec, 0xb2}
	p := []byte{0x17, 0x00, 0, 0, 0, // keyframe|AVC, seq header, CTS=0
		0x01, 100, 0, level, 0xff, 0xe1, // avcC: ver, profile, compat, level, nalsize, numSPS
		byte(len(sps) >> 8), byte(len(sps))}
	p = append(p, sps...)
	p = append(p, 0x01, byte(len(pps)>>8), byte(len(pps)))
	p = append(p, pps...)
	return p
}

func aacSeqHdr() []byte { return []byte{0xaf, 0x00, 0x12, 0x10} }

func vKey(ts int64) []byte {
	return buildTag(tagVideo, ts, []byte{0x17, 0x01, 0, 0, 0, 0xde, 0xad, 0xbe, 0xef})
}

func vInter(ts int64) []byte {
	return buildTag(tagVideo, ts, []byte{0x27, 0x01, 0, 0, 0, 0xca, 0xfe})
}

func aRaw(ts int64) []byte {
	return buildTag(tagAudio, ts, []byte{0xaf, 0x01, 0x21, 0x42})
}

func vCfgTag(ts int64, res, level byte) []byte { return buildTag(tagVideo, ts, avcSeqHdr(res, level)) }

func aCfgTag(ts int64) []byte { return buildTag(tagAudio, ts, aacSeqHdr()) }

func scriptTag() []byte {
	return buildTag(tagScript, 0, append([]byte{0x02, 0x00, 0x0a}, []byte("onMetaData")...))
}

// stream assembles a source byte stream from parts.
func stream(parts ...[]byte) []byte {
	var b []byte
	for _, p := range parts {
		b = append(b, p...)
	}
	return b
}

// gopSegment emits interleaved A/V media: a keyframe at start ts, then
// audio every 20ms and interframes every 40ms until end (exclusive).
func gopSegment(start, end int64) []byte {
	var b []byte
	b = append(b, vKey(start)...)
	for a := start; a < end; a += 20 {
		b = append(b, aRaw(a)...)
		if v := a + 20; v%40 == 0 && v > start && v < end {
			b = append(b, vInter(v)...)
		}
	}
	return b
}

// ---- output validation ----

type parsedTag struct {
	typ     byte
	ts      int64
	payload []byte
}

// parseOutput strictly parses a sanitized stream, failing the test on any
// structural violation, and returns the tags.
func parseOutput(t *testing.T, out []byte) []parsedTag {
	t.Helper()
	if len(out) == 0 {
		return nil
	}
	if !bytes.Equal(out[:13], fileHeader()) {
		t.Fatalf("output does not start with canonical FLV header: %x", out[:min(13, len(out))])
	}
	pos := 13
	var tags []parsedTag
	for pos < len(out) {
		if len(out)-pos < tagHeaderLen+prevSizeLen {
			t.Fatalf("trailing partial tag at %d (%d bytes left)", pos, len(out)-pos)
		}
		typ := out[pos]
		if typ != tagAudio && typ != tagVideo && typ != tagScript {
			t.Fatalf("invalid tag type 0x%02x at %d", typ, pos)
		}
		size := be24(out[pos+1 : pos+4])
		end := pos + tagHeaderLen + size
		if end+prevSizeLen > len(out) {
			t.Fatalf("tag at %d overruns output", pos)
		}
		if got := be32(out[end : end+prevSizeLen]); got != uint32(tagHeaderLen+size) {
			t.Fatalf("bad PreviousTagSize at %d: %d", end, got)
		}
		tags = append(tags, parsedTag{
			typ:     typ,
			ts:      tagTimestamp(out[pos : pos+tagHeaderLen]),
			payload: out[pos+tagHeaderLen : end],
		})
		pos = end + prevSizeLen
	}
	return tags
}

// checkInvariants asserts the sanitizer's output guarantees on parsed tags.
func checkInvariants(t *testing.T, tags []parsedTag) {
	t.Helper()
	last := map[byte]int64{}
	videoCfgSeen := false
	videoCfgCount := 0
	var videoCfgs [][]byte
	for i, tag := range tags {
		switch tag.typ {
		case tagVideo:
			vi := parseVideoInfo(tag.payload)
			if vi.config {
				videoCfgCount++
				videoCfgs = append(videoCfgs, videoConfigIdentity(tag.payload))
				videoCfgSeen = true
				continue
			}
			if !videoCfgSeen {
				t.Fatalf("tag %d: video media before video config", i)
			}
			if prev, ok := last[tagVideo]; ok && tag.ts <= prev {
				t.Fatalf("tag %d: video ts not increasing: %d then %d", i, prev, tag.ts)
			}
			last[tagVideo] = tag.ts
		case tagAudio:
			if isCfg, _ := audioConfigInfo(tag.payload); isCfg {
				continue
			}
			if prev, ok := last[tagAudio]; ok && tag.ts <= prev {
				t.Fatalf("tag %d: audio ts not increasing: %d then %d", i, prev, tag.ts)
			}
			last[tagAudio] = tag.ts
		}
	}
	for i := 1; i < len(videoCfgs); i++ {
		if !bytes.Equal(videoCfgs[0], videoCfgs[i]) {
			t.Fatalf("output contains %d distinct video configs", videoCfgCount)
		}
	}
}

func mediaTimestamps(tags []parsedTag, typ byte) []int64 {
	var ts []int64
	for _, tag := range tags {
		if tag.typ != typ {
			continue
		}
		if typ == tagVideo {
			if parseVideoInfo(tag.payload).config {
				continue
			}
		} else if typ == tagAudio {
			if isCfg, _ := audioConfigInfo(tag.payload); isCfg {
				continue
			}
		}
		ts = append(ts, tag.ts)
	}
	return ts
}

func run(t *testing.T, src []byte) (*bytes.Buffer, *Sanitizer, error) {
	t.Helper()
	var out bytes.Buffer
	s := New(&out, nil, Options{})
	// These tests assert exact output timestamps, so the output grid is off;
	// timeline_test.go covers the grid, including over these same fixtures.
	s.rawTimestamps = true
	_, err := s.Write(src)
	if err == nil {
		err = s.Finish()
	}
	return &out, s, err
}

// ---- tests ----

func TestCleanPassthrough(t *testing.T) {
	src := stream(fileHeader(), scriptTag(), vCfgTag(0, 0x41, 22), aCfgTag(0),
		gopSegment(0, 2000), gopSegment(2000, 4000))
	out, s, err := run(t, src)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	tags := parseOutput(t, out.Bytes())
	checkInvariants(t, tags)
	vts := mediaTimestamps(tags, tagVideo)
	if len(vts) == 0 || vts[0] != 0 {
		t.Fatalf("video should start at 0, got %v", vts[:min(3, len(vts))])
	}
	st := s.Stats()
	if st.desyncs != 0 || st.gapsGlued != 0 || st.resets != 0 || st.dedupWindows != 0 {
		t.Fatalf("clean stream triggered corrections: %+v", st)
	}
}

func TestChunkedWriteEquivalence(t *testing.T) {
	src := stream(fileHeader(), scriptTag(), vCfgTag(0, 0x41, 22), aCfgTag(0),
		gopSegment(0, 2000),
		fileHeader(), scriptTag(), vCfgTag(0, 0x41, 22), aCfgTag(0), // in-band restart
		gopSegment(1900, 3000), // small replay overlap
	)
	var ref []byte
	for _, chunk := range []int{len(src), 1, 7, 4096} {
		var out bytes.Buffer
		s := New(&out, nil, Options{})
		for i := 0; i < len(src); i += chunk {
			if _, err := s.Write(src[i:min(i+chunk, len(src))]); err != nil {
				t.Fatalf("chunk %d: %v", chunk, err)
			}
		}
		if ref == nil {
			ref = bytes.Clone(out.Bytes())
			continue
		}
		if !bytes.Equal(ref, out.Bytes()) {
			t.Fatalf("chunk size %d produced different output (%d vs %d bytes)",
				chunk, out.Len(), len(ref))
		}
	}
}

func TestInBandRestartAndReplayDedup(t *testing.T) {
	src := stream(fileHeader(), scriptTag(), vCfgTag(0, 0x41, 22), aCfgTag(0),
		gopSegment(0, 10000),
		fileHeader(), scriptTag(), vCfgTag(0, 0x41, 22), aCfgTag(0), // restart, same config
		gopSegment(8000, 8960), // replayed content
		gopSegment(8960, 9920),
		gopSegment(9920, 10920),  // passes high-water at ~9980 mid-GOP
		gopSegment(10920, 12000), // keyframe past high-water: video resumes
	)
	out, s, err := run(t, src)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	tags := parseOutput(t, out.Bytes())
	checkInvariants(t, tags)
	st := s.Stats()
	if st.restartsConsumed != 1 {
		t.Fatalf("RestartsConsumed = %d, want 1", st.restartsConsumed)
	}
	if st.dedupWindows == 0 || st.dedupTags == 0 {
		t.Fatalf("expected dedup activity, got %+v", st)
	}
	if st.scriptsDropped != 1 || st.dupConfigsDropped == 0 {
		t.Fatalf("expected script/config dedupe, got %+v", st)
	}
	// No content duplicated: strictly-increasing check in invariants covers
	// it, but also ensure we didn't lose the post-replay content.
	vts := mediaTimestamps(tags, tagVideo)
	if vts[len(vts)-1] < 11000 {
		t.Fatalf("post-replay content missing, last video ts %d", vts[len(vts)-1])
	}
}

func TestForwardGapGlued(t *testing.T) {
	src := stream(fileHeader(), vCfgTag(0, 0x41, 22), aCfgTag(0),
		gopSegment(0, 5000),
		gopSegment(65000, 67000), // 60s outage
	)
	out, s, err := run(t, src)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	tags := parseOutput(t, out.Bytes())
	checkInvariants(t, tags)
	st := s.Stats()
	if st.gapsGlued != 1 {
		t.Fatalf("GapsGlued = %d, want 1", st.gapsGlued)
	}
	ats := mediaTimestamps(tags, tagAudio)
	lastTs := ats[len(ats)-1]
	if lastTs > 8000 {
		t.Fatalf("gap not removed: final audio ts %d", lastTs)
	}
}

func TestGluedGapWaitsForKeyframe(t *testing.T) {
	// The uplink dies mid-GOP and the encoder resumes without a keyframe.
	// Splicing straight on would leave inter frames leaning on references the
	// gap swept away, which decodes as blocks of garbage; the glue must hold
	// video until the next keyframe while audio carries on.
	var resume []byte
	for ts := int64(9000); ts < 9400; ts += 20 {
		resume = append(resume, aRaw(ts)...)
		if v := ts + 20; v%40 == 0 {
			resume = append(resume, vInter(v)...)
		}
	}
	src := stream(fileHeader(), vCfgTag(0, 0x41, 22), aCfgTag(0),
		gopSegment(0, 2000),
		resume,                  // no keyframe: everything here must be dropped
		gopSegment(9400, 10000), // opens with one: video resumes here
	)
	var out bytes.Buffer
	s := New(&out, nil, Options{})
	if _, err := s.Write(src); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := s.Finish(); err != nil {
		t.Fatalf("finish: %v", err)
	}
	tags := parseOutput(t, out.Bytes())
	checkInvariants(t, tags)
	if s.Stats().gapsGlued != 1 {
		t.Fatalf("gapsGlued = %d, want 1", s.Stats().gapsGlued)
	}
	if s.Stats().keyframeWaitTags == 0 {
		t.Fatal("no video was held back: the glue spliced onto a broken GOP")
	}
	// No video may be emitted before a keyframe.
	seenKey := false
	for _, tag := range tags {
		if tag.typ != tagVideo {
			continue
		}
		vi := parseVideoInfo(tag.payload)
		if vi.config {
			continue
		}
		if !seenKey && !vi.keyframe {
			t.Fatal("video emitted before any keyframe")
		}
		if vi.keyframe {
			seenKey = true
		}
	}
	// Audio must not be held back with it.
	if n := len(mediaTimestamps(tags, tagAudio)); n < 100 {
		t.Fatalf("audio tags = %d, want the audio to carry through the gap", n)
	}
}

func TestTimelineReset(t *testing.T) {
	src := stream(fileHeader(), vCfgTag(0, 0x41, 22), aCfgTag(0),
		gopSegment(100000, 110000), // offset init → out 0..10000
		gopSegment(5000, 8000),     // 105s backward: reset, not replay
	)
	out, s, err := run(t, src)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	tags := parseOutput(t, out.Bytes())
	checkInvariants(t, tags)
	st := s.Stats()
	if st.resets != 1 {
		t.Fatalf("Resets = %d, want 1 (stats: %+v)", st.resets, st)
	}
	vts := mediaTimestamps(tags, tagVideo)
	last := vts[len(vts)-1]
	if last < 12000 || last > 14000 {
		t.Fatalf("reset should glue forward (~13s), last video ts %d", last)
	}
}

func TestReplayEscalatesToReset(t *testing.T) {
	src := stream(fileHeader(), vCfgTag(0, 0x41, 22), aCfgTag(0),
		gopSegment(0, 56000),     // out 0..56s
		gopSegment(1000, 50000),  // 55s back (≤60s ⇒ dedup) but never catches up
		gopSegment(50000, 58000), // eventually passes high-water; escalation should fire first
	)
	out, s, err := run(t, src)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	tags := parseOutput(t, out.Bytes())
	checkInvariants(t, tags)
	st := s.Stats()
	if st.dedupWindows != 1 || st.resets != 1 {
		t.Fatalf("expected dedup window escalating to reset, got %+v", st)
	}
}

func TestConfigChangeRotation(t *testing.T) {
	src := stream(fileHeader(), scriptTag(), vCfgTag(0, 0x41, 22), aCfgTag(0),
		gopSegment(0, 3000),
		vCfgTag(3000, 0x99, 40), // different resolution bits
		gopSegment(3000, 6000),
	)
	var out1 bytes.Buffer
	s := New(&out1, nil, Options{})
	_, err := s.Write(src)
	if !errors.Is(err, ErrConfigChange) {
		t.Fatalf("want ErrConfigChange, got %v", err)
	}
	// Live-path behavior: subsequent writes keep failing.
	if _, err := s.Write(vKey(6000)); !errors.Is(err, ErrConfigChange) {
		t.Fatalf("expected sticky ErrConfigChange, got %v", err)
	}

	// Offline path: rotate and continue losslessly.
	var out2 bytes.Buffer
	if err := s.RotateOutput(&out2); err != nil {
		t.Fatalf("RotateOutput: %v", err)
	}
	tags1 := parseOutput(t, out1.Bytes())
	checkInvariants(t, tags1)
	tags2 := parseOutput(t, out2.Bytes())
	checkInvariants(t, tags2)
	if s.Stats().configChanges != 1 {
		t.Fatalf("ConfigChanges = %d, want 1", s.Stats().configChanges)
	}
	// Second output must start its video with the NEW config and a keyframe,
	// carry the audio config over, and restart its timeline near zero.
	var sawNewCfg, sawAudioCfg bool
	for _, tag := range tags2 {
		if tag.typ == tagVideo && parseVideoInfo(tag.payload).config {
			if !bytes.Equal(videoConfigIdentity(tag.payload), videoConfigIdentity(avcSeqHdr(0x99, 40))) {
				t.Fatal("rotated output does not carry the new video config")
			}
			sawNewCfg = true
		}
		if tag.typ == tagAudio {
			if isCfg, _ := audioConfigInfo(tag.payload); isCfg {
				sawAudioCfg = true
			}
		}
	}
	if !sawNewCfg || !sawAudioCfg {
		t.Fatalf("rotated output missing configs (video=%v audio=%v)", sawNewCfg, sawAudioCfg)
	}
	vts := mediaTimestamps(tags2, tagVideo)
	if len(vts) == 0 || vts[0] > 100 {
		t.Fatalf("rotated part should restart timeline near 0, got %v", vts[:min(3, len(vts))])
	}
}

func TestConfigFlipFlopResumesPerConfigOutput(t *testing.T) {
	// A → B → A: the second A-run must append to the first A-output with a
	// continuous timeline instead of opening a third file.
	src := stream(fileHeader(), vCfgTag(0, 0x41, 22), aCfgTag(0),
		gopSegment(0, 3000),
		vCfgTag(3000, 0x99, 40),
		gopSegment(3000, 6000),
		vCfgTag(6000, 0x41, 22),
		gopSegment(6000, 9000),
	)
	var outA, outB bytes.Buffer
	s := New(&outA, nil, Options{})
	_, err := s.Write(src)
	if !errors.Is(err, ErrConfigChange) {
		t.Fatalf("want first ErrConfigChange, got %v", err)
	}
	keyA := s.ConfigKey()
	if keyA == "" || s.pendingConfigKey() == "" || s.pendingConfigKey() == keyA {
		t.Fatalf("bad keys: active=%q pending=%q", keyA, s.pendingConfigKey())
	}
	if s.hasResumeState() {
		t.Fatal("first switch to B must not have resume state")
	}
	if err := s.RotateOutput(&outB); err == nil {
		t.Fatal("expected second ErrConfigChange when flipping back to A")
	} else if !errors.Is(err, ErrConfigChange) {
		t.Fatalf("want ErrConfigChange, got %v", err)
	}
	if s.pendingConfigKey() != keyA {
		t.Fatalf("flip back should pend key A: %q vs %q", s.pendingConfigKey(), keyA)
	}
	if !s.hasResumeState() {
		t.Fatal("flip back to A must resume A's output")
	}
	if err := s.RotateOutput(&outA); err != nil { // append to A's buffer
		t.Fatalf("resume rotate: %v", err)
	}
	if err := s.Finish(); err != nil {
		t.Fatalf("finish: %v", err)
	}
	tagsA := parseOutput(t, outA.Bytes())
	checkInvariants(t, tagsA) // continuity across the splice: strictly increasing
	tagsB := parseOutput(t, outB.Bytes())
	checkInvariants(t, tagsB)
	vtsA := mediaTimestamps(tagsA, tagVideo)
	// A holds ~3s + ~3s of video; the resumed run must land right after the
	// first run (~3s boundary), not restart at 0 or jump to 6s.
	last := vtsA[len(vtsA)-1]
	if last < 5500 || last > 6500 {
		t.Fatalf("spliced A timeline should end ~6s, got %d", last)
	}
	if s.Stats().configChanges != 2 {
		t.Fatalf("ConfigChanges = %d, want 2", s.Stats().configChanges)
	}
}

func TestNoResumeAlwaysOpensAFreshOutput(t *testing.T) {
	// Same A → B → A flip-flop, but for a caller whose outputs are write-once:
	// the second A-run must open a file of its own, headered and configured
	// and starting at zero, not splice into the first A-output.
	src := stream(fileHeader(), vCfgTag(0, 0x41, 22), aCfgTag(0),
		gopSegment(0, 3000),
		vCfgTag(3000, 0x99, 40),
		gopSegment(3000, 6000),
		vCfgTag(6000, 0x41, 22),
		gopSegment(6000, 9000),
	)
	var outA1, outB, outA2 bytes.Buffer
	s := New(&outA1, nil, Options{NoResume: true})
	if _, err := s.Write(src); !errors.Is(err, ErrConfigChange) {
		t.Fatalf("want first ErrConfigChange, got %v", err)
	}
	if err := s.RotateOutput(&outB); !errors.Is(err, ErrConfigChange) {
		t.Fatalf("want second ErrConfigChange, got %v", err)
	}
	if s.hasResumeState() {
		t.Fatal("NoResume must never report resume state")
	}
	if err := s.RotateOutput(&outA2); err != nil {
		t.Fatalf("third rotate: %v", err)
	}
	if err := s.Finish(); err != nil {
		t.Fatalf("finish: %v", err)
	}
	if outA1.Len() == 0 || outB.Len() == 0 || outA2.Len() == 0 {
		t.Fatalf("every rotation needs its own output: %d/%d/%d",
			outA1.Len(), outB.Len(), outA2.Len())
	}
	for name, out := range map[string]*bytes.Buffer{"A1": &outA1, "B": &outB, "A2": &outA2} {
		tags := parseOutput(t, out.Bytes())
		checkInvariants(t, tags)
		vts := mediaTimestamps(tags, tagVideo)
		if len(vts) == 0 || vts[0] > 100 {
			t.Fatalf("output %s should start near 0, got %v", name, vts[:min(3, len(vts))])
		}
		var sawCfg bool
		for _, tag := range tags {
			if tag.typ == tagVideo && parseVideoInfo(tag.payload).config {
				sawCfg = true
			}
		}
		if !sawCfg {
			t.Fatalf("output %s carries no video config", name)
		}
	}
}

func TestLevelOnlyConfigChangeIsNotRotation(t *testing.T) {
	src := stream(fileHeader(), vCfgTag(0, 0x41, 22), aCfgTag(0),
		gopSegment(0, 2000),
		vCfgTag(2000, 0x41, 30), // same SPS content, level differs
		gopSegment(2000, 4000),
	)
	out, s, err := run(t, src)
	if err != nil {
		t.Fatalf("level-only change must not rotate: %v", err)
	}
	tags := parseOutput(t, out.Bytes())
	checkInvariants(t, tags)
	if s.Stats().configChanges != 0 || s.Stats().dupConfigsDropped == 0 {
		t.Fatalf("expected level-only dedupe, got %+v", s.Stats())
	}
}

func TestGarbageDesyncRecovery(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	garbage := make([]byte, 3000)
	rng.Read(garbage)
	// Ensure garbage can't accidentally contain a file header signature.
	garbage = bytes.ReplaceAll(garbage, []byte("FLV"), []byte("xLV"))
	src := stream(fileHeader(), vCfgTag(0, 0x41, 22), aCfgTag(0),
		gopSegment(0, 2000),
		garbage,
		gopSegment(2000, 4000),
	)
	out, s, err := run(t, src)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	tags := parseOutput(t, out.Bytes())
	checkInvariants(t, tags)
	st := s.Stats()
	if st.desyncs == 0 || st.desyncBytesDrop == 0 {
		t.Fatalf("expected desync recovery, got %+v", st)
	}
	vts := mediaTimestamps(tags, tagVideo)
	if vts[len(vts)-1] < 3500 {
		t.Fatalf("post-garbage content missing, last video ts %d", vts[len(vts)-1])
	}
}

func TestTruncatedTailNotWritten(t *testing.T) {
	full := vKey(2000)
	src := stream(fileHeader(), vCfgTag(0, 0x41, 22),
		vKey(0), vInter(40), vInter(80),
		full[:len(full)-6], // cut mid-tag
	)
	out, _, err := run(t, src)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	tags := parseOutput(t, out.Bytes()) // fails on any partial trailing bytes
	vts := mediaTimestamps(tags, tagVideo)
	if len(vts) != 3 {
		t.Fatalf("want 3 whole video tags, got %d", len(vts))
	}
}

func TestMediaBeforeConfigDropped(t *testing.T) {
	src := stream(fileHeader(),
		vInter(0), aRaw(0), // arrive before any sequence headers
		vCfgTag(0, 0x41, 22), aCfgTag(0),
		vInter(40), // still dropped: waiting for keyframe
		gopSegment(80, 1000),
	)
	out, s, err := run(t, src)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	tags := parseOutput(t, out.Bytes())
	checkInvariants(t, tags)
	st := s.Stats()
	if st.preConfigTags == 0 || st.keyframeWaitTags == 0 {
		t.Fatalf("expected pre-config and keyframe-wait drops, got %+v", st)
	}
}

func TestHeaderlessStreamGetsHeader(t *testing.T) {
	src := stream(vCfgTag(0, 0x41, 22), gopSegment(0, 500))
	out, _, err := run(t, src)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	tags := parseOutput(t, out.Bytes()) // asserts canonical header present
	checkInvariants(t, tags)
}

func TestSinkErrorSticky(t *testing.T) {
	s := New(failWriter{}, nil, Options{})
	_, err := s.Write(stream(fileHeader(), vCfgTag(0, 0x41, 22), vKey(0)))
	if !errors.Is(err, errSinkWrite) {
		t.Fatalf("want ErrSinkWrite, got %v", err)
	}
	if _, err := s.Write(vInter(40)); !errors.Is(err, errSinkWrite) {
		t.Fatalf("sink error must be sticky, got %v", err)
	}
}

type failWriter struct{}

func (failWriter) Write(p []byte) (int, error) { return 0, errors.New("disk full") }

// Opus travels in the enhanced-FLV audio layout: a SequenceStart carrying the
// identification header, then CodedFrames.
func opusSeqTag(ts int64) []byte {
	return buildTag(tagAudio, ts, append([]byte{0x90, 'O', 'p', 'u', 's'}, []byte("OpusHead\x01\x02\x38\x01\x80\xbb\x00\x00\x00\x00\x00")...))
}

func opusFrame(ts int64) []byte {
	return buildTag(tagAudio, ts, []byte{0x91, 'O', 'p', 'u', 's', 0xfc, 0xff})
}

func TestOpusSequenceStartIsAConfig(t *testing.T) {
	src := stream(fileHeader(), vCfgTag(0, 0x41, 22),
		opusFrame(0), // before its sequence start: dropped
		opusSeqTag(0), vKey(0), opusFrame(20),
		opusSeqTag(40), // identical re-send: dropped
		opusFrame(40), vInter(40))
	out, s, err := run(t, src)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	tags := parseOutput(t, out.Bytes())
	checkInvariants(t, tags)
	var seqStarts, frames int
	for _, tag := range tags {
		if tag.typ != tagAudio {
			continue
		}
		switch tag.payload[0] {
		case 0x90:
			seqStarts++
			if frames > 0 {
				t.Fatal("Opus sequence start emitted after a frame")
			}
		case 0x91:
			frames++
		}
	}
	if seqStarts != 1 || frames != 2 {
		t.Fatalf("got %d sequence starts and %d frames, want 1 and 2", seqStarts, frames)
	}
	if st := s.Stats(); st.preConfigTags != 1 || st.dupConfigsDropped != 1 {
		t.Fatalf("stats %+v: want one pre-config drop and one duplicate config", st)
	}
}

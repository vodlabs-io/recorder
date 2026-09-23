package encoding

import (
	"slices"
	"strings"
	"testing"
)

func TestPickBitrate(t *testing.T) {
	cases := []struct {
		h    int
		want int64
	}{
		{2160, 16_000_000}, {2400, 16_000_000},
		{1440, 10_000_000},
		{1080, 6_000_000}, {1200, 6_000_000},
		{720, 4_000_000}, {900, 4_000_000},
		{480, 2_000_000}, {0, 2_000_000},
	}
	for _, c := range cases {
		if got := pickBitrate(c.h); got != c.want {
			t.Errorf("pickBitrate(%d) = %d, want %d", c.h, got, c.want)
		}
	}
}

func TestPlanBitrate(t *testing.T) {
	// Unknown source rate: plain ladder, no cap.
	p := planBitrate(864, 0)
	if p.target != 4_000_000 || p.cap != 0 {
		t.Errorf("planBitrate(864, 0) = %+v, want target 4M, no cap", p)
	}

	// Low-rate source (600kbps TikTok): cap = 1.5×source, target capped.
	p = planBitrate(864, 600_000)
	if p.cap != 900_000 || p.target != 900_000 {
		t.Errorf("planBitrate(864, 600k) = %+v, want cap/target 900k", p)
	}

	// High-rate source: cap exceeds the ladder — ladder wins for the target.
	p = planBitrate(864, 8_000_000)
	if p.target != 4_000_000 || p.cap != 12_000_000 {
		t.Errorf("planBitrate(864, 8M) = %+v, want target 4M, cap 12M", p)
	}

	// Boundary: cap exactly at the ladder.
	p = planBitrate(1080, 4_000_000)
	if p.target != 6_000_000 || p.cap != 6_000_000 {
		t.Errorf("planBitrate(1080, 4M) = %+v, want target 6M, cap 6M", p)
	}
}

func TestPlanAudioBitrate(t *testing.T) {
	cases := []struct {
		srcMaxBps int64
		want      int64
	}{
		{0, 192_000},       // unknown source rate: default
		{-1, 192_000},      // defensive: bogus negative
		{53_546, 80_319},   // typical TikTok AAC: 1.5× source
		{10_000, 64_000},   // misreported/tiny source: floor
		{128_000, 192_000}, // 1.5× lands exactly on the default
		{500_000, 192_000}, // high-rate source: default is the ceiling
	}
	for _, c := range cases {
		if got := planAudioBitrate(c.srcMaxBps); got != c.want {
			t.Errorf("planAudioBitrate(%d) = %d, want %d", c.srcMaxBps, got, c.want)
		}
	}
}

func TestEncoderArgs(t *testing.T) {
	uncapped := planBitrate(1080, 0)    // target 6M, no cap
	capped := planBitrate(864, 600_000) // target/cap 900k

	// Software uses CRF, not a target bitrate; uncapped has no maxrate.
	sw := encoderArgs(softwareEncoder, uncapped)
	if !slices.Contains(sw, "-crf") || slices.Contains(sw, "-b:v") {
		t.Errorf("libx264 args = %v, want CRF and no -b:v", sw)
	}
	if slices.Contains(sw, "-maxrate") {
		t.Errorf("uncapped libx264 args = %v, want no -maxrate", sw)
	}
	if !slices.Contains(sw, "yuv420p") {
		t.Errorf("libx264 args missing yuv420p: %v", sw)
	}

	// Capped software: CRF plus a source-derived maxrate/bufsize ceiling.
	swCap := encoderArgs(softwareEncoder, capped)
	if !slices.Contains(swCap, "-maxrate") || !slices.Contains(swCap, "900000") {
		t.Errorf("capped libx264 args = %v, want -maxrate 900000", swCap)
	}
	if !slices.Contains(swCap, "-bufsize") || !slices.Contains(swCap, "1800000") {
		t.Errorf("capped libx264 args = %v, want -bufsize 1800000", swCap)
	}

	// qsv/mf want nv12 input.
	for _, enc := range []string{"h264_qsv", "h264_mf"} {
		a := encoderArgs(enc, uncapped)
		if !slices.Contains(a, "nv12") {
			t.Errorf("%s args = %v, want nv12", enc, a)
		}
		if !slices.Contains(a, "6000000") {
			t.Errorf("%s args = %v, want the target bitrate", enc, a)
		}
	}

	// Other hardware encoders use yuv420p + the (capped) target bitrate.
	for _, enc := range []string{"h264_videotoolbox", "h264_nvenc", "h264_amf"} {
		a := encoderArgs(enc, capped)
		if !slices.Contains(a, "yuv420p") || !slices.Contains(a, "900000") {
			t.Errorf("%s args = %v, want yuv420p + capped bitrate", enc, a)
		}
	}

	if encoderArgs("nonsense", uncapped) != nil {
		t.Errorf("unknown encoder should return nil args")
	}
}

func TestKnownEncoderIDs(t *testing.T) {
	ids := KnownEncoderIDs()
	if len(ids) == 0 {
		t.Fatal("no encoder ids")
	}
	// Software fallback is always present and last.
	if ids[len(ids)-1] != softwareEncoder {
		t.Errorf("last id = %q, want %q", ids[len(ids)-1], softwareEncoder)
	}
	if !slices.Contains(ids, softwareEncoder) {
		t.Errorf("software encoder missing from %v", ids)
	}
	// The hardware candidates for this platform are a prefix of the known set.
	for _, cand := range hwCandidates() {
		if !slices.Contains(ids, cand) {
			t.Errorf("candidate %q missing from known ids %v", cand, ids)
		}
		if strings.TrimSpace(encoderNames[cand]) == "" {
			t.Errorf("candidate %q has no friendly name", cand)
		}
		if cand == softwareEncoder {
			t.Errorf("software encoder should not be a hardware candidate")
		}
	}
	// No duplicates.
	seen := map[string]bool{}
	for _, id := range ids {
		if seen[id] {
			t.Errorf("duplicate id %q", id)
		}
		seen[id] = true
	}
}

func TestHwFallbackWarning(t *testing.T) {
	// No hardware candidate present is not a fallback.
	if got := hwFallbackWarning(nil, 1920, 1080); got != "" {
		t.Errorf("no failures should not warn, got %q", got)
	}

	got := hwFallbackWarning([]string{"Apple VideoToolbox"}, 284, 160)
	for _, want := range []string{"Apple VideoToolbox", "284x160", "software"} {
		if !strings.Contains(got, want) {
			t.Errorf("warning %q missing %q", got, want)
		}
	}

	got = hwFallbackWarning([]string{"NVIDIA NVENC", "Intel Quick Sync"}, 1920, 1080)
	if !strings.Contains(got, "NVIDIA NVENC and Intel Quick Sync") {
		t.Errorf("multiple failures should all be named: %q", got)
	}
}

func TestEncoderNameFallsBackToID(t *testing.T) {
	if got := encoderName("h264_videotoolbox"); got != "Apple VideoToolbox" {
		t.Errorf("encoderName = %q", got)
	}
	if got := encoderName("h264_madeup"); got != "h264_madeup" {
		t.Errorf("unknown id should pass through, got %q", got)
	}
}

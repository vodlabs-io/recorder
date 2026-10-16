package stream

import "testing"

// bigoStreams is what a Bigo check yields: the master URL is a plain media
// playlist, so there is one variant and no resolution.
var bigoStreams = List{
	{URL: "https://cdn.example/list_1_2_0.m3u8", QualityID: "live"},
}

// douyinStreams is what a Douyin check yields: named qualities, worst to best,
// none of them carrying a resolution.
var douyinStreams = List{
	{URL: "https://cdn.example/ld.m3u8", QualityID: "sd1"},
	{URL: "https://cdn.example/sd.m3u8", QualityID: "sd2"},
	{URL: "https://cdn.example/hd.m3u8", QualityID: "hd1"},
	{URL: "https://cdn.example/or4.m3u8", QualityID: "or4"},
}

// hlsStreams is the ordinary shape: a master playlist with resolutions, plus an
// audio-only rendition.
var hlsStreams = List{
	{URL: "https://cdn.example/audio.m3u8", QualityID: "audio_only"},
	{URL: "https://cdn.example/480.m3u8", QualityID: "480p", ResWidth: 854, ResHeight: 480},
	{URL: "https://cdn.example/720.m3u8", QualityID: "720p60", ResWidth: 1280, ResHeight: 720},
	{URL: "https://cdn.example/1080.m3u8", QualityID: "1080p", ResWidth: 1920, ResHeight: 1080},
}

func TestSelect(t *testing.T) {
	tests := []struct {
		name    string
		streams List
		variant string
		wantURL string // "" means nil
	}{
		// A resolution-less list must still resolve "worst".
		{"bigo worst", bigoStreams, "worst", "https://cdn.example/list_1_2_0.m3u8"},
		{"bigo best", bigoStreams, "best", "https://cdn.example/list_1_2_0.m3u8"},
		{"bigo audio", bigoStreams, "audio", ""},
		{"douyin worst", douyinStreams, "worst", "https://cdn.example/ld.m3u8"},
		{"douyin best", douyinStreams, "best", "https://cdn.example/or4.m3u8"},
		{"douyin by quality id", douyinStreams, "hd1", "https://cdn.example/hd.m3u8"},
		{"douyin audio", douyinStreams, "audio", ""},

		// Resolutions present: "worst" must skip the audio rendition.
		{"hls worst", hlsStreams, "worst", "https://cdn.example/480.m3u8"},
		{"hls best", hlsStreams, "best", "https://cdn.example/1080.m3u8"},
		{"hls audio", hlsStreams, "audio", "https://cdn.example/audio.m3u8"},
		{"hls exact quality", hlsStreams, "720p60", "https://cdn.example/720.m3u8"},
		{"hls quality prefix", hlsStreams, "720p", "https://cdn.example/720.m3u8"},
		{"hls resolution", hlsStreams, "1920x1080", "https://cdn.example/1080.m3u8"},
		{"hls unknown quality falls back to best", hlsStreams, "4k", "https://cdn.example/1080.m3u8"},
		{"empty variant is best", hlsStreams, "", "https://cdn.example/1080.m3u8"},

		{"empty list", List{}, "worst", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.streams.Select(tt.variant)
			if tt.wantURL == "" {
				if got != nil {
					t.Fatalf("Select(%q) = %+v, want nil", tt.variant, *got)
				}
				return
			}
			if got == nil {
				t.Fatalf("Select(%q) = nil, want %s", tt.variant, tt.wantURL)
			}
			if got.URL != tt.wantURL {
				t.Errorf("Select(%q) = %s, want %s", tt.variant, got.URL, tt.wantURL)
			}
		})
	}
}

// A resolution-less variant must never be matched by a pref that merely
// contains "0", such as "1080p".
func TestSelectIgnoresZeroResolutionInNumericMatch(t *testing.T) {
	streams := List{
		{URL: "https://cdn.example/unknown.m3u8", QualityID: "live"},
		{URL: "https://cdn.example/1080.m3u8", QualityID: "1080p", ResWidth: 1920, ResHeight: 1080},
	}
	got := streams.Select("1080")
	if got == nil || got.URL != "https://cdn.example/1080.m3u8" {
		t.Fatalf("Select(\"1080\") = %v, want the 1080p variant", got)
	}
}

func TestIsHLS(t *testing.T) {
	for _, tc := range []struct {
		s    Stream
		want bool
	}{
		{Stream{URL: "https://cdn.example/live/index.m3u8?t=1"}, true},
		{Stream{URL: "https://cdn.example/live/stream.flv"}, false},
		{Stream{URL: "https://cdn.example/live/manifest", Format: FormatHLS}, true},
		{Stream{URL: "https://cdn.example/live.m3u8.flv", Format: FormatFLV}, false},
		{Stream{URL: "nicolive://lv351585633"}, true},
		{Stream{URL: "fc2live://58117384"}, true},
	} {
		if got := tc.s.IsHLS(); got != tc.want {
			t.Errorf("IsHLS(%+v) = %v, want %v", tc.s, got, tc.want)
		}
	}
}

func TestIsVP8(t *testing.T) {
	for _, tc := range []struct {
		url  string
		want bool
	}{
		{"agora://669058195ead4efaa9509c9a400de392/x?mint=manyvids&room=1&codec=vp8", true},
		{"agora://appid/channel?token=t&uid=1", false},
		{"https://cdn.example/live/stream.flv?codec=vp8", false},
	} {
		if got := (Stream{URL: tc.url}).IsVP8(); got != tc.want {
			t.Errorf("IsVP8(%q) = %v, want %v", tc.url, got, tc.want)
		}
	}
}

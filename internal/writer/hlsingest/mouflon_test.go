package hlsingest

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	m3u8 "github.com/Eyevinn/hls-m3u8/m3u8"
)

var testMouflonKeys = map[string]string{"NTK9aqcLmNFMWrpQ": "tOcYOap4Ty1l9Jzb"}

// mouflonPlaylist is an Origin playlist as served, its encrypted fields taken
// from a live capture whose real segment names were observed alongside.
const mouflonPlaylist = `#EXTM3U
#EXT-X-VERSION:6
#EXT-X-MOUFLON:PSCH:v2:NTK9aqcLmNFMWrpQ
#EXT-X-TARGETDURATION:2
#EXT-X-INDEPENDENT-SEGMENTS
#EXT-X-MEDIA-SEQUENCE:713
#EXT-X-DISCONTINUITY-SEQUENCE:0
#EXT-X-MAP:URI="https://media-hls.doppiocdn.net/b-hls-35/47298336/47298336_init_Di8S9p8OFXHM6v8X.mp4"
#EXT-X-PROGRAM-DATE-TIME:2026-10-11T19:41:04.433+0000
#EXTINF:2.021
#EXT-X-MOUFLON:URI:https://media-hls.doppiocdn.net/b-hls-35/47298336/47298336_713_AAZEqPhcaHbWINiNA+gqUL_1791747664.mp4
https://media-hls.doppiocdn.net/b-hls-35/media.mp4
#EXT-X-PROGRAM-DATE-TIME:2026-10-11T19:41:06.433+0000
#EXTINF:1.987
#EXT-X-MOUFLON:URI:https://media-hls.doppiocdn.net/b-hls-35/47298336/47298336_714_wMdA4X2tfEkeZYdNS+/WVQ_1791747666.mp4
https://media-hls.doppiocdn.net/b-hls-35/media.mp4
#EXT-X-PROGRAM-DATE-TIME:2026-10-11T19:41:08.433+0000
#EXTINF:2.000
#EXT-X-MOUFLON:URI:https://media-hls.doppiocdn.net/b-hls-35/47298336/47298336_715_AMpYbIyZaFtDYccJxwg+kK_1791747668.mp4
https://media-hls.doppiocdn.net/b-hls-35/media.mp4
`

func TestRestoreMouflon(t *testing.T) {
	out, err := restoreMouflon([]byte(mouflonPlaylist), testMouflonKeys)
	if err != nil {
		t.Fatal(err)
	}
	pl, _, err := m3u8.Decode(*bytes.NewBuffer(out), false)
	if err != nil {
		t.Fatalf("restored playlist does not parse: %v", err)
	}
	media := pl.(*m3u8.MediaPlaylist)
	want := []string{
		"https://media-hls.doppiocdn.net/b-hls-35/47298336/47298336_713_5rgwWT2cEBo0YRwZ_1791747664.mp4",
		"https://media-hls.doppiocdn.net/b-hls-35/47298336/47298336_714_YmxvwkgqzN3g8ssi_1791747666.mp4",
		"https://media-hls.doppiocdn.net/b-hls-35/47298336/47298336_715_2wgLFjwf3JncFEGj_1791747668.mp4",
	}
	var got []string
	for _, seg := range media.Segments {
		if seg != nil && seg.URI != "" {
			got = append(got, seg.URI)
		}
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("segments:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestRestoreMouflonParts(t *testing.T) {
	in := `#EXTM3U
#EXT-X-MOUFLON:PSCH:v2:NTK9aqcLmNFMWrpQ
#EXT-X-MOUFLON:URI:https://h/b/1_713_AAZEqPhcaHbWINiNA+gqUL_1791747664_part0.mp4
#EXT-X-PART:DURATION=0.528,URI="https://h/b/media.mp4",INDEPENDENT=YES
`
	out, err := restoreMouflon([]byte(in), testMouflonKeys)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `URI="https://h/b/1_713_5rgwWT2cEBo0YRwZ_1791747664_part0.mp4",INDEPENDENT=YES`) {
		t.Errorf("part not restored:\n%s", out)
	}
}

func TestRestoreMouflonUnprotected(t *testing.T) {
	in := []byte("#EXTM3U\n#EXTINF:2.0\nseg1.ts\n")
	out, err := restoreMouflon(in, nil)
	if err != nil || !bytes.Equal(out, in) {
		t.Errorf("unprotected playlist changed: %q, %v", out, err)
	}
}

func TestRestoreMouflonWrongKey(t *testing.T) {
	if _, err := restoreMouflon([]byte(mouflonPlaylist), nil); !errors.Is(err, errMouflonKey) {
		t.Errorf("no keys: err = %v", err)
	}
	other := map[string]string{"SomeOtherKeyId00": "tOcYOap4Ty1l9Jzb"}
	if _, err := restoreMouflon([]byte(mouflonPlaylist), other); !errors.Is(err, errMouflonKey) {
		t.Errorf("unnamed key: err = %v", err)
	}
	stale := map[string]string{"NTK9aqcLmNFMWrpQ": "aDifferentKey000"}
	if _, err := restoreMouflon([]byte(mouflonPlaylist), stale); !errors.Is(err, errMouflonKey) {
		t.Errorf("stale key: err = %v", err)
	}
}

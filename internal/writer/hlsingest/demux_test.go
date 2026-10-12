package hlsingest

import (
	"encoding/binary"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Synthetic fMP4: just the boxes the join reads and rewrites.

func fullBox(typ string, version byte, payload ...[]byte) []byte {
	b := []byte{version, 0, 0, 0}
	for _, p := range payload {
		b = append(b, p...)
	}
	return boxBytes(typ, b)
}

func u32(v uint32) []byte { return binary.BigEndian.AppendUint32(nil, v) }
func u64(v uint64) []byte { return binary.BigEndian.AppendUint64(nil, v) }
func concat(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// testInit builds an init segment with one track of the given handler.
func testInit(id, timescale uint32, handler string) []byte {
	mvhd := fullBox("mvhd", 0, make([]byte, 92), u32(id+1)) // ...next_track_ID last
	tkhd := fullBox("tkhd", 0, u32(0), u32(0), u32(id), make([]byte, 68))
	mdhd := fullBox("mdhd", 0, u32(0), u32(0), u32(timescale), u32(0), make([]byte, 4))
	hdlr := fullBox("hdlr", 0, u32(0), []byte(handler), make([]byte, 13))
	trak := boxBytes("trak", concat(tkhd, boxBytes("mdia", concat(mdhd, hdlr))))
	trex := fullBox("trex", 0, u32(id), u32(1), u32(0), u32(0), u32(0))
	moov := boxBytes("moov", concat(mvhd, trak, boxBytes("mvex", trex)))
	return concat(boxBytes("ftyp", []byte("iso6\x00\x00\x00\x00")), moov)
}

// testFragment builds a segment of one fragment for track id starting at
// decode time t, whose mdat carries tag.
func testFragment(id uint32, t uint64, tag string) []byte {
	tfhd := fullBox("tfhd", 0, u32(id))
	tfdt := fullBox("tfdt", 1, u64(t))
	moof := boxBytes("moof", concat(fullBox("mfhd", 0, u32(1)), boxBytes("traf", concat(tfhd, tfdt))))
	return concat(moof, boxBytes("mdat", []byte(tag)))
}

func TestMergeInitsDeclaresBothTracks(t *testing.T) {
	merged, audioID, err := mergeInits(testInit(1, 90000, "vide"), testInit(1, 48000, "soun"))
	if err != nil {
		t.Fatal(err)
	}
	if audioID != 2 {
		t.Fatalf("audio track id = %d, want 2", audioID)
	}
	var ids []uint32
	for _, b := range findBoxes(merged, "moov", "trak", "tkhd") {
		id, _ := trackID(merged, b)
		ids = append(ids, id)
	}
	if fmt.Sprint(ids) != "[1 2]" {
		t.Fatalf("tkhd ids = %v, want [1 2]", ids)
	}
	var trex []uint32
	for _, b := range findBoxes(merged, "moov", "mvex", "trex") {
		trex = append(trex, binary.BigEndian.Uint32(merged[b.payload()+4:]))
	}
	if fmt.Sprint(trex) != "[1 2]" {
		t.Fatalf("trex ids = %v, want [1 2]", trex)
	}
	mvhd := findBoxes(merged, "moov", "mvhd")[0]
	if next := binary.BigEndian.Uint32(merged[mvhd.end-4:]); next != 3 {
		t.Fatalf("next_track_ID = %d, want 3", next)
	}
	if !initHasSound(merged) {
		t.Fatal("merged init declares no sound track")
	}
}

func TestRetrackFragmentsKeepsSize(t *testing.T) {
	seg := concat(testFragment(1, 100, "a"), testFragment(1, 200, "b"))
	out, err := retrackFragments(seg, 7)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != len(seg) {
		t.Fatalf("size changed: %d -> %d", len(seg), len(out))
	}
	for _, b := range findBoxes(out, "moof", "traf", "tfhd") {
		if id := binary.BigEndian.Uint32(out[b.payload()+4:]); id != 7 {
			t.Fatalf("tfhd track id = %d, want 7", id)
		}
	}
	if tfhd := findBoxes(seg, "moof", "traf", "tfhd")[0]; binary.BigEndian.Uint32(seg[tfhd.payload()+4:]) != 1 {
		t.Fatal("input was rewritten in place")
	}
}

// joinCapture records the init bodies too.
type joinCapture struct {
	capture
	initBodies [][]byte
}

func (c *joinCapture) Init(m Map, body []byte) error {
	c.mu.Lock()
	c.initBodies = append(c.initBodies, body)
	c.mu.Unlock()
	return c.capture.Init(m, body)
}

// demuxOrigin serves a master pairing one video variant with an audio
// rendition, and closed media playlists of n 2-second segments for each. The
// audio rendition starts half a segment early.
func demuxOrigin(t *testing.T, videoHandler string, n int) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/master.m3u8", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "#EXTM3U\n#EXT-X-VERSION:6\n")
		fmt.Fprint(w, `#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="aac",NAME="a",DEFAULT=YES,AUTOSELECT=YES,URI="/audio.m3u8?session=s"`+"\n")
		fmt.Fprint(w, `#EXT-X-STREAM-INF:BANDWIDTH=1000000,RESOLUTION=1280x720,AUDIO="aac"`+"\n/video.m3u8?session=s\n")
	})
	playlist := func(kind string, n int) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("session") != "s" {
				http.Error(w, "missing session", http.StatusForbidden)
				return
			}
			var b strings.Builder
			fmt.Fprintf(&b, "#EXTM3U\n#EXT-X-VERSION:6\n#EXT-X-TARGETDURATION:2\n#EXT-X-MEDIA-SEQUENCE:0\n")
			fmt.Fprintf(&b, "#EXT-X-MAP:URI=\"/%s_init.mp4\"\n", kind)
			for i := 0; i < n; i++ {
				fmt.Fprintf(&b, "#EXTINF:2.0,\n/%s_%d.m4s\n", kind, i)
			}
			b.WriteString("#EXT-X-ENDLIST\n")
			fmt.Fprint(w, b.String())
		}
	}
	mux.HandleFunc("/video.m3u8", playlist("v", n))
	mux.HandleFunc("/audio.m3u8", playlist("a", n+1))
	mux.HandleFunc("/v_init.mp4", func(w http.ResponseWriter, r *http.Request) {
		w.Write(testInit(1, 90000, videoHandler))
	})
	mux.HandleFunc("/a_init.mp4", func(w http.ResponseWriter, r *http.Request) {
		w.Write(testInit(1, 48000, "soun"))
	})
	for i := 0; i <= n; i++ {
		mux.HandleFunc(fmt.Sprintf("/v_%d.m4s", i), func(w http.ResponseWriter, r *http.Request) {
			// Video segment i spans [10+2i, 12+2i) seconds.
			w.Write(testFragment(1, uint64(10+2*i)*90000, fmt.Sprintf("v%d", i)))
		})
		mux.HandleFunc(fmt.Sprintf("/a_%d.m4s", i), func(w http.ResponseWriter, r *http.Request) {
			// Audio segment i spans [9+2i, 11+2i) seconds.
			w.Write(testFragment(1, uint64(9+2*i)*48000, fmt.Sprintf("a%d", i)))
		})
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// mdatTags lists the mdat payloads of a joined segment with the track each
// fragment is addressed to, in order.
func mdatTags(seg []byte) []string {
	top, _ := readBoxes(seg, 0, len(seg))
	var out []string
	var id uint32
	for _, b := range top {
		switch b.typ {
		case "moof":
			sub := seg[b.start:b.end]
			tfhd := findBoxes(sub, "moof", "traf", "tfhd")[0]
			id = binary.BigEndian.Uint32(sub[tfhd.payload()+4:])
		case "mdat":
			out = append(out, fmt.Sprintf("%d:%s", id, seg[b.payload():b.end]))
		}
	}
	return out
}

func TestDemuxedRenditionsAreJoined(t *testing.T) {
	srv := demuxOrigin(t, "vide", 3)
	sink := &joinCapture{}
	err := Run(t.Context(), Config{URL: srv.URL + "/master.m3u8", Retries: 1}, srv.Client(),
		slog.New(slog.DiscardHandler), sink)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(sink.initBodies) != 1 {
		t.Fatalf("inits = %d, want 1", len(sink.initBodies))
	}
	if n := len(findBoxes(sink.initBodies[0], "moov", "trak")); n != 2 {
		t.Fatalf("joined init has %d tracks, want 2", n)
	}

	var got []string
	for _, s := range sink.bodies() {
		got = append(got, strings.Join(mdatTags([]byte(s)), " "))
	}
	// An audio segment goes with the video segment it starts in; a0 [9,11)
	// starts before v0 [10,12) but overlaps it, so it is kept with v0.
	want := []string{
		"1:v0 2:a0 2:a1",
		"1:v1 2:a2",
		"1:v2 2:a3",
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("joined segments:\n got %q\nwant %q", got, want)
	}
}

func TestDemuxedVideoWithItsOwnSoundIsPassedThrough(t *testing.T) {
	srv := demuxOrigin(t, "soun", 2)
	sink := &joinCapture{}
	err := Run(t.Context(), Config{URL: srv.URL + "/master.m3u8", Retries: 1}, srv.Client(),
		slog.New(slog.DiscardHandler), sink)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if n := len(findBoxes(sink.initBodies[0], "moov", "trak")); n != 1 {
		t.Fatalf("init has %d tracks, want the video's own 1", n)
	}
	for i, s := range sink.bodies() {
		if tags := strings.Join(mdatTags([]byte(s)), " "); tags != fmt.Sprintf("1:v%d", i) {
			t.Fatalf("segment %d = %q, want the video alone", i, tags)
		}
	}
}

package webui

import (
	"testing"
)

func TestSheetRefRewrite(t *testing.T) {
	for _, cue := range []string{"thumbs.012.jpg", "vod1.thumbs.012.jpg"} {
		in := []byte("WEBVTT\n\n00:00:00.000 --> 00:00:01.000\n" + cue + "#xywh=256,0,256,144\n")
		got := sheetRef.ReplaceAllFunc(in, func(m []byte) []byte {
			sub := sheetRef.FindSubmatch(m)
			return append([]byte("preview?n="+string(sub[1])), sub[2]...)
		})
		want := "WEBVTT\n\n00:00:00.000 --> 00:00:01.000\npreview?n=012#xywh=256,0,256,144\n"
		if string(got) != want {
			t.Fatalf("%s: got %q", cue, got)
		}
	}
}

func TestSheetNameMatchesBothLayouts(t *testing.T) {
	for _, name := range []string{"thumbs.003.jpg", "vod1.thumbs.003.jpg"} {
		if m := sheetName.FindStringSubmatch(name); m == nil || m[1] != "003" {
			t.Fatalf("%s: got %q", name, m)
		}
	}
	if sheetName.MatchString("vod1.thumbs.vtt") || sheetName.MatchString("mythumbs.003.jpg") {
		t.Fatal("matched a name that is no sheet")
	}
}

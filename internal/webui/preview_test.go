package webui

import (
	"testing"
)

func TestSheetRefRewrite(t *testing.T) {
	in := []byte("WEBVTT\n\n00:00:00.000 --> 00:00:01.000\nvod1.thumbs.012.jpg#xywh=256,0,256,144\n")
	got := sheetRef.ReplaceAllFunc(in, func(m []byte) []byte {
		sub := sheetRef.FindSubmatch(m)
		return append([]byte("preview?n="+string(sub[1])), sub[2]...)
	})
	want := "WEBVTT\n\n00:00:00.000 --> 00:00:01.000\npreview?n=012#xywh=256,0,256,144\n"
	if string(got) != want {
		t.Fatalf("got %q", got)
	}
}

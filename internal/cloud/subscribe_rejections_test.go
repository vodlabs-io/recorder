package cloud

import (
	"slices"
	"testing"
)

// TestDefinitiveRejections pins which rejections may be acted on by deleting
// config. Getting this wrong in the lenient direction costs a stale entry;
// getting it wrong the other way silently deletes streamers someone is
// recording the next time the resolver has a bad minute.
func TestDefinitiveRejections(t *testing.T) {
	got := definitiveRejections([]subscribeRejection{
		{URL: "https://vimeo.com/12345", Error: codeUnsupportedSite},
		{URL: "https://www.twitch.tv/videos/123", Error: codeNotALiveURL},
		{URL: "https://www.youtube.com/watch?v=abc", Error: codeUnavailable},
		{URL: "https://kick.com/nobody", Error: codeNotFound},
	})

	want := []string{
		"https://vimeo.com/12345",
		"https://www.twitch.tv/videos/123",
		"https://kick.com/nobody",
	}
	if !slices.Equal(got, want) {
		t.Errorf("definitiveRejections = %v, want %v", got, want)
	}
}

// TestDefinitiveRejections_KeepsSoftCodes covers the codes that say nothing
// about the streamer, including one this build doesn't know: deleting over a
// code that may be temporary would lose an entry the user has no copy of.
func TestDefinitiveRejections_KeepsSoftCodes(t *testing.T) {
	got := definitiveRejections([]subscribeRejection{
		{URL: "https://example.com/a", Error: codeOverCapacity},
		{URL: "https://example.com/b", Error: codeDailyCapReached},
		{URL: "https://example.com/c", Error: "some_future_code"},
	})
	if len(got) != 0 {
		t.Errorf("definitiveRejections = %v, want nothing", got)
	}
}

// TestDefinitiveRejections_NoURL guards the caller: DropURLs matches by exact
// URL string, so a rejection with no URL matches nothing and must not be
// offered as one.
func TestDefinitiveRejections_NoURL(t *testing.T) {
	if got := definitiveRejections([]subscribeRejection{
		{Platform: "twitch", Error: codeNotFound},
	}); len(got) != 0 {
		t.Errorf("definitiveRejections = %v, want nothing", got)
	}
	if got := definitiveRejections(nil); len(got) != 0 {
		t.Errorf("definitiveRejections(nil) = %v, want nothing", got)
	}
}

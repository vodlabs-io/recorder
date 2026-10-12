package nicolive

import (
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"testing"
)

func TestParse(t *testing.T) {
	if lv, err := Parse("nicolive://lv351585633"); err != nil || lv != "lv351585633" {
		t.Errorf("Parse = %q, %v", lv, err)
	}
	for _, bad := range []string{"https://live.nicovideo.jp/watch/lv1", "nicolive://ch123", "nicolive://lv", "nicolive://lv0"} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("Parse(%q) accepted", bad)
		}
	}
	if !IsURL("nicolive://lv1") || IsURL("https://x.m3u8") {
		t.Error("IsURL")
	}
}

// TestSeatCookiesKeepTheirPaths pins that each grant cookie reaches only the
// paths it was granted for: CloudFront sends one policy per path, all under
// the same names, and refuses a request carrying the wrong one.
func TestSeatCookiesKeepTheirPaths(t *testing.T) {
	g := streamGrant{
		URI: "https://livedelivery.dlive.nicovideo.jp/hls/playlists/p/s/multivariant/variant.m3u8",
		Cookies: []grantCookie{
			{Name: "CloudFront-Policy", Value: "playlists", Domain: "nicovideo.jp", Path: "/hls/playlists/p/s", Secure: true},
			{Name: "CloudFront-Policy", Value: "video", Domain: "nicovideo.jp", Path: "/hls/segments/p/video", Secure: true},
			{Name: "session", Value: "k", Domain: "nicovideo.jp", Path: "/hls/keys/p", Expires: "Tue, 13 Oct 2099 19:17:26 GMT", Secure: true},
			{Name: "elsewhere", Value: "x", Domain: "evil.test", Path: "/"},
		},
	}
	s := &Seat{MasterURL: g.URI, cookies: g.httpCookies()}
	jar := s.Client(nil).Jar.(*cookiejar.Jar)
	for path, want := range map[string]string{
		"/hls/playlists/p/s/media/v.m3u8":       "CloudFront-Policy=playlists",
		"/hls/segments/p/video/1/main/343.cmfv": "CloudFront-Policy=video",
		"/hls/keys/p/s/abc.key":                 "session=k",
		"/hls/segments/p/audio/1/main/343.cmfa": "",
	} {
		u, _ := url.Parse("https://livedelivery.dlive.nicovideo.jp" + path)
		got := ""
		for _, c := range jar.Cookies(u) {
			got = (&http.Cookie{Name: c.Name, Value: c.Value}).String()
		}
		if len(jar.Cookies(u)) > 1 || got != want {
			t.Errorf("%s: cookies %v, want %q", path, jar.Cookies(u), want)
		}
	}
}

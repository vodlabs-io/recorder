package platformauth

import "testing"

func TestParseNetscapeCookies(t *testing.T) {
	raw := "# Netscape HTTP Cookie File\n" +
		"# This is a generated file!  Do not edit.\n" +
		"\n" +
		".tiktok.com\tTRUE\t/\tTRUE\t1799999999\tsessionid\tabc123\n" +
		"#HttpOnly_.tiktok.com\tTRUE\t/\tTRUE\t1799999999\ttt_token\tsecret\n" +
		"www.example.com\tFALSE\t/\tFALSE\t0\tother\tvalue\n" +
		"malformed line without tabs\n" +
		".tiktok.com\tTRUE\t/\tTRUE\t1799999999\tsessionid\tduplicate\n"

	got := ParseNetscapeCookies(raw)
	if len(got) != 3 {
		t.Fatalf("got %d cookies, want 3: %+v", len(got), got)
	}

	if got[0].Name != "sessionid" || got[0].Value != "abc123" {
		t.Errorf("first cookie = %+v", got[0])
	}
	// The leading dot is stripped so the value matches a host suffix check.
	if got[0].Domain != "tiktok.com" {
		t.Errorf("domain = %q, want tiktok.com", got[0].Domain)
	}
	if !got[0].Secure || got[0].Expires != 1799999999 {
		t.Errorf("flags not parsed: %+v", got[0])
	}
	if !got[1].HTTPOnly || got[1].Name != "tt_token" {
		t.Errorf("#HttpOnly_ entry not parsed: %+v", got[1])
	}
	if got[2].Domain != "www.example.com" || got[2].Secure {
		t.Errorf("third cookie = %+v", got[2])
	}
}

func TestParseNetscapeCookies_Empty(t *testing.T) {
	for _, raw := range []string{"", "# only a comment\n", "\n\n"} {
		if got := ParseNetscapeCookies(raw); len(got) != 0 {
			t.Errorf("ParseNetscapeCookies(%q) = %+v, want none", raw, got)
		}
	}
}

func TestCookieHeader(t *testing.T) {
	in := []Cookie{
		{Name: "a", Value: "1"},
		{Name: "", Value: "skipped"},
		{Name: "b", Value: "2"},
	}
	if got, want := CookieHeader(in), "a=1; b=2"; got != want {
		t.Errorf("CookieHeader = %q, want %q", got, want)
	}
	if got := CookieHeader(nil); got != "" {
		t.Errorf("CookieHeader(nil) = %q, want empty", got)
	}
}

// A header rendered from a parsed export must round-trip through the paste
// parser, since that is the shape the daemon endpoint receives.
func TestNetscapeRoundTripsThroughCookieHeader(t *testing.T) {
	raw := ".tiktok.com\tTRUE\t/\tTRUE\t0\tsessionid\tabc\n" +
		".tiktok.com\tTRUE\t/\tTRUE\t0\tuid\t42\n"
	header := CookieHeader(ParseNetscapeCookies(raw))
	got := ParseCookieHeader(header)
	if len(got) != 2 || got[0].Name != "sessionid" || got[1].Value != "42" {
		t.Fatalf("round trip produced %+v", got)
	}
}
